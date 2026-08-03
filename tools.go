package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	measyai "github.com/measyai/Go-SDK"
)

const (
	maxRead    = 200 << 10 // bytes returned to the model by read
	maxOutput  = 32 << 10  // bytes returned to the model by run
	maxEntries = 500       // paths returned by list
	runTimeout = 2 * time.Minute
)

// toolSchema is the tools array sent to the API. It is raw JSON rather than a
// Go literal because that is what it has to become anyway, and a schema builder
// for five fixed tools is a layer that earns nothing.
const toolSchema = `[
{"type":"function","function":{
  "name":"read","description":"Read a file and return its full contents.",
  "parameters":{"type":"object","properties":{
    "path":{"type":"string","description":"File path, relative to the working directory."}},
    "required":["path"]}}},
{"type":"function","function":{
  "name":"list","description":"List files under a directory, recursively. Build and dependency directories are skipped.",
  "parameters":{"type":"object","properties":{
    "path":{"type":"string","description":"Directory to list. Defaults to the working directory."}},
    "required":[]}}},
{"type":"function","function":{
  "name":"write","description":"Create or overwrite a file with the given contents. Parent directories are created.",
  "parameters":{"type":"object","properties":{
    "path":{"type":"string"},
    "content":{"type":"string","description":"The complete file contents."}},
    "required":["path","content"]}}},
{"type":"function","function":{
  "name":"edit","description":"Replace an exact snippet in an existing file. Prefer this over write for small changes to large files.",
  "parameters":{"type":"object","properties":{
    "path":{"type":"string"},
    "old":{"type":"string","description":"Text to replace. Must appear exactly once in the file, so include surrounding context."},
    "new":{"type":"string","description":"Replacement text."}},
    "required":["path","old","new"]}}},
{"type":"function","function":{
  "name":"run","description":"Run a shell command in the working directory and return its combined output and exit status. Non-interactive commands only.",
  "parameters":{"type":"object","properties":{
    "command":{"type":"string"}},
    "required":["command"]}}}
]`

// toolArgs is every parameter any tool takes. One struct for five tools beats
// five structs and a type switch.
type toolArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Old     string `json:"old"`
	New     string `json:"new"`
	Command string `json:"command"`
}

// execTool runs one requested call and returns the text handed back to the
// model. A tool failure is a result, not an error: the model reads it and
// adjusts, which is the whole point of the loop.
func execTool(ctx context.Context, tc measyai.ToolCall) string {
	var args toolArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		toolStart(cErr, tc.Function.Name, "?")
		toolResult(cErr, "bad arguments")
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	out, err := dispatch(ctx, tc.Function.Name, args)
	if err != nil {
		toolResult(cErr, err.Error())
		return "ERROR: " + err.Error()
	}
	return out
}

func dispatch(ctx context.Context, name string, a toolArgs) (string, error) {
	switch name {
	case "read":
		return toolRead(a.Path)
	case "list":
		return toolList(a.Path)
	case "write":
		return toolWrite(a.Path, a.Content)
	case "edit":
		return toolEdit(a.Path, a.Old, a.New)
	case "run":
		return toolRun(ctx, a.Command)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func toolRead(path string) (string, error) {
	toolStart(cRead, "Read", shortPath(path))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	toolResult(cRead, fmt.Sprintf("read %d lines", strings.Count(string(data), "\n")+1))
	if len(data) > maxRead {
		return string(data[:maxRead]) + "\n... [truncated]", nil
	}
	return string(data), nil
}

// skipDirs are never worth walking into and never worth the model's context.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, ".venv": true, "__pycache__": true,
}

func toolList(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	toolStart(cRead, "List", shortPath(root))

	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry is not worth aborting the walk
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if len(paths) >= maxEntries {
			return fs.SkipAll
		}
		paths = append(paths, filepath.ToSlash(p))
		return nil
	})
	if err != nil {
		return "", err
	}
	toolResult(cRead, fmt.Sprintf("found %d files", len(paths)))
	toolBody(strings.Join(paths, "\n"), 8)
	if len(paths) == 0 {
		return "(empty)", nil
	}
	return strings.Join(paths, "\n"), nil
}

func toolWrite(path, content string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("write needs a path")
	}
	verb := "Write"
	if _, err := os.Stat(path); err != nil {
		verb = "Create"
	}
	toolStart(cWrite, verb, shortPath(path))
	toolBody(content, 12)
	if !approve(verb+" "+shortPath(path), content) {
		return "", errDenied
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	toolResult(cOK, fmt.Sprintf("wrote %d bytes, %d lines",
		len(content), strings.Count(content, "\n")+1))
	return fmt.Sprintf("wrote %s (%d bytes)", path, len(content)), nil
}

func toolEdit(path, old, replacement string) (string, error) {
	if path == "" || old == "" {
		return "", fmt.Errorf("edit needs a path and the text to replace")
	}
	toolStart(cWrite, "Edit", shortPath(path))

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if n := strings.Count(string(data), old); n != 1 {
		return "", fmt.Errorf("old text matches %d times in %s, need exactly 1 — include more surrounding context", n, path)
	}

	diffBody(old, replacement)
	if !approve("Edit "+shortPath(path), "- "+old+"\n+ "+replacement) {
		return "", errDenied
	}

	updated := strings.Replace(string(data), old, replacement, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", err
	}
	toolResult(cOK, fmt.Sprintf("updated %s", shortPath(path)))
	return fmt.Sprintf("edited %s", path), nil
}

func toolRun(ctx context.Context, cmdline string) (string, error) {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return "", fmt.Errorf("run needs a command")
	}
	toolStart(cRun, "Run", cmdline)
	if !approve("Run", cmdline) {
		return "", errDenied
	}

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	sp := startSpinner("running")
	out, err := shell(ctx, cmdline).CombinedOutput()
	sp.Stop()

	text := string(out)
	if len(text) > maxOutput {
		text = text[:maxOutput] + "\n... [truncated]"
	}

	switch {
	case ctx.Err() != nil:
		toolResult(cErr, "killed after "+runTimeout.String())
		toolBody(text, 10)
		return text + "\n[killed after " + runTimeout.String() +
			" — this command blocks. Start servers in the background, or use a one-shot check instead]", nil
	case err != nil:
		// A non-zero exit is information for the model, not a harness failure.
		toolResult(cErr, err.Error())
		toolBody(text, 15)
		return fmt.Sprintf("[%v]\n%s", err, text), nil
	case strings.TrimSpace(text) == "":
		toolResult(cOK, "exit 0, no output")
		return "[exit 0, no output]", nil
	}
	toolResult(cOK, "exit 0")
	toolBody(text, 15)
	return text, nil
}

func shellName() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "sh"
}

func shell(ctx context.Context, cmdline string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", cmdline)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdline)
	}

	// A model will eventually run a foreground server. Killing the shell is not
	// enough: whatever it launched inherits the output pipes, so CombinedOutput
	// blocks forever on a process the timeout already "killed". WaitDelay is
	// what actually bounds the call — it forces the pipes shut and returns.
	cmd.WaitDelay = 5 * time.Second

	if runtime.GOOS == "windows" {
		// ponytail: taskkill /T is the whole tree on Windows. On Unix the
		// grandchild survives as an orphan; fix with Setpgid if that bites.
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
	}
	return cmd
}
