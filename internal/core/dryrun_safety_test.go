package core

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestDryRunKubectlCannotDisablePreview(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"default", []string{"pod", "demo"}},
		{"none", []string{"pod", "demo", "--dry-run=none"}},
		{"false", []string{"--dry-run=false", "pod", "demo"}},
		{"server", []string{"--dry-run=server", "pod", "demo"}},
		{"bare", []string{"--dry-run", "pod", "demo"}},
		{"separate none", []string{"--dry-run", "none", "pod", "demo"}},
		{"separate false", []string{"pod", "demo", "--dry-run", "false"}},
		{"duplicate override", []string{"--dry-run=client", "pod", "demo", "--dry-run=none"}},
		{"terminator", []string{"--", "pod", "demo"}},
		{"positional mode", []string{"pod", "--", "--dry-run=none"}},
		{"dangling value flag", []string{"pod", "demo", "--namespace"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]string{"kubectl", "delete"}, tt.args...)
			original := append([]string(nil), input...)
			got, ok := dryRunKubectl(input)
			if !ok || len(got) < 3 || got[2] != "--dry-run=client" {
				t.Fatalf("preview must enforce client mode before all user arguments: %v, %v", got, ok)
			}
			modes := 0
			for _, arg := range got[2:] {
				if arg == "--" {
					break
				}
				if strings.HasPrefix(arg, "--dry-run") {
					modes++
					if arg != "--dry-run=client" {
						t.Fatalf("unsafe override remains: %v", got)
					}
				}
			}
			if modes != 1 {
				t.Fatalf("expected exactly one enforced mode, got %v", got)
			}
			if !reflect.DeepEqual(input, original) {
				t.Fatalf("preview mutated requested argv: %v", input)
			}
			if boundary := indexOfTerminator(input); boundary >= 0 {
				outputBoundary := indexOfTerminator(got)
				if outputBoundary < 0 || !reflect.DeepEqual(input[boundary:], got[outputBoundary:]) {
					t.Fatalf("operands changed after --: input=%v output=%v", input, got)
				}
			}
		})
	}
}

func indexOfTerminator(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i
		}
	}
	return -1
}

func TestDryRunKubectlRejectsRawDeletion(t *testing.T) {
	for _, args := range [][]string{
		{"--raw=/api/v1/namespaces/demo"},
		{"--raw", "/api/v1/namespaces/demo"},
		{"pod", "demo", "--dry-run-unknown"},
	} {
		if got, ok := dryRunKubectl(append([]string{"kubectl", "delete"}, args...)); ok {
			t.Errorf("unsafe/ambiguous preview accepted: %v", got)
		}
	}
}

func TestDryRunKubectlPreservesOutputAndScope(t *testing.T) {
	for _, output := range [][]string{{"-o", "json"}, {"-o=json"}, {"--output", "json"}, {"--output=json"}} {
		args := append([]string{"kubectl", "delete", "pod", "demo", "--namespace", "staging", "--context=test"}, output...)
		got, ok := dryRunKubectl(args)
		want := append([]string{"kubectl", "delete", "--dry-run=client"}, args[2:]...)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("scope/output changed: got=%v want=%v", got, want)
		}
	}
}

func TestDryRunRejectsIncompleteCommandDescriptions(t *testing.T) {
	for _, raw := range []string{
		`kubectl delete pod demo; echo done`,
		`kubectl delete pod demo && echo done`,
		`kubectl delete pod demo | cat`,
		`kubectl delete pod "unterminated`,
		`kubectl delete pod $(printf demo)`,
	} {
		if got, ok := GetDryRunCommand(raw); ok {
			t.Errorf("partial/ambiguous preview for %q: %q", raw, got)
		}
	}
}

func TestRunDryRunEnforcesClientModeAtProcessBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test executable is a POSIX shell script")
	}
	dir := t.TempDir()
	// The fake kubectl only records argv. No cluster or destructive command is used.
	tool := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, raw := range []string{
		"kubectl delete pod demo --dry-run=none",
		"kubectl delete -- pod demo",
	} {
		result, err := RunDryRun(&db.CommandSpec{Raw: raw, Cwd: dir})
		if err != nil || result == nil {
			t.Fatalf("RunDryRun(%q): result=%v err=%v", raw, result, err)
		}
		if !strings.HasPrefix(result.Output, "delete\n--dry-run=client\n") {
			t.Fatalf("actual process did not receive the enforced mode first: %q", result.Output)
		}
	}
}
