package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	measyai "github.com/measyai/Go-SDK"
)

// Where the agent is pointed, and what is there.
//
// An agent that can only ever work in the directory it was launched from is
// a nuisance: you notice the wrong folder one prompt too late, and the only
// fix is to quit, cd and lose the conversation. /switch moves the whole
// session — process working directory, tool sandbox, system prompt and
// banner — in one step.

// setWorkspace points the agent at dir and returns its absolute path.
//
// The chdir is what actually matters: every tool resolves relative paths
// against the process working directory, so changing workDir alone would
// leave the UI claiming one folder while writes landed in another.
func setWorkspace(dir string) (string, error) {
	expanded, err := expandPath(dir)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(expanded)
	if err != nil {
		return "", fmt.Errorf("%s: %w", expanded, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file, not a folder", expanded)
	}
	if err := os.Chdir(expanded); err != nil {
		return "", err
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = expanded
	}
	workDir = cwd
	return cwd, nil
}

// expandPath accepts what people actually type on each platform.
//
//	~           and ~/projects            everywhere
//	%USERPROFILE% and other %VARS%        Windows
//	$HOME and ${HOME}                     unix
//	C:\Projects, C:/Projects, /home/…     as given
//
// Quotes are stripped because dragging a folder into a terminal wraps the
// path in them on every OS, and "no such directory: "C:\Users\..."" is a
// baffling way to learn that.
func expandPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return "", fmt.Errorf("no path given")
	}

	p = os.Expand(p, func(key string) string { return os.Getenv(key) })
	if runtime.GOOS == "windows" {
		p = expandWindowsVars(p)
	}

	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p[1:], "/"), `\`))
	}

	// A bare "C:" means "the current directory on drive C", which is almost
	// never what someone typing it into a prompt means.
	if runtime.GOOS == "windows" && len(p) == 2 && p[1] == ':' {
		p += `\`
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// expandWindowsVars resolves %VAR%, which os.Expand does not know about.
func expandWindowsVars(p string) string {
	for {
		start := strings.Index(p, "%")
		if start < 0 {
			return p
		}
		end := strings.Index(p[start+1:], "%")
		if end < 0 {
			return p
		}
		end += start + 1

		name := p[start+1 : end]
		value, ok := os.LookupEnv(name)
		if !ok {
			// Leave an unknown %…% alone rather than silently deleting it —
			// a wrong path you can see beats an empty one you cannot.
			return p
		}
		p = p[:start] + value + p[end+1:]
	}
}

// switchWorkspace is the /switch command. Given no argument it prompts, so
// both "/switch folder" and "/switch D:\code" work.
func switchWorkspace(arg string, history *[]measyai.Message) {
	path := strings.TrimSpace(arg)
	// "folder" is the word from the documented spelling, "/switch folder".
	// Treating it as a literal path would be obtuse.
	if strings.EqualFold(path, "folder") || strings.EqualFold(path, "dir") {
		path = ""
	}

	if path == "" {
		say("")
		say("  " + dim("New workspace path:"))
		say("  " + dim(pathExample()))
		fmt.Print("  " + paint(cUser, "›") + " ")
		if !stdin.Scan() {
			return
		}
		path = cleanInput(stdin.Text())
		if path == "" {
			return
		}
	}

	cwd, err := setWorkspace(path)
	if err != nil {
		say("  " + paint(cErr, "✗ "+err.Error()))
		return
	}

	// The system prompt names the working directory, so a switch that left
	// it stale would have the model reasoning about the old folder while the
	// tools wrote to the new one.
	if history != nil && len(*history) > 0 {
		(*history)[0] = measyai.Message{Role: "system", Content: systemPrompt(cwd)}
	}

	say("  " + paint(cOK, "✓") + "  workspace " + paint(cUser, cwd))
	if g := gitInfo(cwd); g != nil {
		say(dim("     " + g.line()))
	}
}

// pathExample shows the shape of a path on the platform in use — the three
// operating systems disagree enough that a generic example helps nobody.
func pathExample() string {
	switch runtime.GOOS {
	case "windows":
		return `e.g. C:\Projects\TestPlugin   ·   ~\Desktop   ·   %USERPROFILE%\code`
	case "darwin":
		return "e.g. /Users/you/projects/test   ·   ~/code"
	default:
		return "e.g. /home/you/projects/test   ·   ~/code"
	}
}

// openWorkspace reveals the current folder in the desktop file manager.
func openWorkspace(dir string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	// explorer.exe exits non-zero even when it worked, and a headless box has
	// no file manager at all — neither is worth an error message.
	_ = cmd.Start()
	say("  " + paint(cOK, "✓") + "  opened " + dim(dir))
}

// ---------------------------------------------------------------- git

type gitState struct {
	Branch  string
	Changed int
	Ahead   string
}

func (g *gitState) line() string {
	parts := []string{"git " + g.Branch}
	switch {
	case g.Changed == 1:
		parts = append(parts, "1 change")
	case g.Changed > 1:
		parts = append(parts, fmt.Sprintf("%d changes", g.Changed))
	default:
		parts = append(parts, "clean")
	}
	if g.Ahead != "" {
		parts = append(parts, g.Ahead)
	}
	return strings.Join(parts, " · ")
}

// gitInfo reports the repository state, or nil when dir is not a checkout.
//
// Bounded and best-effort: this runs at every startup and every /switch, and
// a slow or broken git must not hold the prompt hostage.
func gitInfo(dir string) *gitState {
	if out, err := git(dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil
	}

	state := &gitState{Branch: "detached"}
	if out, err := git(dir, "branch", "--show-current"); err == nil {
		if b := strings.TrimSpace(out); b != "" {
			state.Branch = b
		}
	}
	if out, err := git(dir, "status", "--porcelain"); err == nil {
		state.Changed = len(nonEmptyLines(out))
	}
	if out, err := git(dir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) == 2 {
			behind, ahead := fields[0], fields[1]
			var bits []string
			if ahead != "0" {
				bits = append(bits, "↑"+ahead)
			}
			if behind != "0" {
				bits = append(bits, "↓"+behind)
			}
			state.Ahead = strings.Join(bits, " ")
		}
	}
	return state
}

func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// gitCommand backs /git. Only read-only subcommands run unattended; anything
// that writes goes through the same approval gate as a shell tool, because
// "commit everything" is not a decision a harness should make quietly.
func gitCommand(arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		arg = "status"
	}
	sub, rest, _ := strings.Cut(arg, " ")

	switch sub {
	case "status":
		show(git(workDir, "status", "--short", "--branch"))
	case "diff":
		show(git(workDir, "diff", "--stat"))
		show(git(workDir, "diff"))
	case "log":
		show(git(workDir, "log", "--oneline", "-15"))
	case "commit":
		message := strings.TrimSpace(rest)
		if message == "" {
			say("  " + dim("usage: /git commit <message>"))
			return
		}
		if !approve("git commit", message) {
			return
		}
		show(git(workDir, "commit", "-am", message))
	default:
		say("  " + dim("/git status · /git diff · /git log · /git commit <message>"))
	}
}

func show(out string, err error) {
	if err != nil && strings.TrimSpace(out) == "" {
		say("  " + paint(cErr, "✗ "+err.Error()))
		return
	}
	toolBody(out, 40)
}

// ---------------------------------------------------------------- scan

// scanProject sketches what is in the workspace: the languages present, the
// biggest directories, and whether it is a repository. It answers the
// question a developer asks on arriving somewhere unfamiliar, and it gives
// the model the same footing without burning a turn on `ls -R`.
func scanProject(dir string) {
	type stat struct {
		files int
		lines int
	}
	byLang := map[string]*stat{}
	var files, skipped int

	// Bounded walk: a node_modules three levels deep would otherwise turn a
	// one-line command into a minute of disk I/O.
	const maxFiles = 20000
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isIgnoredDir(d.Name()) && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if files >= maxFiles {
			skipped++
			return nil
		}
		files++

		lang := languageOf(filepath.Ext(d.Name()))
		if lang == "" {
			return nil
		}
		s := byLang[lang]
		if s == nil {
			s = &stat{}
			byLang[lang] = s
		}
		s.files++
		if info, err := d.Info(); err == nil {
			// Bytes/40 is a serviceable stand-in for lines and costs no reads.
			s.lines += int(info.Size() / 40)
		}
		return nil
	})

	langs := make([]string, 0, len(byLang))
	for lang := range byLang {
		langs = append(langs, lang)
	}
	sort.Slice(langs, func(i, j int) bool { return byLang[langs[i]].files > byLang[langs[j]].files })

	say("")
	say("  " + paint(cUser, "Project") + dim("  "+dir))
	say("")
	if g := gitInfo(dir); g != nil {
		say("  " + dim("repository ") + g.line())
	}
	say("  " + dim("files      ") + fmt.Sprint(files))
	if skipped > 0 {
		say("  " + dim("           ") + dim(fmt.Sprintf("(stopped counting after %d)", maxFiles)))
	}

	if len(langs) == 0 {
		say("  " + dim("languages  ") + dim("nothing recognisable yet"))
		return
	}
	say("  " + dim("languages  ") + langs[0] + dim("  "+plural(byLang[langs[0]].files, "file")))
	for _, lang := range langs[1:min(len(langs), 5)] {
		say("  " + dim("           ") + lang + dim("  "+plural(byLang[lang].files, "file")))
	}
}

// plural saves the reader from "1 files", which reads as a bug in whatever
// produced it even when the number is right.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isIgnoredDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "target",
		".next", ".venv", "venv", "__pycache__", ".idea", ".vscode", "bin", "obj":
		return true
	}
	return false
}

func languageOf(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "Go"
	case ".ts", ".tsx":
		return "TypeScript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JavaScript"
	case ".py":
		return "Python"
	case ".rs":
		return "Rust"
	case ".java":
		return "Java"
	case ".kt", ".kts":
		return "Kotlin"
	case ".swift":
		return "Swift"
	case ".c", ".h":
		return "C"
	case ".cpp", ".cc", ".hpp":
		return "C++"
	case ".cs":
		return "C#"
	case ".rb":
		return "Ruby"
	case ".php":
		return "PHP"
	case ".sh", ".bash":
		return "Shell"
	case ".sql":
		return "SQL"
	case ".css", ".scss":
		return "CSS"
	case ".html":
		return "HTML"
	case ".md":
		return "Markdown"
	case ".yaml", ".yml":
		return "YAML"
	case ".json":
		return "JSON"
	default:
		return ""
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
