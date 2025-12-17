package core

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestGetDryRunCommand(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantOK    bool
		wantParts []string
	}{
		{
			name:      "kubectl delete adds dry-run",
			in:        "kubectl delete deployment foo",
			wantOK:    true,
			wantParts: []string{"kubectl", "delete", "--dry-run=client", "-o", "yaml"},
		},
		{
			name:      "kubectl delete keeps existing dry-run",
			in:        "kubectl delete deployment foo --dry-run=client",
			wantOK:    true,
			wantParts: []string{"kubectl", "delete", "--dry-run=client"},
		},
		{
			name:      "terraform destroy becomes plan -destroy",
			in:        "terraform destroy",
			wantOK:    true,
			wantParts: []string{"terraform", "plan", "-destroy"},
		},
		{
			name:      "rm becomes ls listing",
			in:        "rm -rf ./build",
			wantOK:    true,
			wantParts: []string{"ls", "-la", "./build"},
		},
		{
			name:      "git reset --hard becomes diff",
			in:        "git reset --hard HEAD~5",
			wantOK:    true,
			wantParts: []string{"git", "diff", "HEAD~5..HEAD"},
		},
		{
			name:      "helm uninstall becomes get manifest",
			in:        "helm uninstall myrelease",
			wantOK:    true,
			wantParts: []string{"helm", "get", "manifest", "myrelease"},
		},
		{
			name:      "wrapper stripping still detects kubectl",
			in:        "sudo kubectl delete pod nginx-123",
			wantOK:    true,
			wantParts: []string{"kubectl", "delete", "--dry-run=client"},
		},
		{
			name:   "unsupported command",
			in:     "echo hello",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := GetDryRunCommand(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v (out=%q)", ok, tt.wantOK, out)
			}
			if !ok {
				return
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(out, part) {
					t.Fatalf("output %q does not contain %q", out, part)
				}
			}
		})
	}
}

func TestRunCommand_StreamOptional(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell execution test uses /bin/sh or $SHELL")
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")

	spec := &db.CommandSpec{
		Raw:   "echo hi",
		Cwd:   dir,
		Shell: true,
	}

	// With stream writer, output should be written to it.
	var streamed bytes.Buffer
	res, err := RunCommand(context.Background(), spec, logPath, &streamed)
	if err != nil {
		t.Fatalf("RunCommand(streamed) error: %v", err)
	}
	if !strings.Contains(streamed.String(), "hi") {
		t.Fatalf("expected stream to contain command output, got %q", streamed.String())
	}
	if res == nil || !strings.Contains(res.Output, "hi") {
		t.Fatalf("expected captured output to contain command output, got %#v", res)
	}

	// With nil stream, command output should not be written to process stdout.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	_, err = RunCommand(context.Background(), spec, logPath, nil)
	_ = w.Close()
	os.Stdout = oldStdout

	b, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatalf("read stdout pipe: %v", readErr)
	}
	if len(bytes.TrimSpace(b)) != 0 {
		t.Fatalf("expected no stdout when stream is nil, got %q", string(b))
	}
	if err != nil {
		t.Fatalf("RunCommand(nil stream) error: %v", err)
	}
}

func TestCombineStdoutStderr(t *testing.T) {
	tests := []struct {
		name           string
		stdout         string
		stderr         string
		wantEmpty      bool
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:      "both empty",
			stdout:    "",
			stderr:    "",
			wantEmpty: true,
		},
		{
			name:         "only stdout",
			stdout:       "hello world",
			stderr:       "",
			wantContains: []string{"hello world"},
		},
		{
			name:         "only stderr",
			stdout:       "",
			stderr:       "error occurred",
			wantContains: []string{"error occurred"},
		},
		{
			name:         "both stdout and stderr",
			stdout:       "output line",
			stderr:       "error line",
			wantContains: []string{"output line", "--- stderr ---", "error line"},
		},
		{
			name:           "strips trailing newlines from stdout",
			stdout:         "hello\n\n\n",
			stderr:         "",
			wantContains:   []string{"hello"},
			wantNotContain: []string{"\n\n"},
		},
		{
			name:           "strips trailing newlines from stderr",
			stdout:         "",
			stderr:         "error\n\n",
			wantContains:   []string{"error"},
			wantNotContain: []string{"\n\n"},
		},
		{
			name:         "combined with trailing newlines stripped",
			stdout:       "out\n",
			stderr:       "err\n",
			wantContains: []string{"out", "err", "--- stderr ---"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := combineStdoutStderr(tc.stdout, tc.stderr)
			if tc.wantEmpty && result != "" {
				t.Errorf("expected empty result, got %q", result)
			}
			for _, s := range tc.wantContains {
				if !strings.Contains(result, s) {
					t.Errorf("result %q should contain %q", result, s)
				}
			}
			for _, s := range tc.wantNotContain {
				if strings.Contains(result, s) {
					t.Errorf("result %q should not contain %q", result, s)
				}
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty string", "", "''"},
		{"simple word", "hello", "hello"},
		{"word with no special chars", "filename.txt", "filename.txt"},
		{"word with space", "hello world", "'hello world'"},
		{"word with tab", "hello\tworld", "'hello\tworld'"},
		{"word with single quote", "it's", "'it'\\''s'"},
		{"word with double quote", `say "hi"`, `'say "hi"'`},
		{"word with dollar sign", "$HOME", "'$HOME'"},
		{"word with ampersand", "foo & bar", "'foo & bar'"},
		{"word with semicolon", "cmd1; cmd2", "'cmd1; cmd2'"},
		{"word with pipe", "cmd | grep", "'cmd | grep'"},
		{"word with asterisk", "*.txt", "'*.txt'"},
		{"word with parentheses", "foo(bar)", "'foo(bar)'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shellQuote(tc.in)
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		name   string
		tokens []string
		want   string
	}{
		{"empty slice", []string{}, ""},
		{"single token", []string{"hello"}, "hello"},
		{"multiple simple tokens", []string{"ls", "-la", "/tmp"}, "ls -la /tmp"},
		{"tokens with spaces", []string{"echo", "hello world"}, "echo 'hello world'"},
		{"mixed tokens", []string{"grep", "-r", "foo bar", "/path"}, "grep -r 'foo bar' /path"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shellJoin(tc.tokens)
			if got != tc.want {
				t.Errorf("shellJoin(%v) = %q, want %q", tc.tokens, got, tc.want)
			}
		})
	}
}
