package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// isolateMCPHome keeps the user-level config lookup out of the developer's
// real ~/.measycode, the same trick isolateHome plays for credentials.
func isolateMCPHome(t *testing.T) string {
	t.Helper()
	return isolateHome(t)
}

func TestFindInstructionsPriority(t *testing.T) {
	dir := t.TempDir()

	// No file at all: the ordinary "project has no rules" case.
	if path, text, err := findInstructions(dir); err != nil || path != "" || text != "" {
		t.Errorf("empty dir: got (%q, %q, %v)", path, text, err)
	}

	// AGENTS.md alone is found.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("use pnpm, not npm"), 0o600)
	path, text, _ := findInstructions(dir)
	if filepath.Base(path) != "AGENTS.md" || !strings.Contains(text, "pnpm") {
		t.Errorf("AGENTS.md: got (%q, %q)", path, text)
	}

	// MEASY.md outranks it.
	os.WriteFile(filepath.Join(dir, "MEASY.md"), []byte("measy rules"), 0o600)
	path, text, _ = findInstructions(dir)
	if filepath.Base(path) != "MEASY.md" || !strings.Contains(text, "measy rules") {
		t.Errorf("MEASY.md priority: got (%q, %q)", path, text)
	}

	// An empty MEASY.md does not shadow a real AGENTS.md.
	os.WriteFile(filepath.Join(dir, "MEASY.md"), []byte("  \n"), 0o600)
	path, _, _ = findInstructions(dir)
	if filepath.Base(path) != "AGENTS.md" {
		t.Errorf("empty MEASY.md should fall through, got %q", path)
	}
}

func TestInstructionsTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxInstructions+500)
	os.WriteFile(filepath.Join(dir, "MEASY.md"), []byte(big), 0o600)

	_, text, _ := findInstructions(dir)
	if !strings.Contains(text, "[...truncated") {
		t.Error("an over-long file was not marked as truncated")
	}
	if len(text) > maxInstructions+100 {
		t.Errorf("truncated file still %d chars", len(text))
	}
}

func TestSystemPromptCarriesInstructions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("always run make test"), 0o600)

	prompt := systemPrompt(dir)
	if !strings.Contains(prompt, "always run make test") {
		t.Error("project instructions missing from the system prompt")
	}
	if !strings.Contains(prompt, "AGENTS.md") {
		t.Error("the prompt should name the file the rules came from")
	}

	// Without a file the prompt is exactly the base template.
	plain := systemPrompt(t.TempDir())
	if strings.Contains(plain, "Project instructions") {
		t.Error("instruction block present with no instruction file")
	}
}

func TestMCPExposedName(t *testing.T) {
	cases := map[string]string{
		"github/list-issues": "mcp_github_list_issues",
		"my-api/fetch.data":  "mcp_my_api_fetch_data",
		"time/get_now":       "mcp_time_get_now",
	}
	for in, want := range cases {
		server, tool, _ := strings.Cut(in, "/")
		if got := mcpExposedName(server, tool); got != want {
			t.Errorf("mcpExposedName(%q) = %q, want %q", in, got, want)
		}
	}
	// Over-long names are capped, not dropped.
	long := mcpExposedName(strings.Repeat("s", 40), strings.Repeat("t", 40))
	if len(long) > mcpMaxName {
		t.Errorf("exposed name %d chars, cap is %d", len(long), mcpMaxName)
	}
}

func TestSpliceToolSchemaWithoutMCP(t *testing.T) {
	// With no registry the splice must return the built-in schema untouched.
	mcpReg = nil
	if got := spliceToolSchema(); string(got) != toolSchema {
		t.Error("splice changed the built-in schema with no MCP configured")
	}
}

func TestSpliceToolSchemaMerges(t *testing.T) {
	defer func() { mcpReg = nil }()
	mcpReg = newMCPRegistry()
	mcpReg.tools["mcp_test_ping"] = mcpToolRef{server: "test", name: "ping"}
	mcpReg.cachedTools["test/ping"] = &mcp.Tool{
		Name:        "ping",
		Description: "ping the server",
	}
	mcpReg.schema = mcpReg.buildSchema()

	merged := spliceToolSchema()
	var defs []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(merged, &defs); err != nil {
		t.Fatalf("merged schema is not valid JSON: %v", err)
	}
	var names []string
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"read", "write", "mcp_test_ping"} {
		if !strings.Contains(joined, want) {
			t.Errorf("merged schema missing %q: %s", want, joined)
		}
	}
}

// TestMCPConfigMerge pins the precedence rule: the workspace file wins over
// the user file for the same server name.
func TestMCPConfigMerge(t *testing.T) {
	home := isolateMCPHome(t)
	ws := t.TempDir()

	writeCfg := func(dir, cmd string) {
		cfgDir := filepath.Join(dir, ".measycode")
		os.MkdirAll(cfgDir, 0o700)
		os.WriteFile(filepath.Join(cfgDir, "config.json"),
			[]byte(`{"mcp_servers":{"srv":{"command":"`+cmd+`"}}}`), 0o600)
	}
	writeCfg(home, "user-cmd")
	writeCfg(ws, "ws-cmd")

	reg := mcpRegistryFromWorkspace(ws)
	if got := reg.specs["srv"].Command; got != "ws-cmd" {
		t.Errorf("workspace config should win, got command %q", got)
	}
}

// TestMCPLiveServer runs the whole path against a real stdio MCP server
// written as a Go test helper: connect, discover, splice schema, call.
func TestMCPLiveServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper server uses sh; covered by CI on unix")
	}
	isolateMCPHome(t)
	ws := t.TempDir()

	// A minimal MCP stdio server as a shell script: answers initialize and
	// tools/list, and echoes arguments back from tools/call. Line-delimited
	// JSON-RPC is the whole protocol surface we need for this test.
	server := filepath.Join(ws, "fake-mcp.sh")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"0.1"}}}' ;;
    *'"method":"tools/list"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","description":"echo back","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}}' ;;
    *'"method":"tools/call"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"pong"}]}}' ;;
  esac
done
`
	if err := os.WriteFile(server, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	session, tools, err := connectMCP("fake", mcpServerSpec{Command: "sh", Args: []string{server}, ConnectTimeout: 20})
	if err != nil {
		t.Fatalf("connectMCP: %v", err)
	}
	defer session.Close()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("discovered tools = %+v", tools)
	}

	reg := newMCPRegistry()
	reg.specs["fake"] = mcpServerSpec{Command: "sh", Args: []string{server}}
	reg.sessions["fake"] = session
	exposed := mcpExposedName("fake", "echo")
	reg.tools[exposed] = mcpToolRef{server: "fake", name: "echo"}
	reg.cachedTools["fake/echo"] = &tools[0]

	out, err := reg.call(context.Background(), exposed, json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, "pong") {
		t.Errorf("call returned %q, want the server's pong", out)
	}
}
