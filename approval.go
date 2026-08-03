package main

import (
	"fmt"
	"strings"
)

// How much the agent may do without asking.
//
// A single on/off switch forces a bad choice: confirm every read of every
// file, or hand over the workspace wholesale. Almost everyone wants the
// middle — let it look around freely, ask before it changes anything.
//
// The distinction that matters is not "dangerous" but **reversible**.
// Reading a file, listing a directory, running `git status` leave nothing
// behind. Writing, deleting and running arbitrary commands do. Balanced
// draws the line exactly there.

type approvalMode int

const (
	// modeSafe asks before every tool call, including reads.
	modeSafe approvalMode = iota
	// modeBalanced runs read-only tools unattended and asks before any
	// change. The default, and the one to reach for.
	modeBalanced
	// modeDeveloper never asks. For a scratch directory or a container.
	modeDeveloper
)

var approvalNames = map[approvalMode]string{
	modeSafe:      "Safe",
	modeBalanced:  "Balanced",
	modeDeveloper: "Developer",
}

var approvalBlurbs = map[approvalMode]string{
	modeSafe:      "Ask before every action, including reads",
	modeBalanced:  "Read freely · ask before any change",
	modeDeveloper: "Full workspace access · never asks",
}

// mode is session-wide because the approval prompt itself can change it
// ("a" = always), as can /auto and /approval. Threading a pointer through
// every tool to express one switch buys nothing.
var mode = modeBalanced

func (m approvalMode) String() string { return approvalNames[m] }

// developerMode is what the JSON-lines protocol's boolean `auto` flag means.
// The desktop app has no three-way picker yet, so it sees the two ends.
func developerMode() bool { return mode == modeDeveloper }

// autoApprove reports whether a tool runs unattended.
//
// readOnly is the tool's own claim about itself, which is why the mapping
// lives here rather than being re-derived from the tool name at each call
// site — one table, one place to be wrong.
func (m approvalMode) autoApprove(readOnly bool) bool {
	switch m {
	case modeDeveloper:
		return true
	case modeBalanced:
		return readOnly
	default:
		return false
	}
}

func parseApprovalMode(s string) (approvalMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "safe":
		return modeSafe, true
	case "2", "balanced":
		return modeBalanced, true
	case "3", "developer", "dev", "full":
		return modeDeveloper, true
	}
	return 0, false
}

// modeLabel is the one-line summary in the banner.
func modeLabel() string {
	label := mode.String()
	if mode == modeDeveloper {
		return paint(cErr, label) + dim("  ·  "+approvalBlurbs[mode])
	}
	return label + dim("  ·  "+approvalBlurbs[mode])
}

// chooseApprovalMode is /approval: show the three, take a number.
func chooseApprovalMode(arg string) {
	if picked, ok := parseApprovalMode(arg); ok {
		setApprovalMode(picked)
		return
	}

	say("")
	say("  " + paint(cUser, "Approval Mode"))
	say("")
	for _, m := range []approvalMode{modeSafe, modeBalanced, modeDeveloper} {
		marker := "  "
		if m == mode {
			marker = paint(cOK, "● ")
		}
		say("  " + marker + paint(cUser, fmt.Sprint(int(m)+1)) + "  " + m.String())
		say("       " + dim(approvalBlurbs[m]))
	}
	say("")
	fmt.Print("  " + paint(cUser, "›") + " ")
	if !stdin.Scan() {
		return
	}

	choice := cleanInput(stdin.Text())
	if choice == "" {
		return
	}
	picked, ok := parseApprovalMode(choice)
	if !ok {
		say("  " + dim("1, 2 or 3"))
		return
	}
	setApprovalMode(picked)
}

func setApprovalMode(m approvalMode) {
	mode = m
	say("  " + paint(cOK, "✓") + "  " + modeLabel())
	if m == modeDeveloper {
		say("  " + dim("     the agent can now write and run anything in this folder"))
	}
}

// toggleApproval keeps /auto meaningful: it flips between the everyday mode
// and the unattended one, rather than cycling through all three.
func toggleApproval() {
	if mode == modeDeveloper {
		setApprovalMode(modeBalanced)
		return
	}
	setApprovalMode(modeDeveloper)
}
