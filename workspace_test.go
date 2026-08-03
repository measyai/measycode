package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A path typed into a prompt arrives with whatever the platform and the
// terminal added to it. Rejecting any of these would make /switch feel
// broken in exactly the moment it is supposed to feel effortless.
func TestExpandPathAcceptsWhatPeopleType(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	t.Run("tilde", func(t *testing.T) {
		got, err := expandPath("~")
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Clean(home) {
			t.Errorf("expandPath(~) = %q, want %q", got, home)
		}
	})

	t.Run("tilde with child", func(t *testing.T) {
		got, err := expandPath("~/projects")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, "projects"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Dragging a folder onto a terminal wraps it in quotes on every OS.
	t.Run("quoted", func(t *testing.T) {
		got, err := expandPath(`"` + home + `"`)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Clean(home) {
			t.Errorf("quoted path survived as %q", got)
		}
	})

	t.Run("environment variable", func(t *testing.T) {
		t.Setenv("MEASY_TEST_DIR", home)
		var raw string
		if runtime.GOOS == "windows" {
			raw = "%MEASY_TEST_DIR%"
		} else {
			raw = "$MEASY_TEST_DIR"
		}
		got, err := expandPath(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Clean(home) {
			t.Errorf("expandPath(%q) = %q, want %q", raw, got, home)
		}
	})

	t.Run("empty is an error", func(t *testing.T) {
		if _, err := expandPath("   "); err == nil {
			t.Error("an empty path was accepted")
		}
	})
}

// "C:" alone means "the current directory on drive C" to Windows, which is
// never what someone typing it into a workspace prompt means.
func TestExpandPathTreatsABareDriveAsItsRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only spelling")
	}
	got, err := expandPath("C:")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, `C:\`) {
		t.Errorf(`expandPath("C:") = %q, want C:\`, got)
	}
}

// An unknown %VAR% is left alone rather than deleted: a visibly wrong path
// is far easier to diagnose than a silently truncated one.
func TestExpandPathKeepsUnknownWindowsVariables(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only spelling")
	}
	os.Unsetenv("MEASY_DEFINITELY_UNSET")
	got := expandWindowsVars(`%MEASY_DEFINITELY_UNSET%\x`)
	if !strings.Contains(got, "MEASY_DEFINITELY_UNSET") {
		t.Errorf("an unknown variable was silently dropped: %q", got)
	}
}

func TestSetWorkspaceRejectsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-folder.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setWorkspace(file); err == nil {
		t.Error("a file was accepted as a workspace")
	}
	if _, err := setWorkspace(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing folder was accepted as a workspace")
	}
}

// The three modes differ only in what runs unattended, and the line is
// reversibility: reads leave nothing behind, writes do.
func TestApprovalModesGateOnReversibility(t *testing.T) {
	cases := []struct {
		mode           approvalMode
		read, mutating bool
	}{
		{modeSafe, false, false},
		{modeBalanced, true, false},
		{modeDeveloper, true, true},
	}
	for _, tc := range cases {
		if got := tc.mode.autoApprove(true); got != tc.read {
			t.Errorf("%s: read auto-approve = %v, want %v", tc.mode, got, tc.read)
		}
		if got := tc.mode.autoApprove(false); got != tc.mutating {
			t.Errorf("%s: mutating auto-approve = %v, want %v", tc.mode, got, tc.mutating)
		}
	}
}

func TestParseApprovalMode(t *testing.T) {
	for _, in := range []string{"1", "safe", "SAFE", " Safe "} {
		if m, ok := parseApprovalMode(in); !ok || m != modeSafe {
			t.Errorf("parseApprovalMode(%q) = %v, %v", in, m, ok)
		}
	}
	if _, ok := parseApprovalMode("yolo"); ok {
		t.Error("an unknown mode was accepted")
	}
}
