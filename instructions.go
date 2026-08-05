package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Project instructions.
//
// A coding agent dropped into an unfamiliar repository asks the same questions
// every session: how do I build this, what style does the team use, what must
// I never touch. Those answers do not belong in the harness — they belong to
// the project, written down where any contributor (human or agent) can find
// them. measycode looks for that file in three spellings, first match wins:
//
//	MEASY.md          this tool's own name
//	AGENTS.md         the cross-agent convention (Codex, Claude Code, …)
//	.measycode/instructions.md
//
// Keeping the files in the repo — not in ~/.measycode — is deliberate: they
// travel with the code, land in review like any other change, and never leak
// into a different project the way a home-level file would.

const (
	// maxInstructions caps a single file. Longer files are truncated with a
	// marker rather than silently swallowing the model's context window.
	maxInstructions = 20000
)

// instructionFiles are the spellings searched, in priority order.
var instructionFiles = []string{
	"MEASY.md",
	"AGENTS.md",
	filepath.Join(".measycode", "instructions.md"),
}

// findInstructions reads the first matching instruction file under dir and
// returns its path and contents. Absence is the ordinary case, not an error.
func findInstructions(dir string) (path, contents string, err error) {
	for _, name := range instructionFiles {
		p := filepath.Join(dir, name)
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			continue // missing or unreadable: try the next spelling
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		if len(text) > maxInstructions {
			text = text[:maxInstructions] + "\n\n[...truncated: instruction file exceeds " +
				fmt.Sprint(maxInstructions) + " characters...]"
		}
		return p, text, nil
	}
	return "", "", nil
}

// instructionsBlock renders the project-instruction section of the system
// prompt. Empty when the project has no instructions, so the prompt stays
// exactly as it was before this feature existed.
func instructionsBlock(dir string) string {
	path, text, err := findInstructions(dir)
	if err != nil || text == "" {
		return ""
	}
	return fmt.Sprintf(`
Project instructions (from %s — these are the repository's own rules; follow them unless they conflict with staying inside the working directory):

%s`, filepath.Base(path), text)
}
