package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP support.
//
// measycode's five built-in tools cover files and a shell; everything else —
// GitHub, databases, internal APIs — is a per-user need no single binary
// should bake in. MCP (Model Context Protocol) is the standard way to bolt
// those on: a server advertises tools, a client discovers and calls them.
// measycode is that client.
//
// Servers are declared in config files with two levels:
//
//	~/.measycode/config.json          user-level: available in every workspace
//	<workspace>/.measycode/config.json project-level: travels with the repo
//
// with the project file winning per server name, and per-repo config kept
// outside the workspace is avoided on purpose: a config that lives with the
// code gets reviewed with the code.
//
// Each discovered tool is exposed to the model as mcp_<server>_<tool>, so two
// servers offering a "query" tool never collide, and the model sees at a
// glance which capability belongs to which integration. MCP tools run without
// approval prompts only in Developer mode; in Safe and Balanced they ask like
// any write, because the harness cannot tell a read-only MCP tool from a
// mutating one.

const (
	// mcpConnectTimeout bounds server startup and tool discovery. A slow npx
	// install on first launch must not hold the prompt hostage forever.
	mcpConnectTimeout = 60 * time.Second
	// mcpCallTimeout bounds a single MCP tool call.
	mcpCallTimeout = 120 * time.Second
	// mcpMaxName is the API's function-name ceiling; the mcp_<server>_<tool>
	// join is shortened deterministically rather than dropped.
	mcpMaxName = 64
	// mcpMaxResult caps a tool result handed back to the model.
	mcpMaxResult = 32 << 10
)

// mcpConfig is the on-disk shape of one config file.
type mcpConfig struct {
	Servers map[string]mcpServerSpec `json:"mcp_servers"`
}

// mcpServerSpec describes how to reach one server. Exactly one of Command
// (stdio transport) or URL (streamable HTTP transport) must be set.
type mcpServerSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	// Timeout bounds one tool call, seconds. ConnectTimeout bounds startup
	// and discovery, seconds.
	Timeout        int `json:"timeout"`
	ConnectTimeout int `json:"connect_timeout"`
}

// safeEnv passes only a baseline environment to server subprocesses: an API
// key in the caller's shell must not silently leak into a third-party MCP
// server. Anything more is opt-in via the server's env map.
var safeEnv = []string{"PATH", "HOME", "USERPROFILE", "USER", "LANG", "TERM", "SHELL", "TMPDIR", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA", "SystemRoot", "COMSPEC"}

// mcpRegistry is every connected server and its discovered tools.
type mcpRegistry struct {
	mu       sync.Mutex
	sessions map[string]*mcp.ClientSession
	// tools maps the exposed name (mcp_<server>_<tool>) to its location.
	tools map[string]mcpToolRef
	// cachedTools keeps each server's discovered mcp.Tool per
	// "server/tool" key, so the JSON schema built from it stays identical
	// for the process lifetime (prompt caching depends on that).
	cachedTools map[string]*mcp.Tool
	// schema is appended to toolSchema at generation time; nil when no
	// server is configured, keeping the API request byte-identical to before.
	schema json.RawMessage
	// failed lists servers that did not connect, so /mcp can say so once
	// instead of the model discovering it mid-task.
	failed map[string]error
	specs  map[string]mcpServerSpec
}

type mcpToolRef struct {
	server string
	name   string // the server's own tool name
}

// mcpReg is the process-wide registry. Nil until configured; execTool and
// the schema splice both tolerate that, so a measycode with no MCP config
// behaves byte-for-byte like one built before this file existed.
var mcpReg *mcpRegistry

// mcpToolsSchema returns the MCP half of the tools array for one generation.
// The two halves are spliced in generate: hand-written JSON for the five
// built-ins stays untouched, discovered tools are appended as one array.
func mcpToolsSchema() json.RawMessage {
	if mcpReg == nil || len(mcpReg.schema) == 0 {
		return nil
	}
	return mcpReg.schema
}

// spliceToolSchema merges the built-in tools array with the MCP one. Both
// are JSON arrays of function definitions; the merge is textual to keep the
// built-in half byte-identical to what it has always been.
func spliceToolSchema() json.RawMessage {
	extra := mcpToolsSchema()
	if len(extra) == 0 {
		return json.RawMessage(toolSchema)
	}
	base := strings.TrimSpace(toolSchema)
	// Strip the closing ']' of the built-ins and the opening '[' of the
	// MCP array, then join. Both are trusted, schema-validated inputs.
	joined := base[:len(base)-1] + "," + strings.TrimSpace(string(extra))[1:]
	return json.RawMessage(joined)
}

func newMCPRegistry() *mcpRegistry {
	return &mcpRegistry{
		sessions:    map[string]*mcp.ClientSession{},
		tools:       map[string]mcpToolRef{},
		cachedTools: map[string]*mcp.Tool{},
		failed:      map[string]error{},
		specs:       map[string]mcpServerSpec{},
	}
}

// mcpRegistryFromWorkspace loads user and workspace configs, connects to
// every declared server and discovers its tools. A broken server is reported
// and skipped — one bad npx package must not keep measycode from starting.
func mcpRegistryFromWorkspace(dir string) *mcpRegistry {
	specs := map[string]mcpServerSpec{}
	// User config first; the workspace file overrides it per server name,
	// because project-level settings are more specific.
	if home, err := os.UserHomeDir(); err == nil {
		mergeMCPServers(specs, loadMCPConfig(filepath.Join(home, ".measycode", "config.json")))
	}
	mergeMCPServers(specs, loadMCPConfig(filepath.Join(dir, ".measycode", "config.json")))

	reg := newMCPRegistry()
	reg.specs = specs
	if len(specs) == 0 {
		return reg
	}

	// Connections are independent, so they are made concurrently: three slow
	// npx installs in series would triple the startup wait.
	var wg sync.WaitGroup
	var mu sync.Mutex
	type discovered struct {
		server  string
		session *mcp.ClientSession
		tools   []mcp.Tool
		err     error
	}
	results := make([]discovered, 0, len(specs))
	for name, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, tools, err := connectMCP(name, spec)
			mu.Lock()
			results = append(results, discovered{name, session, tools, err})
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			reg.failed[r.server] = r.err
			continue
		}
		reg.sessions[r.server] = r.session
		for i := range r.tools {
			t := &r.tools[i]
			exposed := mcpExposedName(r.server, t.Name)
			reg.tools[exposed] = mcpToolRef{server: r.server, name: t.Name}
			reg.cachedTools[r.server+"/"+t.Name] = t
		}
	}
	reg.schema = reg.buildSchema()
	return reg
}

func mergeMCPServers(dst map[string]mcpServerSpec, src mcpConfig) {
	for name, spec := range src.Servers {
		dst[name] = spec
	}
}

// loadMCPConfig reads one config file. A missing file is the normal case; a
// malformed one is surfaced to the caller's terminal by the startup notice,
// not fatal — typos in a config must not lock anyone out of their agent.
func loadMCPConfig(path string) mcpConfig {
	var cfg mcpConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		notice(fmt.Sprintf("MCP config %s: %v — ignoring that file", shortPath(path), err))
		return mcpConfig{}
	}
	return cfg
}

// connectMCP starts one server, initialises the session and lists its tools.
func connectMCP(name string, spec mcpServerSpec) (*mcp.ClientSession, []mcp.Tool, error) {
	timeout := time.Duration(spec.ConnectTimeout) * time.Second
	if timeout <= 0 {
		timeout = mcpConnectTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "measycode", Version: version}, nil)

	var transport mcp.Transport
	switch {
	case spec.Command != "" && spec.URL != "":
		return nil, nil, fmt.Errorf("server %q sets both command and url — pick one", name)
	case spec.Command != "":
		cmd := exec.Command(spec.Command, spec.Args...)
		cmd.Env = serverEnv(spec.Env)
		transport = &mcp.CommandTransport{Command: cmd}
	case spec.URL != "":
		transport = &mcp.StreamableClientTransport{
			Endpoint:   spec.URL,
			HTTPClient: httpClientWithHeaders(spec.Headers),
		}
	default:
		return nil, nil, fmt.Errorf("server %q needs a command or a url", name)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("server %q: %w", name, err)
	}

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return nil, nil, fmt.Errorf("server %q: list tools: %w", name, err)
	}
	tools := make([]mcp.Tool, len(res.Tools))
	for i, t := range res.Tools {
		tools[i] = *t
	}
	return session, tools, nil
}

// serverEnv builds the subprocess environment: safe baseline plus the
// server's explicit env map.
func serverEnv(extra map[string]string) []string {
	var env []string
	for _, key := range safeEnv {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// mcpExposedName builds mcp_<server>_<tool>, sanitised for the API's
// function-name rules and capped at 64 characters. Characters outside
// [a-zA-Z0-9_] become '_', matching what every other MCP client does —
// including '-', which the OpenAI function-name pattern rejects.
func mcpExposedName(server, tool string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
				return r
			default:
				return '_'
			}
		}, s)
	}
	name := "mcp_" + clean(server) + "_" + clean(tool)
	if len(name) > mcpMaxName {
		name = name[:mcpMaxName]
	}
	return name
}

// buildSchema renders the discovered tools as OpenAI-compatible function
// definitions, to be spliced into toolSchema before each generation.
func (r *mcpRegistry) buildSchema() json.RawMessage {
	if len(r.tools) == 0 {
		return nil
	}
	// Exposed name → original tool, in a stable order so repeated requests
	// are byte-identical (prompt caching depends on it).
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sortStrings(names)

	// Schemas were captured at discovery; they are kept here rather than
	// re-listed so the schema stays stable for the process lifetime.
	type fn struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	var defs []fn
	for _, exposed := range names {
		ref := r.tools[exposed]
		tool := r.toolSchema(ref.server, ref.name)
		if tool == nil {
			continue
		}
		var f fn
		f.Type = "function"
		f.Function.Name = exposed
		f.Function.Description = tool.Description
		if f.Function.Description == "" {
			f.Function.Description = fmt.Sprintf("MCP tool %q from server %q", ref.name, ref.server)
		}
		f.Function.Description = "[mcp:" + ref.server + "] " + f.Function.Description
		params, err := json.Marshal(tool.InputSchema)
		if err != nil || string(params) == "null" {
			params = []byte(`{"type":"object","properties":{}}`)
		}
		f.Function.Parameters = params
		defs = append(defs, f)
	}
	if len(defs) == 0 {
		return nil
	}
	data, err := json.Marshal(defs)
	if err != nil {
		return nil
	}
	return data
}

// toolSchemas caches the discovered mcp.Tool per server for schema building.
func (r *mcpRegistry) toolSchema(server, toolName string) *mcp.Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cachedTools == nil {
		return nil
	}
	return r.cachedTools[server+"/"+toolName]
}

// call runs one MCP tool by exposed name.
func (r *mcpRegistry) call(ctx context.Context, exposed string, args json.RawMessage) (string, error) {
	r.mu.Lock()
	ref, ok := r.tools[exposed]
	session := r.sessions[ref.server]
	spec := r.specs[ref.server]
	r.mu.Unlock()
	if !ok || session == nil {
		return "", fmt.Errorf("unknown MCP tool %q", exposed)
	}

	timeout := time.Duration(spec.Timeout) * time.Second
	if timeout <= 0 {
		timeout = mcpCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ref.name, Arguments: argMap})
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
			sb.WriteString("\n")
		}
	}
	out := strings.TrimRight(sb.String(), "\n")
	if out == "" {
		// Non-text content (images, resources) is summarised rather than
		// dropped, so the model knows something came back.
		out = fmt.Sprintf("[%d content block(s) returned]", len(res.Content))
	}
	if len(out) > mcpMaxResult {
		out = out[:mcpMaxResult] + "\n... [truncated]"
	}
	if res.IsError {
		return "ERROR: " + out, nil
	}
	return out, nil
}

// closeAll shuts every server down; called from main on exit.
func (r *mcpRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, s := range r.sessions {
		s.Close()
		delete(r.sessions, name)
	}
}

// statusLines feeds /mcp: connected servers and their tool counts, then
// failures. Stable ordering keeps the output diffable between runs.
func (r *mcpRegistry) statusLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lines []string
	names := make([]string, 0, len(r.specs))
	for n := range r.specs {
		names = append(names, n)
	}
	sortStrings(names)
	for _, name := range names {
		if err, bad := r.failed[name]; bad {
			lines = append(lines, name+"|failed|"+err.Error())
			continue
		}
		n := 0
		for _, ref := range r.tools {
			if ref.server == name {
				n++
			}
		}
		lines = append(lines, fmt.Sprintf("%s|connected|%d tools", name, n))
	}
	return lines
}

// mcpStatus is the /mcp command: what is connected, what failed, and where
// the config files live so the user can fix either. With no servers
// configured it points at the two file locations instead of saying nothing.
func mcpStatus() {
	if mcpReg == nil || len(mcpReg.specs) == 0 {
		say("  " + dim("no MCP servers configured"))
		say("  " + dim("add one in ~/.measycode/config.json or .measycode/config.json"))
		return
	}
	say("  " + paint(cUser, "MCP Servers"))
	say("")
	for _, line := range mcpReg.statusLines() {
		name, rest, _ := strings.Cut(line, "|")
		state, detail, _ := strings.Cut(rest, "|")
		switch state {
		case "connected":
			say("  " + paint(cOK, "● ") + paint(cUser, name) + dim("  "+detail))
		default:
			say("  " + paint(cErr, "✗ ") + paint(cUser, name) + dim("  "+detail))
		}
	}
	if n := len(mcpReg.tools); n > 0 {
		say("")
		say(dim(fmt.Sprintf("  %d tools exposed to the model as mcp_<server>_<tool>", n)))
	}
}

// headerTransport attaches static headers to every request, for HTTP servers
// that authenticate with a bearer token or a team id.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}

func httpClientWithHeaders(headers map[string]string) *http.Client {
	return &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: headers}}
}

// sortStrings is a tiny insertion sort to avoid pulling in sort for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
