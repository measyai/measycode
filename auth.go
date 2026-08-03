package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Signing in.
//
// There are two ways to hold a MeasyAI subscription open in a terminal, and
// this file is both of them.
//
//	browser  the device authorization grant (RFC 8628). measycode asks the
//	         API for a pair of codes, prints the short one, and waits while
//	         you approve it in a browser that is already signed in. What
//	         comes back is a **session token** — the same kind of credential
//	         the website holds, expiring on its own and revoked with every
//	         other session when the password changes.
//
//	paste    you already made an API key and would rather use that.
//
// measycode never mints an API key. That is a deliberate line: a key created
// on the user's behalf shows up in their dashboard as a credential they do
// not remember asking for, and by the third machine nobody can tell which
// key belongs to which laptop — so nobody revokes any of them. Sessions are
// the thing that is supposed to accumulate per device.
//
// Either credential lands in ~/.measycode/credentials.json, so the next
// start needs neither. MEASYAI_API_KEY in the environment still wins over
// the file: an explicit export is a deliberate act, and it is how CI and a
// throwaway shell are meant to work.
//
// Nothing here writes a credential to the terminal, to a shell history, or
// to the project directory. The browser path never puts one on the clipboard
// at all, which is most of why it exists.

const (
	// defaultAPIRoot is the API host, not the /v1 base the SDK talks to. The
	// sign-in endpoints live above /v1 because they are not part of the
	// OpenAI-compatible surface.
	defaultAPIRoot = "https://api.measyai.com"

	// credentialsFileMode is owner-only. The file holds a live key.
	credentialsFileMode = 0o600
	credentialsDirMode  = 0o700

	// loginTimeout bounds the whole flow. The server expires the code after
	// ten minutes; stopping a little later means the client's own message is
	// about the code rather than about a hang.
	loginTimeout = 11 * time.Minute
)

// credentials is what a signed-in measycode remembers between runs.
//
// Exactly one of SessionToken and APIKey is set. The browser flow produces a
// session — measycode never mints a key on anyone's behalf, because a key
// the user did not knowingly create is a key they will not recognise and
// will not dare revoke. Someone who wants a key makes it in the dashboard
// and pastes it here.
type credentials struct {
	SessionToken string `json:"session_token,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	BaseURL      string `json:"base_url"`
	// ExpiresAt is set for sessions only; a pasted key has no deadline of
	// its own.
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	ProjectID   string    `json:"project_id,omitempty"`
	ProjectName string    `json:"project_name,omitempty"`
	Account     string    `json:"account,omitempty"`
	AccountName string    `json:"account_name,omitempty"`
	Plan        string    `json:"plan,omitempty"`
	SavedAt     time.Time `json:"saved_at"`
}

// token is what goes in the Authorization header. The server tells the two
// apart by prefix, so the client does not have to say which it is sending.
func (c *credentials) token() string {
	if c == nil {
		return ""
	}
	if c.SessionToken != "" {
		return c.SessionToken
	}
	return c.APIKey
}

// label is how the credential is named in the UI: the account if we know it,
// otherwise something that at least distinguishes two credentials.
func (c *credentials) label() string {
	switch {
	case c == nil || c.token() == "":
		return ""
	case c.Account != "":
		return c.Account
	case c.APIKey != "":
		return safePrefix(c.APIKey)
	default:
		return "signed in"
	}
}

// authLabel names how this machine is authenticated. The distinction is the
// whole point of the session rework, so the UI says it out loud.
func (c *credentials) authLabel() string {
	switch {
	case c == nil || c.token() == "":
		return ""
	case c.SessionToken != "":
		return "Browser Session"
	default:
		return "API Key"
	}
}

// safePrefix shows just enough of a credential to tell two apart, and never
// enough to use. Deliberately short: this ends up on screen and in
// screenshots.
func safePrefix(token string) string {
	if len(token) <= 12 {
		return "…"
	}
	return token[:12] + "…"
}

// ---------------------------------------------------------------- storage

// credentialsPath is ~/.measycode/credentials.json. Deliberately outside the
// working directory: the agent edits files there, and a credential inside a
// repo is a credential one `git add -A` away from being published.
func credentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".measycode", "credentials.json"), nil
}

// loadCredentials reads the stored credential. A missing or unreadable file
// is the ordinary "not signed in yet" case, not an error worth reporting —
// the caller's next move is the same either way.
func loadCredentials() *credentials {
	path, err := credentialsPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil || c.token() == "" {
		return nil
	}
	return &c
}

func saveCredentials(c *credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), credentialsDirMode); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	c.SavedAt = time.Now()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	// Written to a temp file and renamed, so an interrupted write cannot
	// leave a half-file that reads as "not signed in" — or worse, as a
	// truncated key. The temp file is created with the final mode, because
	// on POSIX a 0644 window is all an attacker needs.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, credentialsFileMode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("save %s: %w", path, err)
	}
	// Rename preserves the temp file's mode, but an existing file from an
	// older version might not have had one.
	os.Chmod(path, credentialsFileMode)
	return nil
}

// clearCredentials forgets the local copy. It does not revoke the key: that
// needs the account, and a terminal that has just been told to forget its
// credential is the wrong place to be making irreversible account changes.
// The dashboard's revoke button is the real off switch.
func clearCredentials() error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- endpoints

// apiRootOf normalises either spelling of a MeasyAI URL to the root.
//
// Two conventions collide here and the mismatch is not cosmetic: the Go SDK
// wants the root and appends "/v1/..." itself, while every OpenAI-compatible
// client (and therefore the credential we store) wants the "/v1" base. Feed
// the SDK the wrong one and every request goes to "/v1/v1/chat/completions",
// which the API answers with a 404 that says nothing about the cause.
func apiRootOf(url string) string {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	return strings.TrimSuffix(url, "/v1")
}

// baseURLOf is apiRootOf with a floor under it.
//
// A credentials file can be hand-edited, half-written or left over from an
// older layout, and a base without a scheme fails in two different unhelpful
// ways: the SDK quietly falls back to the public API and presents the token
// to the wrong host, while the plain client reports "unsupported protocol
// scheme" with no hint of where the empty string came from. Neither is worth
// preserving over an obviously-correct default.
func baseURLOf(c *credentials) string {
	root := ""
	if c != nil {
		root = apiRootOf(c.BaseURL)
	}
	if !strings.HasPrefix(root, "http://") && !strings.HasPrefix(root, "https://") {
		return apiRoot()
	}
	return root
}

// apiRoot resolves the host the sign-in endpoints live on.
//
// Either environment variable may be given in either spelling; both are
// normalised, so a self-hosted install cannot get this subtly wrong.
func apiRoot() string {
	for _, env := range []string{"MEASYAI_API_URL", "MEASYAI_BASE_URL"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return apiRootOf(v)
		}
	}
	return defaultAPIRoot
}

type startResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type pollResponse struct {
	Status     string `json:"status"`
	Interval   int    `json:"interval"`
	Credential *struct {
		SessionToken string    `json:"session_token"`
		ExpiresAt    time.Time `json:"expires_at"`
		BaseURL      string    `json:"base_url"`
		ProjectID    string    `json:"project_id"`
		ProjectName  string    `json:"project_name"`
		Account      string    `json:"account"`
		AccountName  string    `json:"account_name"`
	} `json:"credential"`
}

// apiError is the envelope every MeasyAI endpoint uses for a failure.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// postJSON is a plain client rather than the SDK's: none of these endpoints
// take a key, which is the entire point — the SDK has nothing to sign with
// until this succeeds.
func postJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiRoot()+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "measycode/0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, 1<<20)); err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		var e apiError
		if json.Unmarshal(buf.Bytes(), &e) == nil && e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return fmt.Errorf("%s returned %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(buf.Bytes(), out)
}

// ---------------------------------------------------------------- the flow

// clientName is what the approval screen shows. The hostname is the useful
// part: it is how you tell "yes, that is my laptop" from "no, it is not".
func clientName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "measycode on " + runtime.GOOS
	}
	return "measycode on " + host
}

// browserLogin runs the device flow to completion and saves the credential.
//
// show is how the two front ends differ: the terminal prints the code, the
// desktop app emits it as an event. Everything else — the polling, the
// backoff, the saving — is the same, so it lives here once.
func browserLogin(ctx context.Context, show func(s startResponse)) (*credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var start startResponse
	if err := postJSON(ctx, "/cli/auth/start", map[string]any{
		"client_name": clientName(),
	}, &start); err != nil {
		return nil, err
	}

	show(start)

	interval := time.Duration(max(start.Interval, 1)) * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("sign-in timed out — run /login to try again")
		case <-time.After(interval):
		}

		var poll pollResponse
		if err := postJSON(ctx, "/cli/auth/poll",
			map[string]any{"device_code": start.DeviceCode}, &poll); err != nil {
			// A blip in the middle of a ten-minute wait is not worth losing
			// the code over; the deadline above is the real bound.
			continue
		}

		switch poll.Status {
		case "pending":
			continue
		case "slow_down":
			// The server sets the pace. Honouring it rather than guessing
			// keeps a busy client from being throttled into a failure.
			interval += time.Second
			continue
		case "denied":
			return nil, errors.New("sign-in was denied in the browser")
		case "expired":
			return nil, errors.New("the sign-in code expired — run /login to get a new one")
		case "approved":
			if poll.Credential == nil || poll.Credential.SessionToken == "" {
				return nil, errors.New("the server approved the request but sent no session")
			}
			c := &credentials{
				SessionToken: poll.Credential.SessionToken,
				ExpiresAt:    poll.Credential.ExpiresAt,
				BaseURL:      poll.Credential.BaseURL,
				ProjectID:    poll.Credential.ProjectID,
				ProjectName:  poll.Credential.ProjectName,
				Account:      poll.Credential.Account,
				AccountName:  poll.Credential.AccountName,
			}
			// The plan and the allowance are not part of the grant; ask for
			// them so the banner has something to say on the first start.
			describeCredential(c)
			if err := saveCredentials(c); err != nil {
				return nil, err
			}
			return c, nil
		default:
			return nil, fmt.Errorf("unexpected sign-in status %q", poll.Status)
		}
	}
}

// openBrowser is best-effort. The URL is printed either way, because on a
// remote box there is no browser to open and the printed link is the whole
// interface.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// Through rundll32 rather than `cmd /c start`, whose first quoted
		// argument is the window title — a URL with an ampersand in it ends
		// up interpreted by the shell instead of opened.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Detached and ignored: a machine with no browser is a normal case here,
	// not a failure to report.
	_ = cmd.Start()
}

// ---------------------------------------------------------------- terminal UI

// signIn is the first-run experience: no key anywhere, so ask how to get one.
// Returns nil if the user declines, which the caller treats as "quit".
func signIn(ctx context.Context) *credentials {
	say("")
	boxed([]string{
		paint(cUser, "✦ ") + "sign in to " + paint(cUser, "MeasyAI"),
		dim("  the agent needs an account before it can run"),
	})
	say("")
	say("  " + paint(cUser, "1") + dim("  log in with your browser") + dim("   (recommended)"))
	say("  " + paint(cUser, "2") + dim("  paste an API key"))
	say("  " + paint(cUser, "3") + dim("  quit"))
	say("")

	for {
		fmt.Print("  " + paint(cUser, "›") + " ")
		if !stdin.Scan() {
			return nil
		}
		switch cleanInput(stdin.Text()) {
		case "1", "":
			if c := terminalBrowserLogin(ctx); c != nil {
				return c
			}
			say("")
		case "2":
			if c := pasteKey(); c != nil {
				return c
			}
			say("")
		case "3", "q", "quit", "exit":
			return nil
		default:
			say("  " + dim("1, 2 or 3"))
		}
	}
}

// terminalBrowserLogin runs the device flow with the terminal's rendering.
func terminalBrowserLogin(ctx context.Context) *credentials {
	sp := startSpinner("waiting for approval")
	c, err := browserLogin(ctx, func(s startResponse) {
		sp.Stop()
		say("")
		boxed([]string{
			"open " + paint(cUser, s.VerificationURI),
			"",
			"and enter " + paint(cUser, s.UserCode),
		})
		say(dim("  the browser should open on its own · ctrl-c to cancel"))
		say("")
		openBrowser(s.VerificationURIComplete)
		sp = startSpinner("waiting for approval")
	})
	sp.Stop()

	if err != nil {
		say("  " + paint(cErr, "✗ "+err.Error()))
		return nil
	}
	announceSignIn(c)
	return c
}

// pasteKey is for someone who already has a key.
//
// measycode does not create it for them, on purpose: a key it minted would
// appear in the dashboard as a credential the user never asked for, and the
// third one of those is the one nobody dares revoke. So this path only ever
// accepts a key the user made themselves.
func pasteKey() *credentials {
	say("")
	boxed([]string{
		"MeasyCode does not create API keys for you.",
		dim("Create one yourself, then paste it here:"),
		paint(cUser, "https://measyai.com/app/api-keys"),
	})
	say("")
	fmt.Print("  " + paint(cUser, "key ›") + " ")
	if !stdin.Scan() {
		return nil
	}

	key := cleanInput(stdin.Text())
	if key == "" {
		return nil
	}
	if strings.HasPrefix(key, "msys_") {
		say("  " + paint(cErr, "✗ that is a session token, not an API key"))
		return nil
	}
	if !strings.HasPrefix(key, "msy_") {
		say("  " + paint(cErr, "✗ a MeasyAI API key starts with msy_"))
		return nil
	}

	c := &credentials{APIKey: key, BaseURL: apiRoot() + "/v1"}
	// Best effort: a key that cannot introduce itself is still usable, and
	// failing the sign-in over an unreachable /v1/me would be worse than
	// showing a slightly emptier banner.
	describeCredential(c)

	if err := saveCredentials(c); err != nil {
		say("  " + paint(cErr, "✗ "+err.Error()))
		return nil
	}
	announceSignIn(c)
	return c
}

func announceSignIn(c *credentials) {
	say("  " + paint(cOK, "✓") + "  signed in as " + paint(cUser, c.label()) +
		dim("  ·  "+c.authLabel()))
	if c.ProjectName != "" {
		say(dim("     workspace " + c.ProjectName))
	}
	if path, err := credentialsPath(); err == nil {
		say(dim("     saved to " + path))
	}
	say("")
}

// ---------------------------------------------------------------- identity

// identity is the shape of GET /v1/me.
type identity struct {
	Source string `json:"source"`
	// AuthMethod: browser_session | cli_session | api_key.
	AuthMethod string `json:"auth_method"`
	User       struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"user"`
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Scopes []string `json:"scopes"`
	Plan   string   `json:"plan"`
	Usage  struct {
		Limit       int     `json:"limit"`
		Used        int     `json:"used"`
		Remaining   int     `json:"remaining"`
		WindowHours float64 `json:"window_hours"`
		Unlimited   bool    `json:"unlimited"`
		Unit        string  `json:"unit"`
	} `json:"usage"`
}

// getJSON is an authenticated GET. The credential decides nothing about the
// header shape — the server tells a session token from an API key by prefix,
// so both travel as a plain bearer.
func getJSON(ctx context.Context, c *credentials, url string, out any) error {
	if c == nil || c.token() == "" {
		return errors.New("not signed in")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("User-Agent", "measycode/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, 4<<20)); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e apiError
		if json.Unmarshal(buf.Bytes(), &e) == nil && e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return errors.New(resp.Status)
	}
	return json.Unmarshal(buf.Bytes(), out)
}

// fetchIdentity asks the API who this credential belongs to and what is left
// of the allowance.
func fetchIdentity(ctx context.Context, c *credentials) (*identity, error) {
	var id identity
	if err := getJSON(ctx, c, baseURLOf(c)+"/v1/me", &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// describeCredential fills in the account details a pasted key does not
// carry. Silent on failure by design — see pasteKey.
func describeCredential(c *credentials) { refreshCredential(c, 10*time.Second) }

// refreshCredential re-reads the account from the server and reports whether
// anything changed.
//
// Called at every start, because the banner would otherwise show whatever
// was true on the day of sign-in — a plan upgrade made in the browser would
// never reach a terminal that stays signed in for weeks. The timeout is
// deliberately short: this sits in front of the banner, and a slow network
// must cost a stale line, never a hung prompt.
func refreshCredential(c *credentials, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id, err := fetchIdentity(ctx, c)
	if err != nil {
		return false
	}

	before := *c
	c.Account = id.User.Email
	c.AccountName = id.User.DisplayName
	c.Plan = id.Plan
	if id.Project != nil {
		c.ProjectID = id.Project.ID
		c.ProjectName = id.Project.Name
	}

	return before.Account != c.Account ||
		before.Plan != c.Plan ||
		before.ProjectName != c.ProjectName
}

// whoami prints the account behind the current key, and what is left of the
// allowance. The allowance is the number people actually want: "am I about
// to run out mid-task".
func whoami(c *credentials) {
	if c == nil {
		say("  " + dim("not signed in") + dim("  (/login)"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sp := startSpinner("checking")
	id, err := fetchIdentity(ctx, c)
	sp.Stop()

	if err != nil {
		say("  " + paint(cErr, "✗ "+err.Error()))
		say("  " + dim("key "+c.label()))
		return
	}

	name := id.User.Email
	if id.User.DisplayName != "" {
		name = id.User.DisplayName + dim(" · ") + id.User.Email
	}
	say("  " + paint(cOK, "●") + "  " + name)
	if id.Project != nil {
		say("     " + dim("workspace ") + id.Project.Name)
	}
	say("     " + dim("plan      ") + planLabel(id.Plan))
	say("     " + dim("auth      ") + authMethodLabel(id.AuthMethod))
	if len(id.Scopes) > 0 {
		say("     " + dim("scopes    ") + strings.Join(id.Scopes, ", "))
	}
	say("     " + dim("allowance ") + allowanceLine(id))
	if c.SessionToken != "" && !c.ExpiresAt.IsZero() {
		say("     " + dim("expires   ") + c.ExpiresAt.Format("2 Jan 2006"))
	}
}

// planLabel capitalises a plan id for display. Unknown ids are shown as
// they came rather than mapped to something reassuring — if the server ever
// says "enterprise", the user should see that, not "Free".
func planLabel(plan string) string {
	switch plan {
	case "", "free":
		return "Free"
	case "pro":
		return paint(cUser, "Pro")
	case "max":
		return paint(cUser, "Max")
	default:
		return plan
	}
}

// authMethodLabel turns the wire value into something a person reads. The
// server is the authority here, not the local file — if a token was revoked
// and replaced, /v1/me knows and the file does not.
func authMethodLabel(method string) string {
	switch method {
	case "cli_session":
		return paint(cOK, "✓") + " Browser Session"
	case "browser_session":
		return paint(cOK, "✓") + " Browser Session"
	case "api_key":
		return "API Key"
	default:
		return method
	}
}

func allowanceLine(id *identity) string {
	if id.Usage.Unlimited || id.Usage.Limit <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%s of %s %s left in a %.0fh window",
		compactCount(id.Usage.Remaining), compactCount(id.Usage.Limit),
		id.Usage.Unit, id.Usage.WindowHours)
}

// compactCount renders 1_250_000 as "1.2M" — the figure is a rough gauge,
// and nine digits of it is nine digits of noise.
func compactCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}
