package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// isolateHome points the credential store at a temp directory.
//
// Without this, every test that saves a credential would overwrite the one
// the developer running `go test` is actually signed in with.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)          // unix
	t.Setenv("USERPROFILE", home)   // windows
	t.Setenv("MEASYAI_API_KEY", "") // so resolveCredentials reads the file
	return home
}

func TestCredentialsRoundTrip(t *testing.T) {
	home := isolateHome(t)

	if got := loadCredentials(); got != nil {
		t.Fatalf("a fresh home reported a stored credential: %+v", got)
	}

	want := &credentials{
		SessionToken: "msys_test",
		BaseURL:      "https://api.example.com/v1",
		ProjectName:  "Default",
		Account:      "ada@example.com",
	}
	if err := saveCredentials(want); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	got := loadCredentials()
	if got == nil {
		t.Fatal("saved credential did not load back")
	}
	if got.token() != want.token() || got.Account != want.Account || got.ProjectName != want.ProjectName {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.SavedAt.IsZero() {
		t.Error("saved_at was not stamped")
	}

	path := filepath.Join(home, ".measycode", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The file holds a live key. Windows does not model POSIX bits, so the
	// assertion only means something where it is enforced.
	if runtime.GOOS != "windows" && info.Mode().Perm() != credentialsFileMode {
		t.Errorf("credentials file is %v, want %v", info.Mode().Perm(), os.FileMode(credentialsFileMode))
	}

	// No stray temp file: a leftover .tmp holds the same key with nothing
	// watching its lifetime.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("the temp file survived the save")
	}

	if err := clearCredentials(); err != nil {
		t.Fatalf("clearCredentials: %v", err)
	}
	if loadCredentials() != nil {
		t.Error("credential survived a sign-out")
	}
	// Signing out twice is something a person does; it must not error.
	if err := clearCredentials(); err != nil {
		t.Errorf("second clearCredentials: %v", err)
	}
}

// A truncated or hand-edited file must read as "not signed in" rather than
// as a credential with an empty key — which would sail past every check and
// fail at the first request with a confusing 401.
func TestLoadCredentialsRejectsUnusableFiles(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".measycode", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), credentialsDirMode); err != nil {
		t.Fatal(err)
	}

	for name, content := range map[string]string{
		"empty":         "",
		"truncated":     `{"api_key": "msy_`,
		"no key":        `{"base_url": "https://api.example.com/v1"}`,
		"blank key":     `{"api_key": "", "session_token": ""}`,
		"not an object": `["msy_test"]`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), credentialsFileMode); err != nil {
				t.Fatal(err)
			}
			if got := loadCredentials(); got != nil {
				t.Errorf("an unusable file loaded as a credential: %+v", got)
			}
		})
	}
}

func TestAPIRootDerivesFromTheBaseURL(t *testing.T) {
	cases := []struct {
		name, apiURL, baseURL, want string
	}{
		{"default", "", "", defaultAPIRoot},
		{"explicit root wins", "https://root.example", "https://other.example/v1", "https://root.example"},
		{"derived from base", "", "https://api.example.com/v1", "https://api.example.com"},
		{"trailing slash", "", "https://api.example.com/v1/", "https://api.example.com"},
		{"root with slash", "https://root.example/", "", "https://root.example"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEASYAI_API_URL", tc.apiURL)
			t.Setenv("MEASYAI_BASE_URL", tc.baseURL)
			if got := apiRoot(); got != tc.want {
				t.Errorf("apiRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The whole device flow against a stub server: start, a couple of pending
// polls, then approval. What is being pinned is that the client keeps
// polling instead of giving up, and that it saves what it is handed.
func TestBrowserLoginPollsUntilApproved(t *testing.T) {
	isolateHome(t)

	var polls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/cli/auth/start":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("start body: %v", err)
			}
			if name, _ := body["client_name"].(string); !strings.HasPrefix(name, "measycode on ") {
				t.Errorf("client_name = %q, want it to name the machine", name)
			}
			json.NewEncoder(w).Encode(startResponse{
				DeviceCode:              "secret-device-code",
				UserCode:                "CDFG-HJKM",
				VerificationURI:         "https://measyai.com/cli",
				VerificationURIComplete: "https://measyai.com/cli?code=CDFG-HJKM",
				ExpiresIn:               600,
				Interval:                1,
			})

		case "/cli/auth/poll":
			var body struct {
				DeviceCode string `json:"device_code"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.DeviceCode != "secret-device-code" {
				t.Errorf("poll sent device_code %q", body.DeviceCode)
			}

			switch polls.Add(1) {
			case 1:
				json.NewEncoder(w).Encode(map[string]any{"status": "pending", "interval": 1})
			case 2:
				// The server asking for more room must not be read as a
				// failure — this is the case that used to end sign-ins early.
				json.NewEncoder(w).Encode(map[string]any{"status": "slow_down", "interval": 1})
			default:
				json.NewEncoder(w).Encode(map[string]any{
					"status": "approved",
					"credential": map[string]any{
						"session_token": "msys_minted",
						"base_url":      "https://api.example.com/v1",
						"project_id":    "22222222-2222-2222-2222-222222222222",
						"project_name":  "Default",
						"account":       "ada@example.com",
						"account_name":  "Ada",
					},
				})
			}

		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("MEASYAI_API_URL", server.URL)

	var shown startResponse
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := browserLogin(ctx, func(s startResponse) { shown = s })
	if err != nil {
		t.Fatalf("browserLogin: %v", err)
	}

	if shown.UserCode != "CDFG-HJKM" {
		t.Errorf("the code shown to the user was %q", shown.UserCode)
	}
	if got.SessionToken != "msys_minted" || got.Account != "ada@example.com" || got.ProjectName != "Default" {
		t.Errorf("credential did not carry the server's answer: %+v", got)
	}
	if polls.Load() < 3 {
		t.Errorf("gave up after %d polls", polls.Load())
	}

	// Signed in means signed in on the next start, too.
	stored := loadCredentials()
	if stored == nil || stored.SessionToken != "msys_minted" {
		t.Errorf("approval did not persist: %+v", stored)
	}
}

// A denial is a decision, not a network problem: it must stop the flow
// immediately rather than polling out the remaining ten minutes.
func TestBrowserLoginStopsOnDenial(t *testing.T) {
	isolateHome(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/cli/auth/start" {
			json.NewEncoder(w).Encode(startResponse{DeviceCode: "d", UserCode: "CDFG-HJKM", Interval: 1})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "denied"})
	}))
	defer server.Close()

	t.Setenv("MEASYAI_API_URL", server.URL)

	started := time.Now()
	if _, err := browserLogin(context.Background(), func(startResponse) {}); err == nil {
		t.Fatal("a denied sign-in reported success")
	}
	if time.Since(started) > 10*time.Second {
		t.Error("a denial kept polling instead of stopping")
	}
	if loadCredentials() != nil {
		t.Error("a denied sign-in wrote a credential")
	}
}

// The error envelope carries a message written for a person. Losing it and
// printing "start returned 400 Bad Request" instead is a real regression:
// the server's message is the only thing that says *what* was wrong.
func TestPostJSONSurfacesTheServerMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"invalid_scope","message":"Unknown scope: chat:delete."}}`))
	}))
	defer server.Close()

	t.Setenv("MEASYAI_API_URL", server.URL)

	err := postJSON(context.Background(), "/cli/auth/start", map[string]any{}, nil)
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if !strings.Contains(err.Error(), "Unknown scope") {
		t.Errorf("the server's message was lost: %v", err)
	}
}

func TestCompactCount(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1_000: "1k", 82_400: "82k", 1_250_000: "1.2M"}
	for n, want := range cases {
		if got := compactCount(n); got != want {
			t.Errorf("compactCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCredentialLabelPrefersTheAccount(t *testing.T) {
	cases := []struct {
		name  string
		creds *credentials
		want  string
	}{
		{"nil", nil, ""},
		{"account", &credentials{SessionToken: "msys_x", Account: "ada@example.com"}, "ada@example.com"},
		{"key without account", &credentials{APIKey: "msy_0123456789abc"}, "msy_01234567…"},
		{"session without account", &credentials{SessionToken: "msys_x"}, "signed in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.creds.label(); got != tc.want {
				t.Errorf("label() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The regression that produced "Not Found (404)" on the very first prompt:
// the SDK wants the API root and appends "/v1/..." itself, while a stored
// credential holds the "/v1" base every other client wants. Handing the SDK
// the credential verbatim sent every request to "/v1/v1/chat/completions".
func TestAPIRootOfAcceptsEitherSpelling(t *testing.T) {
	cases := map[string]string{
		"https://api.measyai.com/v1":  "https://api.measyai.com",
		"https://api.measyai.com/v1/": "https://api.measyai.com",
		"https://api.measyai.com":     "https://api.measyai.com",
		"https://api.measyai.com/":    "https://api.measyai.com",
		"http://127.0.0.1:8080/v1":    "http://127.0.0.1:8080",
		"  https://x.test/v1  ":       "https://x.test",
		"":                            "",
	}
	for in, want := range cases {
		if got := apiRootOf(in); got != want {
			t.Errorf("apiRootOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A credential must never be handed to the SDK in the "/v1" spelling — that
// is the 404 above, restated as an invariant.
func TestNewClientNeverDoublesTheVersionSegment(t *testing.T) {
	for _, base := range []string{
		"https://api.measyai.com/v1",
		"https://api.measyai.com",
	} {
		if root := apiRootOf(base); strings.HasSuffix(root, "/v1") {
			t.Errorf("apiRootOf(%q) = %q, which the SDK would turn into /v1/v1/...", base, root)
		}
	}
}
