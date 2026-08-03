package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	measyai "github.com/measyai/Go-SDK"
)

// The -jsonl protocol: one JSON object per line each way. It exists so a GUI
// can drive the same agent loop the terminal uses, rather than a second
// implementation drifting alongside it.
//
// Out: kind = ready | auth_required | auth_prompt | user | turn_start |
//              delta | tool_start | tool_body | tool_result |
//              approval_request | turn_end | notice | error | bye
// In:  kind = prompt | approval | model | auto | reset | login | logout
//
// Sign-in is part of the protocol rather than a separate handshake because
// the desktop app has the same problem the terminal does: on first run there
// is no key, and the app cannot invent one. It gets `auth_required`, sends
// `login`, renders the `auth_prompt` (a URL and a short code) in its own
// chrome, and waits for `ready` — the same device flow, drawn differently.

type event struct {
	Kind    string   `json:"kind"`
	Text    string   `json:"text,omitempty"`
	Name    string   `json:"name,omitempty"`
	Arg     string   `json:"arg,omitempty"`
	Summary string   `json:"summary,omitempty"`
	OK      bool     `json:"ok,omitempty"`
	ID      int      `json:"id,omitempty"`
	Model   string   `json:"model,omitempty"`
	Models  []string `json:"models,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Auto    bool     `json:"auto"`
	Tokens  int      `json:"tokens,omitempty"`
	Millis  int64    `json:"millis,omitempty"`
	// Sign-in. URL and Code carry the device flow's two halves; Account and
	// Project ride along on `ready` so the app can show who is signed in
	// without asking.
	URL     string `json:"url,omitempty"`
	Code    string `json:"code,omitempty"`
	Account string `json:"account,omitempty"`
	Project string `json:"project,omitempty"`
}

type command struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	ID     int    `json:"id"`
	Allow  bool   `json:"allow"`
	Always bool   `json:"always"`
}

var (
	// jsonOut is nil in terminal mode, which is what every render function
	// checks to decide whether to draw or to emit.
	jsonOut *json.Encoder
	jsonMu  sync.Mutex

	commands   chan command // decoded stdin, read by the loop and by approve
	queued     []string     // prompts that arrived mid-turn
	approvalID int
)

func emit(e event) {
	if jsonOut == nil {
		return
	}
	jsonMu.Lock()
	defer jsonMu.Unlock()
	jsonOut.Encode(e)
}

// startJSONL switches output to the protocol and starts decoding stdin.
func startJSONL() {
	jsonOut = json.NewEncoder(os.Stdout)
	useColor = false // no escapes, no spinner: the GUI draws its own

	commands = make(chan command, 16)
	go func() {
		defer close(commands)
		in := bufio.NewScanner(os.Stdin)
		in.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for in.Scan() {
			// The BOM a Windows pipe can prepend is not valid JSON, and a
			// dropped first line looks exactly like a hung agent.
			line := bytes.TrimSpace(bytes.TrimPrefix(in.Bytes(), []byte("\xef\xbb\xbf")))
			if len(line) == 0 {
				continue
			}
			var c command
			if err := json.Unmarshal(line, &c); err != nil {
				emit(event{Kind: "error", Text: "bad command: " + err.Error()})
				continue // a malformed line is not worth killing the session
			}
			commands <- c
		}
	}()
}

// runJSONL is the agent loop the desktop app drives. It is the same turn()
// the terminal uses; only the input and the rendering differ.
//
// creds may be nil — the app is expected to handle `auth_required` and send
// `login` — so every path that needs the client checks first.
func runJSONL(creds *credentials, model, cwd string, history *[]measyai.Message) {
	var client *measyai.Client
	if creds != nil {
		client = newClient(creds)
	}
	emit(readyEvent(client, creds, model, cwd))

	for {
		c, ok := nextCommand()
		if !ok {
			return // stdin closed
		}

		switch c.Kind {
		case "prompt":
			if c.Text == "" {
				continue
			}
			if client == nil {
				emit(event{Kind: "auth_required", Text: "Sign in to continue."})
				continue
			}
			emit(event{Kind: "user", Text: c.Text})
			*history = append(*history, measyai.Message{Role: "user", Content: c.Text})

			emit(event{Kind: "turn_start"})
			if err := turn(client, model, history); err != nil {
				emit(event{Kind: "error", Text: err.Error()})
			}
			emit(event{Kind: "turn_end"})

		case "login":
			// Blocking on purpose: the flow owns the session until it
			// resolves, and there is nothing useful to do meanwhile. The
			// app's cancel is closing stdin.
			signed, err := browserLogin(context.Background(), func(s startResponse) {
				emit(event{Kind: "auth_prompt", URL: s.VerificationURI, Code: s.UserCode})
				openBrowser(s.VerificationURIComplete)
			})
			if err != nil {
				emit(event{Kind: "error", Text: err.Error()})
				emit(event{Kind: "auth_required"})
				continue
			}
			creds, client = signed, newClient(signed)
			emit(readyEvent(client, creds, model, cwd))

		case "logout":
			if err := clearCredentials(); err != nil {
				emit(event{Kind: "error", Text: err.Error()})
				continue
			}
			creds, client = nil, nil
			emit(event{Kind: "auth_required", Text: "Signed out."})

		case "model":
			if client == nil {
				emit(event{Kind: "auth_required"})
				continue
			}
			if id, ok := resolveModel(client, c.Text); ok {
				model = id
			}
			emit(readyEvent(client, creds, model, cwd))

		case "auto":
			// The desktop app has no three-way picker yet, so its toggle
			// maps to the same two ends /auto moves between.
			if c.Allow {
				mode = modeDeveloper
			} else {
				mode = modeBalanced
			}
			notice("approval mode " + mode.String())

		case "think":
			showThinking = c.Allow
			notice("thinking " + onOff(showThinking))

		case "reset":
			*history = (*history)[:1]
			notice("context cleared")
		}
	}
}

// readyEvent announces a usable session. Without a credential there is no
// catalogue to list and no session to be ready for, so it announces the sign
// -in requirement instead — the app's cue to render its login screen.
func readyEvent(client *measyai.Client, creds *credentials, model, cwd string) event {
	if client == nil {
		return event{Kind: "auth_required", Dir: cwd, Auto: developerMode()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var ids []string
	if list, err := client.Models.List(ctx); err == nil {
		for _, m := range list {
			ids = append(ids, m.ID)
		}
	}
	return event{
		Kind: "ready", Model: model, Models: ids, Dir: cwd, Auto: developerMode(),
		Account: creds.label(), Project: creds.ProjectName,
	}
}

// resolveModel accepts a full id or a bare name like "cadence".
func resolveModel(client *measyai.Client, want string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	list, err := client.Models.List(ctx)
	if err != nil {
		return "", false
	}
	for _, m := range list {
		if m.ID == want || strings.TrimPrefix(m.ID, "measyai/") == want {
			return m.ID, true
		}
	}
	return "", false
}

// nextCommand returns the next prompt-ish command, replaying anything that was
// queued while a turn was running.
func nextCommand() (command, bool) {
	if len(queued) > 0 {
		text := queued[0]
		queued = queued[1:]
		return command{Kind: "prompt", Text: text}, true
	}
	c, ok := <-commands
	return c, ok
}

// awaitApproval blocks until the GUI answers this specific request. Prompts
// typed while the dialog is open are held rather than dropped.
func awaitApproval(id int) bool {
	for c := range commands {
		switch c.Kind {
		case "approval":
			if c.ID != id {
				continue
			}
			if c.Always {
				mode = modeDeveloper
			}
			return c.Allow || c.Always
		case "prompt":
			queued = append(queued, c.Text)
		case "auto":
			toggleApproval()
		}
	}
	return false // stdin closed: the GUI is gone
}
