package core

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommandCWDCapturesAdmissionDirectory(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindCommandCWD("")
	if err != nil || bound != before {
		t.Fatalf("bound=%q error=%v, want %q", bound, err, before)
	}
	// Run this test binary as a harmless child. A different executor cwd
	// must not change where a command with the captured Dir starts.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(before); err != nil {
			t.Error(err)
		}
	})
	cmd := exec.Command(executable, "-test.run=^TestCommandCWDChild$")
	cmd.Dir = bound
	cmd.Env = append(os.Environ(), "SLB_TEST_EXPECT_CWD="+bound)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("captured directory was not used: %v\n%s", err, output)
	}
}

func TestCommandCWDChild(t *testing.T) {
	want := os.Getenv("SLB_TEST_EXPECT_CWD")
	if want == "" {
		return
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Getwd may use a different spelling of the same directory on macOS.
	a, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(want)
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("cwd=%q, want %q (%v)", got, want, err)
	}
}

func TestCommandCWDRejectsUnboundContext(t *testing.T) {
	invalid := []string{".", "..", "project", filepath.Join("project", "child"), "~", "$HOME", "\x00", t.TempDir() + "\x00suffix"}
	if runtime.GOOS == "windows" {
		invalid = append(invalid, `C:project`, `\project`)
	}
	for _, cwd := range invalid {
		t.Run(strings.ReplaceAll(cwd, "\x00", "NUL"), func(t *testing.T) {
			if err := validateCommandCWD(cwd); !errors.Is(err, ErrUnboundCommandCWD) {
				t.Fatalf("stored cwd %q accepted: %v", cwd, err)
			}
			if got, err := bindCommandCWD(cwd); got != "" || !errors.Is(err, ErrUnboundCommandCWD) {
				t.Fatalf("input cwd %q rebound to %q: %v", cwd, got, err)
			}
		})
	}
	if err := validateCommandCWD(""); !errors.Is(err, ErrUnboundCommandCWD) {
		t.Fatalf("stored empty cwd was accepted: %v", err)
	}
}

func TestCommandCWDPreservesAbsoluteSpelling(t *testing.T) {
	base := t.TempDir()
	sep := string(filepath.Separator)
	for _, cwd := range []string{base, base + sep + "link" + sep + "..", base + sep + ".", base + sep + "space and 'quote"} {
		got, err := bindCommandCWD(cwd)
		if err != nil || got != cwd {
			t.Errorf("cwd %q changed to %q: %v", cwd, got, err)
		}
	}
}
