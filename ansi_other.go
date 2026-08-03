//go:build !windows

package main

import (
	"os"
	"strconv"
)

// enableANSI is a no-op: every terminal that matters here already speaks VT.
func enableANSI() {}

// termWidth reads COLUMNS.
//
// ponytail: the real answer is a TIOCGWINSZ ioctl, which does not survive a
// terminal resize being missed anyway. Export COLUMNS, or swap this for
// golang.org/x/term if the fallback ever looks wrong.
func termWidth() int {
	if w, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && w > 20 {
		return w
	}
	return defaultWidth
}
