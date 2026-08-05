package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	measyai "github.com/measyai/Go-SDK"
)

const (
	// maxRetries covers transient empty generations from the upstream provider.
	maxRetries = 3
	// defaultWidth is used when stdout is not a console.
	defaultWidth = 100
)

var (
	errDenied = errors.New("skipped by user")
	stdin     = bufio.NewScanner(os.Stdin)
)

func main() {
	model := flag.String("model", "measyai/cipher", "model to use")
	dir := flag.String("dir", ".", "working directory for the agent")
	env := flag.String("env", ".env", "env file to load, if present")
	jsonMode := flag.Bool("jsonl", false, "speak the JSON-lines protocol on stdio (used by the desktop app)")
	doLogin := flag.Bool("login", false, "sign in and exit")
	doLogout := flag.Bool("logout", false, "forget the stored credential and exit")
	doWhoami := flag.Bool("whoami", false, "print the signed-in account and exit")
	auto := flag.Bool("auto", false, "run in Developer mode: never ask for approval")
	approval := flag.String("approval", "", "approval mode: safe | balanced | developer")
	flag.BoolVar(&showThinking, "think", true, "show the model's chain of thought")
	flag.Parse()

	loadEnv(*env)

	// `measy` with no flags means "here", which is why -dir defaults to "."
	// — the shell has already chosen the folder by the time we start.
	cwd, err := setWorkspace(*dir)
	if err != nil {
		die(err.Error())
	}

	// MCP servers are configured per workspace (plus a user-level file), so
	// they connect after the workspace is known and before the banner draws:
	// the banner line "3 MCP tools from github" is part of what "where is
	// this session pointed" means.
	mcpReg = mcpRegistryFromWorkspace(cwd)
	defer mcpReg.closeAll()
	for name, ferr := range mcpReg.failed {
		notice(fmt.Sprintf("MCP server %s failed: %v", name, ferr))
	}

	if picked, ok := parseApprovalMode(*approval); ok {
		mode = picked
	}
	if *auto {
		mode = modeDeveloper
	}

	if *jsonMode {
		startJSONL()
	} else {
		enableANSI()
	}
	stdin.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// The one-shot credential commands run before anything else needs a key,
	// so `measycode -login` works on a machine that has never had one.
	switch {
	case *doLogout:
		if err := clearCredentials(); err != nil {
			die(err.Error())
		}
		say("  " + paint(cOK, "✓") + "  signed out on this machine" +
			dim("  (change your password at measyai.com to revoke every session)"))
		return
	case *doLogin:
		if terminalBrowserLogin(context.Background()) == nil {
			os.Exit(1)
		}
		return
	case *doWhoami:
		whoami(resolveCredentials())
		return
	}

	creds := resolveCredentials()
	if creds == nil && !*jsonMode {
		// First run: no key in the environment and none stored. Asking beats
		// dying with an instruction to go set a variable.
		if creds = signIn(context.Background()); creds == nil {
			return
		}
	}

	history := []measyai.Message{{Role: "system", Content: systemPrompt(cwd)}}
	current := *model

	if *jsonMode {
		runJSONL(creds, current, cwd, &history)
		return
	}

	// Bring the account line up to date before drawing it. Bounded hard:
	// a plan shown one start late is a cosmetic problem, a prompt that
	// hangs on a slow network is not.
	if refreshCredential(creds, 2500*time.Millisecond) {
		if err := saveCredentials(creds); err != nil {
			notice("could not save refreshed account details: " + err.Error())
		}
	}

	client := newClient(creds)
	banner(current, cwd, creds)
	for {
		input, ok := readInput()
		if !ok {
			return
		}

		// Commands that take an argument, matched by prefix. Checked before
		// the exact-match switch so "/switch D:\code" and a bare "/switch"
		// reach the same handler.
		if verb, arg, ok := splitCommand(input); ok {
			switch verb {
			case "/model":
				// The catalogue is fetched with the credential.
				if creds == nil {
					notSignedIn()
					continue
				}
				current = pickModel(creds, current, arg)
				say("")
				continue
			case "/switch", "/cd":
				switchWorkspace(arg, &history)
				say("")
				continue
			case "/approval":
				chooseApprovalMode(arg)
				say("")
				continue
			case "/git":
				gitCommand(arg)
				say("")
				continue
			}
		}

		switch input {
		case "":
			continue
		case "/exit", "/quit":
			return
		case "/pwd":
			say("  " + paint(cUser, workDir))
			if g := gitInfo(workDir); g != nil {
				say(dim("  " + g.line()))
			}
			say("")
			continue
		case "/open":
			openWorkspace(workDir)
			say("")
			continue
		case "/scan":
			scanProject(workDir)
			say("")
			continue
		case "/usage":
			usage(creds)
			say("")
			continue
		case "/login":
			if c := terminalBrowserLogin(context.Background()); c != nil {
				creds, client = c, newClient(c)
				// The system prompt is unaffected, but the banner's account
				// line is now stale — reprint it rather than leave a header
				// claiming the previous account.
				banner(current, workDir, creds)
			}
			say("")
			continue
		case "/logout":
			if err := clearCredentials(); err != nil {
				say("  " + paint(cErr, "✗ "+err.Error()))
			} else {
				// The client is dropped with the credential: leaving it in
				// place would keep the session working off a key the user
				// just asked this machine to forget.
				creds, client = nil, nil
				say(dim("  signed out · /login to sign in again"))
			}
			say("")
			continue
		case "/whoami":
			whoami(creds)
			say("")
			continue
		case "/reset":
			history = history[:1]
			say(dim("  context cleared"))
			say("")
			continue
		case "/auto":
			toggleApproval()
			say("")
			continue
		case "/think":
			showThinking = !showThinking
			say(dim("  thinking " + onOff(showThinking)))
			say("")
			continue
		case "/mcp":
			mcpStatus()
			say("")
			continue
		case "/help":
			helpText()
			say("")
			continue
		}

		// Reachable only via /logout mid-session; the first-run flow means a
		// session never starts without one.
		if creds == nil {
			notSignedIn()
			continue
		}

		// Ultrathink: if the message starts with "ultrathink" (case-insensitive),
		// strip the keyword and enable extended reasoning for this turn.
		ultra := false
		if trimmed, found := stripUltrathink(input); found {
			ultra = true
			input = trimmed
			if input == "" {
				notice("ultrathink needs a message after it")
				continue
			}
			note(cRun, "✻", "ultrathink on for this turn")
		}

		history = append(history, measyai.Message{Role: "user", Content: input})
		if err := turn(client, current, &history, ultra); err != nil {
			say("  " + paint(cErr, "✗ "+err.Error()))
		}
		say("")
	}
}

func notSignedIn() {
	say("  " + dim("not signed in") + dim("  ·  /login"))
	say("")
}

// splitCommand separates "/switch D:\code" into verb and argument. A bare
// "/switch" still matches, with an empty argument, so a command that can
// prompt for what it needs gets the chance to.
func splitCommand(input string) (verb, arg string, ok bool) {
	if !strings.HasPrefix(input, "/") {
		return "", "", false
	}
	verb, arg, _ = strings.Cut(input, " ")
	return verb, strings.TrimSpace(arg), true
}

// stripUltrathink detects when the user prefixes a message with "ultrathink"
// (case-insensitive). Returns the remaining message and true if the prefix
// was present. Works like Claude Code's ultrathink keyword.
func stripUltrathink(input string) (string, bool) {
	const keyword = "ultrathink"
	lower := strings.ToLower(input)
	if lower == keyword {
		return "", true
	}
	if strings.HasPrefix(lower, keyword+" ") {
		return strings.TrimSpace(input[len(keyword):]), true
	}
	return input, false
}

// usage prints what is left of the rolling allowance. Split out from
// /whoami because "am I about to run out" is asked far more often than
// "which account is this", and it deserves its own command.
func usage(c *credentials) {
	if c == nil {
		notSignedIn()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sp := startSpinner("checking")
	id, err := fetchIdentity(ctx, c)
	sp.Stop()
	if err != nil {
		say("  " + paint(cErr, "✗ "+err.Error()))
		return
	}

	if id.Usage.Unlimited || id.Usage.Limit <= 0 {
		say("  " + paint(cOK, "●") + "  unlimited")
		return
	}

	used, limit := id.Usage.Used, id.Usage.Limit
	say("  " + paint(cUser, "Token usage"))
	say("")
	say("  " + usageBar(used, limit))
	say("  " + dim(fmt.Sprintf("%s of %s used  ·  %s left",
		compactCount(used), compactCount(limit), compactCount(id.Usage.Remaining))))
	say("  " + dim(fmt.Sprintf("rolling %.0f-hour window", id.Usage.WindowHours)))
}

// usageBar is a plain proportion bar. It turns two numbers into the one
// thing the reader wants — "how close am I" — without arithmetic.
func usageBar(used, limit int) string {
	const width = 32
	filled := 0
	if limit > 0 {
		filled = used * width / limit
	}
	filled = max(0, min(filled, width))

	colour := cOK
	switch {
	case filled >= width*9/10:
		colour = cErr
	case filled >= width*3/4:
		colour = cWrite
	}
	return paint(colour, strings.Repeat("█", filled)) +
		dim(strings.Repeat("░", width-filled))
}

// resolveCredentials finds the key to run with.
//
// The environment wins over the stored credential, and deliberately so: an
// explicit MEASYAI_API_KEY is how CI, a container and a throwaway shell are
// meant to work, and a file written months ago should not quietly override
// the one the operator just exported.
func resolveCredentials() *credentials {
	if key := strings.TrimSpace(os.Getenv("MEASYAI_API_KEY")); key != "" {
		return &credentials{APIKey: key, BaseURL: apiRoot() + "/v1"}
	}
	return loadCredentials()
}

// newClient builds the SDK client for a credential.
//
// The SDK wants the API *root* and appends "/v1/..." itself, while a
// credential stores the OpenAI-compatible base ("…/api.measyai.com/v1")
// because that is what every other client wants. Handing the SDK the latter
// produced "/v1/v1/chat/completions" and a 404 on the first prompt — hence
// apiRootOf, which accepts either form.
func newClient(c *credentials) *measyai.Client {
	opts := []measyai.Option{
		measyai.WithUserAgent("measycode/" + version),
		// No overall Timeout: a generation legitimately runs for minutes.
		measyai.WithHTTPClient(&http.Client{Transport: tappingTransport{http.DefaultTransport}}),
	}
	opts = append(opts, measyai.WithBaseURL(baseURLOf(c)))
	return measyai.New(c.token(), opts...)
}

// loadEnv reads KEY=value lines from a .env file into the environment. A
// variable already set in the real environment wins, so an explicit export
// still overrides the file. A missing file is the normal case, not an error.
func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
	}
}

// turn runs the model until it stops asking for tools.
func turn(client *measyai.Client, model string, history *[]measyai.Message, ultrathink ...bool) error {
	ultra := len(ultrathink) > 0 && ultrathink[0]
	started, tokens, retries := time.Now(), 0, 0

	// Unbounded by design: the loop runs until the model stops asking for
	// tools. Ctrl-C is the brake.
	for {
		// Per-turn signal handling: ctrl-c aborts the reply in flight, and at
		// the prompt it falls back to Go's default handler and exits.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		reply, err := generate(ctx, client, model, *history, ultra)
		interrupted := ctx.Err() != nil
		stop()
		tokens += reply.tokens

		if reply.text != "" || len(reply.calls) > 0 {
			*history = append(*history, measyai.Message{
				Role:      "assistant",
				Content:   reply.text,
				ToolCalls: reply.calls,
			})
		}
		if interrupted {
			notice("interrupted")
			return nil
		}
		if err != nil {
			return err
		}

		if len(reply.calls) == 0 {
			// The upstream provider intermittently ends a generation with
			// finish_reason "error" — HTTP 200, tokens billed, no content at
			// all. Retrying the unchanged history is the only recovery.
			if strings.TrimSpace(reply.text) == "" {
				if retries >= maxRetries {
					return fmt.Errorf("model returned nothing %d times (finish_reason %q)", retries+1, reply.reason)
				}
				retries++
				notice(fmt.Sprintf("empty reply (%s), retry %d/%d", reply.reason, retries, maxRetries))
				time.Sleep(time.Duration(retries) * time.Second)
				continue
			}
			footer(time.Since(started), tokens)
			return nil
		}
		retries = 0

		for _, tc := range reply.calls {
			*history = append(*history, measyai.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    execTool(context.Background(), tc),
			})
		}
	}
}

// reply is one completed generation.
type reply struct {
	text   string
	calls  []measyai.ToolCall
	reason string // finish_reason, which is how a failed generation announces itself
	tokens int
}

// generate streams one completion, printing prose as it arrives and assembling
// any tool calls the model requested.
func generate(ctx context.Context, client *measyai.Client, model string, history []measyai.Message, ultrathink ...bool) (reply, error) {
	ultra := len(ultrathink) > 0 && ultrathink[0]

	label := "thinking"
	if ultra {
		label = "ultrathinking"
	}
	sp := startSpinner(label)
	defer sp.Stop()

	extra := map[string]any{
		"tools":       spliceToolSchema(),
		"tool_choice": "auto",
	}
	if ultra {
		extra["reasoning_effort"] = "max"
	}

	stream, err := client.Chat.Stream(ctx, measyai.ChatRequest{
		Model:    model,
		Messages: history,
		Extra:    extra,
	})
	if err != nil {
		return reply{}, err
	}
	defer stream.Close()

	var out reply
	var text strings.Builder
	var calls callBuilder
	prose := newWrapWriter(os.Stdout, paint(cBullet, "● "), "  ", termWidth())

	thoughts := newThinking()
	reasoningSink = func(s string) {
		sp.Stop() // the spinner owns the line until the first real output
		thoughts.write(s)
	}
	defer func() { reasoningSink = nil }()

	for stream.Next() {
		if delta := stream.Delta(); delta != "" {
			sp.Stop()
			thoughts.close() // the answer supersedes the reasoning behind it
			if jsonOut != nil {
				emit(event{Kind: "delta", Text: delta})
			} else {
				prose.WriteString(delta)
			}
			text.WriteString(delta)
		}
		chunk := stream.Chunk()
		if chunk.Usage != nil {
			out.tokens = chunk.Usage.TotalTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if raw := chunk.Choices[0].Delta.ToolCalls; len(raw) > 0 {
			calls.add(raw)
		}
		if chunk.Choices[0].FinishReason != nil {
			out.reason = *chunk.Choices[0].FinishReason
		}
	}
	sp.Stop()
	thoughts.close()
	if jsonOut == nil {
		prose.Close()
	}

	out.text, out.calls = text.String(), calls.calls
	return out, stream.Err()
}

// toolCallDelta is the streamed fragment shape. One call is split across many
// frames — the name arrives once, then arguments accumulate — and index is what
// ties the fragments together.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// callBuilder reassembles streamed tool-call fragments into whole calls.
type callBuilder struct{ calls []measyai.ToolCall }

func (b *callBuilder) add(raw json.RawMessage) {
	var deltas []toolCallDelta
	if err := json.Unmarshal(raw, &deltas); err != nil {
		return // a frame we cannot read is not worth killing the generation
	}
	for _, d := range deltas {
		if d.Index < 0 {
			continue
		}
		for d.Index >= len(b.calls) {
			b.calls = append(b.calls, measyai.ToolCall{Type: "function"})
		}
		call := &b.calls[d.Index]
		if d.ID != "" {
			call.ID = d.ID
		}
		if d.Type != "" {
			call.Type = d.Type
		}
		if d.Function.Name != "" {
			call.Function.Name = d.Function.Name
		}
		call.Function.Arguments += d.Function.Arguments
	}
}
