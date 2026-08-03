package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The tools prompt for approval on stdin, which no test can answer.
func TestMain(m *testing.M) {
	mode = modeDeveloper
	os.Exit(m.Run())
}

func TestWrapWriterKeepsTheGutter(t *testing.T) {
	var buf bytes.Buffer
	w := newWrapWriter(&buf, "> ", "| ", 20) // 18 columns of text after the gutter
	for _, delta := range []string{"the quick ", "brown fox jumps ", "over\nthe lazy dog"} {
		w.WriteString(delta)
	}
	w.Close()

	// The opening line takes the bullet; continuations take the gutter.
	want := "> the quick brown\n| fox jumps over\n| the lazy dog\n"
	if buf.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", buf.String(), want)
	}
	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		prefix := "| "
		if i == 0 {
			prefix = "> "
		}
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("line lost its gutter: %q", line)
		}
		if len(line) > 20 {
			t.Errorf("line ran past the width: %q", line)
		}
	}
}

// Anything wider than the frame has to be trimmed before it is padded, or the
// box loses its right edge.
func TestBoxedLinesNeverOutgrowTheFrame(t *testing.T) {
	long := strings.Repeat("C:/very/long/path/segment", 20)
	if got := utf8.RuneCountInString(fitRight(long, 40)); got != 40 {
		t.Errorf("fitRight produced %d columns, want 40", got)
	}
	if fitRight("short", 40) != "short" {
		t.Error("fitRight trimmed a string that already fit")
	}
	if got := visibleLen(paint(cErr, "abc") + dim("de")); got != 5 {
		t.Errorf("visibleLen counted %d columns of escapes as text, want 5", got)
	}
}

// The schema is hand-written JSON, so a typo would only surface as a 400 from
// the API mid-session.
func TestToolSchemaIsValidAndComplete(t *testing.T) {
	var defs []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(toolSchema), &defs); err != nil {
		t.Fatalf("toolSchema is not valid JSON: %v", err)
	}

	seen := map[string]bool{}
	for _, d := range defs {
		if d.Type != "function" || d.Function.Description == "" || len(d.Function.Parameters) == 0 {
			t.Errorf("%q is missing type, description or parameters", d.Function.Name)
		}
		seen[d.Function.Name] = true
	}
	// Every advertised tool must actually dispatch, or the model calls into a void.
	for _, name := range []string{"read", "list", "write", "edit", "run"} {
		if !seen[name] {
			t.Errorf("tool %q is implemented but not advertised", name)
		}
		if _, err := dispatch(context.Background(), name, toolArgs{}); err != nil &&
			strings.HasPrefix(err.Error(), "unknown tool") {
			t.Errorf("tool %q is advertised but not implemented", name)
		}
	}
}

func TestCallBuilderReassemblesFragments(t *testing.T) {
	var b callBuilder
	for _, frame := range []string{
		`[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":""}}]`,
		`[{"index":0,"function":{"arguments":"{\"path\":\"a"}}]`,
		`[{"index":0,"function":{"arguments":".txt\",\"content\":\"hi\"}"}}]`,
		`[{"index":1,"id":"call_2","type":"function","function":{"name":"run","arguments":"{\"command\":\"ls\"}"}}]`,
		`not json at all`,
	} {
		b.add(json.RawMessage(frame))
	}

	if len(b.calls) != 2 {
		t.Fatalf("assembled %d calls, want 2: %+v", len(b.calls), b.calls)
	}
	if b.calls[0].ID != "call_1" || b.calls[0].Function.Name != "write" {
		t.Errorf("call 0 = %+v", b.calls[0])
	}

	var args toolArgs
	if err := json.Unmarshal([]byte(b.calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("reassembled arguments do not parse: %v (%q)", err, b.calls[0].Function.Arguments)
	}
	if args.Path != "a.txt" || args.Content != "hi" {
		t.Errorf("args = %+v", args)
	}
	if b.calls[1].Function.Name != "run" {
		t.Errorf("call 1 = %+v", b.calls[1])
	}
}

func TestWriteThenEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "hello.txt")

	if _, err := toolWrite(path, "alpha\nbeta\ngamma\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := toolEdit(path, "beta", "BETA"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Errorf("contents = %q", got)
	}

	// An ambiguous match must fail rather than silently patch the wrong line.
	if _, err := toolEdit(path, "a", "x"); err == nil {
		t.Error("expected an error for a multi-match edit, got nil")
	}
}

func TestRunReportsFailureWithoutErroring(t *testing.T) {
	out, err := toolRun(context.Background(), "exit 3")
	if err != nil {
		t.Fatalf("a failing command must come back as output, not an error: %v", err)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("exit status missing from %q", out)
	}
}

// The timeout has to survive a grandchild holding the output pipes open, which
// is what a model starting a dev server actually does.
func TestBlockingCommandIsBounded(t *testing.T) {
	nested := `powershell -NoProfile -Command "Start-Sleep 300"`
	if runtime.GOOS != "windows" {
		nested = `sh -c "sleep 300"`
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	shell(ctx, nested).CombinedOutput()

	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("blocking command took %v to return; the timeout is not bounding it", elapsed)
	}
}

func TestLoadEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# a comment\n\nMEASYCODE_A=\"quoted value\"\nexport MEASYCODE_B=plain\nMEASYCODE_C=from-file\nnot-a-pair\n"), 0o600)

	t.Setenv("MEASYCODE_C", "from-shell")
	loadEnv(path)

	for key, want := range map[string]string{
		"MEASYCODE_A": "quoted value",
		"MEASYCODE_B": "plain",
		"MEASYCODE_C": "from-shell", // the real environment must win
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
