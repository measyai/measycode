package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// version is stamped into the banner, the user agent and the approval
// events we log. It is bumped manually when releasing.
const version = "1.1.0"

// Palette. Kept as raw SGR codes so paint stays a single string concat.
const (
	cUser   = "38;5;215" // warm orange, the accent
	cRead   = "38;5;110" // blue
	cWrite  = "38;5;180" // sand
	cRun    = "38;5;140" // violet
	cOK     = "38;5;114" // green
	cErr    = "38;5;203" // red
	cFrame  = "38;5;240" // box drawing
	cBullet = "38;5;250"
	cThink  = "38;5;245" // chain of thought
)

// useColor is off for NO_COLOR and switched off by enableANSI when stdout is
// not a console. FORCE_COLOR wins over the console check, for piping into a
// pager or a CI log that renders escapes anyway.
var (
	useColor   = os.Getenv("NO_COLOR") == ""
	forceColor = os.Getenv("FORCE_COLOR") != ""
)

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func dim(s string) string { return paint("2", s) }

func say(s string) { fmt.Println(s) }

func die(msg string) {
	fmt.Fprintln(os.Stderr, "measycode: "+msg)
	os.Exit(1)
}

// ---------------------------------------------------------------- wrapping

// wrapWriter word-wraps text as it streams, keeping a constant left gutter.
// Terminals soft-wrap on their own, but they wrap back to column zero, which
// breaks the gutter the rest of the UI is aligned to.
type wrapWriter struct {
	out    io.Writer
	first  string // written once, in place of indent, on the opening line
	indent string
	width  int
	col    int
	word   strings.Builder
	fresh  bool // at the start of a line, indent not yet written
	space  bool // a separator is owed, held back so it never ends a line
}

func newWrapWriter(out io.Writer, first, indent string, width int) *wrapWriter {
	if width < 20 {
		width = defaultWidth
	}
	return &wrapWriter{out: out, first: first, indent: indent, width: width - len(indent) - 1, fresh: true}
}

func (w *wrapWriter) WriteString(s string) {
	for _, r := range s {
		switch r {
		case '\n':
			w.flushWord()
			fmt.Fprint(w.out, "\n")
			w.col, w.fresh, w.space = 0, true, false
		case ' ', '\t':
			w.flushWord()
			w.space = w.col > 0
		default:
			w.word.WriteRune(r)
		}
	}
}

func (w *wrapWriter) flushWord() {
	if w.word.Len() == 0 {
		return
	}
	word := w.word.String()
	w.word.Reset()
	n := utf8.RuneCountInString(word)

	need := n
	if w.space {
		need++
	}
	if w.col > 0 && w.col+need > w.width {
		fmt.Fprint(w.out, "\n")
		w.col, w.fresh, w.space = 0, true, false
	}
	if w.fresh {
		if w.first != "" {
			fmt.Fprint(w.out, w.first)
			w.first = ""
		} else {
			fmt.Fprint(w.out, w.indent)
		}
		w.fresh = false
	}
	if w.space {
		fmt.Fprint(w.out, " ")
		w.col++
		w.space = false
	}
	fmt.Fprint(w.out, word)
	w.col += n
}

// Close flushes the trailing word and ends the line.
func (w *wrapWriter) Close() {
	w.flushWord()
	if !w.fresh {
		fmt.Fprint(w.out, "\n")
	}
}

// ---------------------------------------------------------------- spinner

var spinFrames = []string{"✢", "✳", "✻", "✽", "✻", "✳"}

// spinner fills the long silences. This API streams hundreds of empty frames
// before any content arrives, so without one the tool looks hung.
type spinner struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func startSpinner(label string) *spinner {
	if !useColor {
		return nil // a dumb or redirected terminal cannot repaint a line
	}
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		start := time.Now()
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				fmt.Print("\r\033[K")
				return
			case <-time.After(120 * time.Millisecond):
			}
			fmt.Printf("\r\033[K%s %s %s",
				paint(cUser, spinFrames[i%len(spinFrames)]),
				label,
				dim(fmt.Sprintf("(%.0fs · ctrl-c to interrupt)", time.Since(start).Seconds())))
		}
	}()
	return s
}

// Stop clears the spinner line and waits for the goroutine to release stdout,
// so nothing else can interleave with it. Safe to call more than once.
func (s *spinner) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
	})
}

// ---------------------------------------------------------------- thinking

// showThinking controls whether the chain of thought is rendered. It is on by
// default: watching the model reason is most of the point of a harness.
var showThinking = true

// thinking renders the reasoning stream, dimmed and set apart from the answer.
// It opens itself on the first fragment, so a model that does no reasoning
// (cadence never does) prints no header at all.
type thinking struct {
	out  *wrapWriter
	open bool
}

func newThinking() *thinking { return &thinking{} }

func (t *thinking) write(s string) {
	if !showThinking || s == "" {
		return
	}
	if jsonOut != nil {
		emit(event{Kind: "reasoning", Text: s})
		return
	}
	if !t.open {
		say(paint(cThink, "✻ thinking"))
		if useColor {
			fmt.Print("\033[2;3m") // dim italic, closed in close()
		}
		t.out = newWrapWriter(os.Stdout, "  ", "  ", termWidth())
		t.open = true
	}
	t.out.WriteString(s)
}

func (t *thinking) close() {
	if !t.open {
		return
	}
	t.out.Close()
	if useColor {
		fmt.Print("\033[0m")
	}
	say("")
	t.open, t.out = false, nil
}

// ---------------------------------------------------------------- tool output

const bodyIndent = "     "

// workDir is where the agent was pointed, used only to shorten displayed paths.
var workDir string

// shortPath renders a path relative to the working directory. Models hand back
// absolute paths, and a 90-character prefix on every tool line drowns the UI.
func shortPath(p string) string {
	if p == "" || !filepath.IsAbs(p) {
		return filepath.ToSlash(p)
	}
	if rel, err := filepath.Rel(workDir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

// toolStart announces a call: ● Write(index.html)
func toolStart(color, name, arg string) {
	if jsonOut != nil {
		emit(event{Kind: "tool_start", Name: name, Arg: arg})
		return
	}
	say(paint(color, "● ") + name + dim("(") + arg + dim(")"))
}

// toolResult is the one-line outcome hanging off the call.
func toolResult(color, summary string) {
	if jsonOut != nil {
		emit(event{Kind: "tool_result", Summary: summary, OK: color != cErr})
		return
	}
	say("  " + paint(color, "⎿") + "  " + dim(summary))
}

// toolBody prints detail under a result, clipped.
func toolBody(body string, maxLines int) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return
	}
	if jsonOut != nil {
		emit(event{Kind: "tool_body", Text: body})
		return
	}
	lines := strings.Split(body, "\n")
	clipped := 0
	if len(lines) > maxLines {
		clipped = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	width := termWidth() - len(bodyIndent) - 1
	for _, line := range lines {
		line = strings.ReplaceAll(line, "\t", "    ")
		if utf8.RuneCountInString(line) > width {
			line = string([]rune(line)[:width-1]) + "…"
		}
		say(dim(bodyIndent + line))
	}
	if clipped > 0 {
		say(dim(fmt.Sprintf("%s… +%d lines", bodyIndent, clipped)))
	}
}

// diffBody shows an edit as removed and added lines.
func diffBody(old, replacement string) {
	if jsonOut != nil {
		emit(event{Kind: "tool_body", Text: "- " +
			strings.ReplaceAll(old, "\n", "\n- ") + "\n+ " +
			strings.ReplaceAll(replacement, "\n", "\n+ ")})
		return
	}
	for _, line := range clip(strings.Split(old, "\n"), 8) {
		say(bodyIndent + paint(cErr, "- "+line))
	}
	for _, line := range clip(strings.Split(replacement, "\n"), 8) {
		say(bodyIndent + paint(cOK, "+ "+line))
	}
}

func clip(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	return append(lines[:max:max], fmt.Sprintf("… +%d lines", len(lines)-max))
}

func note(color, symbol, text string) { say("  " + paint(color, symbol) + "  " + dim(text)) }

// ---------------------------------------------------------------- chrome

// wordmark is MEASYCODE in the ANSI Shadow figlet font.
var wordmark = []string{
	`███╗   ███╗███████╗ █████╗ ███████╗██╗   ██╗ ██████╗ ██████╗ ██████╗ ███████╗`,
	`████╗ ████║██╔════╝██╔══██╗██╔════╝╚██╗ ██╔╝██╔════╝██╔═══██╗██╔══██╗██╔════╝`,
	`██╔████╔██║█████╗  ███████║███████╗ ╚████╔╝ ██║     ██║   ██║██║  ██║█████╗  `,
	`██║╚██╔╝██║██╔══╝  ██╔══██║╚════██║  ╚██╔╝  ██║     ██║   ██║██║  ██║██╔══╝  `,
	`██║ ╚═╝ ██║███████╗██║  ██║███████║   ██║   ╚██████╗╚██████╔╝██████╔╝███████╗`,
	`╚═╝     ╚═╝╚══════╝╚═╝  ╚═╝╚══════╝   ╚═╝    ╚═════╝ ╚═════╝ ╚═════╝ ╚══════╝`,
}

// banner is the first thing anyone sees, so it answers the four questions
// people actually have on starting a coding agent: which model is going to
// run, where it is pointed, whose account pays, and how much it may do
// without asking. Labelled sections rather than a run-on line — at a glance
// you want to find one of those four, not read all of them.
func banner(modelID, cwd string, creds *credentials) {
	say("")
	if termWidth() >= 80 && useColor {
		for _, line := range wordmark {
			say(paint(cUser, line))
		}
		say("")
	}

	lines := []string{
		paint(cUser, "✦ ") + "MeasyCode CLI" + dim("  v"+version),
		dim("  Powered by MeasyAI ") + modelDisplayName(modelID),
		"",
		dim("  Workspace"),
		"  " + fitRight(cwd, boxWidth()-6),
	}
	if g := gitInfo(cwd); g != nil {
		lines = append(lines, dim("  "+g.line()))
	}
	// Connected MCP servers belong in the banner: they change what the
	// agent can do as much as the model does.
	if mcpReg != nil && len(mcpReg.tools) > 0 {
		var servers []string
		seen := map[string]bool{}
		for _, ref := range mcpReg.tools {
			if !seen[ref.server] {
				seen[ref.server] = true
				servers = append(servers, ref.server)
			}
		}
		sortStrings(servers)
		lines = append(lines, dim(fmt.Sprintf("  mcp      %d tools · %s",
			len(mcpReg.tools), strings.Join(servers, ", "))))
	}

	// Which account is paying for this session is worth its own line: the
	// same binary is routinely run against a personal account and a work one.
	if label := creds.label(); label != "" {
		lines = append(lines, "", dim("  Account"), "  "+label)

		// The plan leads the detail line: it decides the allowance, so it is
		// the thing someone actually wants to see here. Empty only on a
		// credential saved before plans existed, or when the refresh at
		// startup could not reach the server — in which case saying nothing
		// beats guessing "Free".
		var detail []string
		if plan := planName(creds.Plan); plan != "" {
			detail = append(detail, plan+" plan")
		}
		if auth := creds.authLabel(); auth != "" {
			detail = append(detail, auth)
		}
		if creds.ProjectName != "" {
			detail = append(detail, creds.ProjectName)
		}
		lines = append(lines, dim("  "+strings.Join(detail, "  ·  ")))
	}

	lines = append(lines, "", dim("  Mode"), "  "+modeLabel())
	boxed(lines)

	say(dim("  /help for commands · " + runtime.GOOS + "/" + runtime.GOARCH))
	say("")
}

// planName is the plain-text plan label for the banner, where the box
// padding counts visible columns and colour escapes would misalign it.
func planName(plan string) string {
	switch plan {
	case "":
		return ""
	case "free":
		return "Free"
	case "pro":
		return "Pro"
	case "max":
		return "Max"
	default:
		return plan
	}
}

// modelDisplayName turns "measyai/cipher" into "Cipher" for prose. The full
// id still appears wherever it has to be typed.
func modelDisplayName(id string) string {
	name := id
	if _, after, ok := strings.Cut(id, "/"); ok {
		name = after
	}
	if name == "" {
		return id
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// boxed draws a rounded box around already-coloured lines.
func boxed(lines []string) {
	width := boxWidth()
	say(paint(cFrame, "╭"+strings.Repeat("─", width-2)+"╮"))
	for _, line := range lines {
		pad := width - 4 - visibleLen(line)
		if pad < 0 {
			pad = 0
		}
		say(paint(cFrame, "│") + " " + line + strings.Repeat(" ", pad) + " " + paint(cFrame, "│"))
	}
	say(paint(cFrame, "╰"+strings.Repeat("─", width-2)+"╯"))
}

func boxWidth() int {
	w := termWidth()
	if w > 100 {
		w = 100
	}
	if w < 40 {
		w = 40
	}
	return w
}

// fitRight keeps the tail of a string that will not fit, which for a path is
// the informative end. An overlong line would otherwise run past the box edge.
func fitRight(s string, max int) string {
	if max < 2 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-max+1:])
}

// visibleLen counts printable columns, skipping SGR escape sequences.
func visibleLen(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		case r == '\033':
			inEscape = true
		default:
			n++
		}
	}
	return n
}

// readInput draws the prompt box and reads one line inside it. The box is
// printed whole, then the cursor is parked on the middle line, so the frame is
// already closed while typing instead of appearing after enter.
func readInput() (string, bool) {
	if !useColor {
		fmt.Print("› ")
		if !stdin.Scan() {
			return "", false
		}
		return cleanInput(stdin.Text()), true
	}

	width := boxWidth()
	say(paint(cFrame, "╭"+strings.Repeat("─", width-2)+"╮"))
	say(paint(cFrame, "│") + " " + paint(cUser, ">") + strings.Repeat(" ", width-4) + paint(cFrame, "│"))
	say(paint(cFrame, "╰"+strings.Repeat("─", width-2)+"╯"))
	fmt.Print("\033[2A\033[5G") // up onto the input line, past "│ > "

	if !stdin.Scan() {
		fmt.Print("\033[2B\n")
		return "", false
	}
	fmt.Print("\033[1B\r\n") // step below the closed box
	return cleanInput(stdin.Text()), true
}

// cleanInput strips the UTF-8 BOM a piped first line can carry, which TrimSpace
// leaves in place — enough to stop a slash command from ever matching.
func cleanInput(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
}

// notice is an aside from the harness itself, not from the model.
func notice(text string) {
	if jsonOut != nil {
		emit(event{Kind: "notice", Text: text, Auto: developerMode()})
		return
	}
	say(dim("  " + text))
}

func footer(elapsed time.Duration, tokens int) {
	if jsonOut != nil {
		emit(event{Kind: "turn_stats", Millis: elapsed.Milliseconds(), Tokens: tokens})
		return
	}
	line := fmt.Sprintf("%.1fs", elapsed.Seconds())
	if tokens > 0 {
		line += fmt.Sprintf(" · %d tokens", tokens)
	}
	say(dim("  " + line))
}

// helpText groups the commands the way people look for them: by what they
// are trying to change. A flat alphabetical list of fifteen commands is a
// list nobody reads twice.
func helpText() {
	groups := []struct {
		title string
		rows  [][2]string
	}{
		{"Models", [][2]string{
			{"/model", "list available models"},
			{"/model cipher", "switch model"},
		}},
		{"Workspace", [][2]string{
			{"/pwd", "show current folder"},
			{"/switch <path>", "change workspace"},
			{"/open", "open current folder"},
			{"/scan", "scan project"},
			{"/git", "status · diff · log · commit <msg>"},
		}},
		{"Account", [][2]string{
			{"/login", "browser login"},
			{"/logout", "remove session"},
			{"/whoami", "account information"},
			{"/usage", "token usage"},
		}},
		{"Settings", [][2]string{
			{"/approval", "approval mode (currently " + mode.String() + ")"},
			{"/auto", "jump to Developer mode and back"},
			{"/think", "reasoning mode (currently " + onOff(showThinking) + ")"},
			{"/mcp", "MCP server status"},
			{"/reset", "clear conversation"},
		}},
		{"General", [][2]string{
			{"/help", "this list"},
			{"/exit", "quit"},
			{"ctrl-c", "interrupt the reply in flight"},
		}},
	}

	for _, g := range groups {
		say("")
		say("  " + paint(cUser, g.title))
		for _, row := range g.rows {
			say("    " + fmt.Sprintf("%-16s", row[0]) + dim(row[1]))
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---------------------------------------------------------------- approval

// approve gates anything that changes the workspace: writes, edits, and
// anything spawned as a process. Answering "a" drops the gate for the rest
// of the session, which is the same switch as -auto and /approval.
func approve(action, detail string) bool { return approveTool(false, action, detail) }

// approveRead gates the tools that only look. In Balanced — the default —
// these run unattended, which is the entire reason the mode exists.
func approveRead(action, detail string) bool { return approveTool(true, action, detail) }

func approveTool(readOnly bool, action, detail string) bool {
	if mode.autoApprove(readOnly) {
		return true
	}
	if jsonOut != nil {
		approvalID++
		emit(event{Kind: "approval_request", ID: approvalID, Name: action, Text: detail})
		return awaitApproval(approvalID)
	}
	fmt.Print("  " + paint(cWrite, "allow?") + dim(" [y/N/a=always] "))
	if !stdin.Scan() {
		return false
	}
	switch strings.ToLower(cleanInput(stdin.Text())) {
	case "y", "yes":
		return true
	case "a", "always":
		mode = modeDeveloper
		note(cOK, "✓", "Developer mode on for this session")
		return true
	}
	note(cErr, "✗", "skipped")
	return false
}
