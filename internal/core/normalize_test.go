package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathsInCommand(t *testing.T) {
	cwd := filepath.Join(string(os.PathSeparator), "tmp", "slb-test-cwd")

	t.Run("expands ./ paths", func(t *testing.T) {
		out := ResolvePathsInCommand("rm -rf ./build", cwd)
		want := filepath.Join(cwd, "build")
		if !strings.Contains(out, want) {
			t.Fatalf("output %q does not contain %q", out, want)
		}
	})

	t.Run("expands ../ paths", func(t *testing.T) {
		out := ResolvePathsInCommand("rm -rf ../secrets", cwd)
		want := filepath.Clean(filepath.Join(cwd, "..", "secrets"))
		if !strings.Contains(out, want) {
			t.Fatalf("output %q does not contain %q", out, want)
		}
	})

	t.Run("expands ~/ paths even when cwd empty", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			t.Skip("no home directory available")
		}

		out := ResolvePathsInCommand("rm -rf ~/build", "")
		want := filepath.Join(home, "build")
		if !strings.Contains(out, want) {
			t.Fatalf("output %q does not contain %q", out, want)
		}
	})
}

func TestNormalizeCommandEdgeCases(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		res := NormalizeCommand("")
		if res.Primary != "" || len(res.Segments) != 0 || res.IsCompound {
			t.Fatalf("got Primary=%q Segments=%v IsCompound=%v", res.Primary, res.Segments, res.IsCompound)
		}
	})

	t.Run("whitespace only", func(t *testing.T) {
		res := NormalizeCommand("   \t  ")
		if res.Primary != "" || len(res.Segments) != 0 || res.IsCompound {
			t.Fatalf("got Primary=%q Segments=%v IsCompound=%v", res.Primary, res.Segments, res.IsCompound)
		}
	})

	t.Run("very long command does not panic", func(t *testing.T) {
		long := "echo " + strings.Repeat("a", 10_000)
		res := NormalizeCommand(long)
		if res.Original == "" {
			t.Fatalf("expected Original to be set")
		}
	})

	t.Run("subshell detection", func(t *testing.T) {
		res := NormalizeCommand("echo $(rm -rf /tmp)")
		if !res.HasSubshell {
			t.Fatalf("expected HasSubshell=true")
		}
	})
}

func TestNormalizeCommandEnvAssignments(t *testing.T) {
	res := NormalizeCommand("env FOO=bar BAR=baz kubectl delete pod nginx-123")
	if res.Primary != "kubectl delete pod nginx-123" {
		t.Fatalf("Primary=%q, want %q", res.Primary, "kubectl delete pod nginx-123")
	}
	if len(res.StrippedWrappers) == 0 || res.StrippedWrappers[0] != "env" {
		t.Fatalf("StrippedWrappers=%v, want prefix [env ...]", res.StrippedWrappers)
	}
}

func TestExtractCommandName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple command", "ls -la", "ls"},
		{"full path", "/usr/bin/git status", "git"},
		{"relative path", "./script.sh --help", "script.sh"},
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"command with leading whitespace", "  rm -rf /tmp", "rm"},
		{"command with many args", "docker run -it --rm ubuntu bash", "docker"},
		{"path without spaces", "/opt/local/bin/python3 script.py", "python3"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ExtractCommandName(tc.input)
			if result != tc.expected {
				t.Errorf("ExtractCommandName(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestExtractXargsCommand(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "xargs with rm command",
			input:    "xargs rm -rf",
			expected: "rm -rf",
		},
		{
			name:     "xargs with complex command",
			input:    "xargs kubectl delete pod",
			expected: "kubectl delete pod",
		},
		{
			name:     "xargs with single command",
			input:    "xargs echo",
			expected: "echo",
		},
		{
			name:     "xargs with flags",
			input:    "xargs -0 rm -rf",
			expected: "-0 rm -rf",
		},
		{
			name:     "xargs with -I replacement",
			input:    "xargs -I {} mv {} /tmp",
			expected: "-I {} mv {} /tmp",
		},
		{
			name:     "not xargs returns empty",
			input:    "rm -rf /tmp",
			expected: "",
		},
		{
			name:     "empty string returns empty",
			input:    "",
			expected: "",
		},
		{
			name:     "just xargs returns empty (no command after)",
			input:    "xargs",
			expected: "",
		},
		{
			name:     "xargs with extra spaces",
			input:    "xargs   rm   -rf",
			expected: "rm   -rf",
		},
		{
			name:     "xargs in middle of pipe",
			input:    "find . -name '*.log' | xargs rm",
			expected: "rm",
		},
		{
			name:     "grep with xargs in string matches (regex limitation)",
			input:    "grep xargs file.txt",
			expected: "file.txt", // Note: regex matches "xargs" anywhere in string
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ExtractXargsCommand(tc.input)
			if result != tc.expected {
				t.Errorf("ExtractXargsCommand(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestNormalizeSegmentShellC(t *testing.T) {
	tests := []struct {
		name              string
		input             string
		wantNormalized    string
		wantParseError    bool
		wantWrapperPrefix string
	}{
		{
			name:              "bash -c with single quotes",
			input:             "bash -c 'rm -rf /tmp'",
			wantNormalized:    "rm -rf /tmp",
			wantParseError:    false,
			wantWrapperPrefix: "bash -c",
		},
		{
			name:              "sh -c with double quotes",
			input:             `sh -c "echo hello"`,
			wantNormalized:    "echo hello",
			wantParseError:    false,
			wantWrapperPrefix: "sh -c",
		},
		{
			name:              "zsh -c nested sudo",
			input:             "zsh -c 'sudo rm -rf /var/log'",
			wantNormalized:    "rm -rf /var/log",
			wantParseError:    false,
			wantWrapperPrefix: "zsh -c",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			normalized, wrappers, parseErr := normalizeSegment(tc.input)
			if normalized != tc.wantNormalized {
				t.Errorf("normalized = %q, want %q", normalized, tc.wantNormalized)
			}
			if parseErr != tc.wantParseError {
				t.Errorf("parseErr = %v, want %v", parseErr, tc.wantParseError)
			}
			if len(wrappers) == 0 || wrappers[0] != tc.wantWrapperPrefix {
				t.Errorf("wrappers = %v, want prefix %q", wrappers, tc.wantWrapperPrefix)
			}
		})
	}
}

func TestNormalizeSegmentWrapperStripping(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		wantNormalized string
		wantWrappers   []string
	}{
		{
			name:           "sudo prefix",
			input:          "sudo rm -rf /tmp",
			wantNormalized: "rm -rf /tmp",
			wantWrappers:   []string{"sudo"},
		},
		{
			name:           "multiple wrappers",
			input:          "sudo nice ionice rm -rf /tmp",
			wantNormalized: "rm -rf /tmp",
			wantWrappers:   []string{"sudo", "nice", "ionice"},
		},
		{
			name:           "doas prefix",
			input:          "doas kubectl delete pod nginx",
			wantNormalized: "kubectl delete pod nginx",
			wantWrappers:   []string{"doas"},
		},
		{
			name:           "time prefix",
			input:          "time make build",
			wantNormalized: "make build",
			wantWrappers:   []string{"time"},
		},
		{
			name:           "nohup prefix",
			input:          "nohup ./long-running-script.sh",
			wantNormalized: "./long-running-script.sh",
			wantWrappers:   []string{"nohup"},
		},
		{
			name:           "no wrapper",
			input:          "ls -la",
			wantNormalized: "ls -la",
			wantWrappers:   []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			normalized, wrappers, _ := normalizeSegment(tc.input)
			if normalized != tc.wantNormalized {
				t.Errorf("normalized = %q, want %q", normalized, tc.wantNormalized)
			}
			if len(wrappers) != len(tc.wantWrappers) {
				t.Errorf("wrappers = %v, want %v", wrappers, tc.wantWrappers)
			}
			for i, w := range tc.wantWrappers {
				if i >= len(wrappers) || wrappers[i] != w {
					t.Errorf("wrappers[%d] = %q, want %q", i, wrappers[i], w)
				}
			}
		})
	}
}

func TestIsWrapper(t *testing.T) {
	wrappers := []string{"sudo", "doas", "env", "command", "builtin", "time", "nice", "ionice", "nohup", "strace", "ltrace"}
	for _, w := range wrappers {
		if !isWrapper(w) {
			t.Errorf("isWrapper(%q) = false, want true", w)
		}
	}

	nonWrappers := []string{"rm", "kubectl", "docker", "git", "make", ""}
	for _, w := range nonWrappers {
		if isWrapper(w) {
			t.Errorf("isWrapper(%q) = true, want false", w)
		}
	}
}

func TestIsEnvAssignment(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"FOO=bar", true},
		{"_VAR=value", true},
		{"VAR123=test", true},
		{"A=b", true},
		{"=value", false},
		{"123VAR=test", false},
		{"FOO", false},
		{"foo bar", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := isEnvAssignment(tc.input)
			if got != tc.want {
				t.Errorf("isEnvAssignment(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestSplitCompoundShellAware(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "simple &&",
			input:    "echo foo && rm -rf /tmp",
			expected: []string{"echo foo", "rm -rf /tmp"},
		},
		{
			name:     "simple semicolon",
			input:    "cd /tmp; rm -rf .",
			expected: []string{"cd /tmp", "rm -rf ."},
		},
		{
			name:     "simple ||",
			input:    "test -f foo || echo missing",
			expected: []string{"test -f foo", "echo missing"},
		},
		{
			name:     "background &",
			input:    "sleep 10 & echo started",
			expected: []string{"sleep 10", "echo started"},
		},
		{
			name:     "&& inside double quotes",
			input:    `echo "foo && bar"`,
			expected: []string{`echo "foo && bar"`},
		},
		{
			name:     "&& inside single quotes",
			input:    `echo 'foo && bar'`,
			expected: []string{`echo 'foo && bar'`},
		},
		{
			name:     "semicolon inside quotes",
			input:    `psql -c "DELETE FROM users; DROP TABLE users;"`,
			expected: []string{`psql -c "DELETE FROM users; DROP TABLE users;"`},
		},
		{
			name:     "mixed: quoted && and real &&",
			input:    `echo "foo && bar" && rm -rf /tmp`,
			expected: []string{`echo "foo && bar"`, "rm -rf /tmp"},
		},
		{
			name:     "escaped quote",
			input:    `echo "foo\"bar" && rm -rf /tmp`,
			expected: []string{`echo "foo\"bar"`, "rm -rf /tmp"},
		},
		{
			name:     "multiple segments",
			input:    "cd /tmp && rm -rf . && echo done",
			expected: []string{"cd /tmp", "rm -rf .", "echo done"},
		},
		{
			name:     "empty command",
			input:    "",
			expected: []string{},
		},
		{
			name:     "no separators",
			input:    "ls -la",
			expected: []string{"ls -la"},
		},
		{
			name:     "nested quotes",
			input:    `bash -c 'echo "hello && world"' && rm -rf /tmp`,
			expected: []string{`bash -c 'echo "hello && world"'`, "rm -rf /tmp"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := splitCompoundShellAware(tc.input)
			if len(result) != len(tc.expected) {
				t.Fatalf("got %d segments %v, want %d segments %v", len(result), result, len(tc.expected), tc.expected)
			}
			for i, seg := range result {
				if seg != tc.expected[i] {
					t.Errorf("segment[%d] = %q, want %q", i, seg, tc.expected[i])
				}
			}
		})
	}
}

func TestCompoundCommandWithQuotes(t *testing.T) {
	// This test verifies the security fix: commands with quotes should still
	// split properly when the && is OUTSIDE the quotes.
	t.Run("dangerous command hidden after quoted echo", func(t *testing.T) {
		res := NormalizeCommand(`echo "foo" && rm -rf /etc`)
		if !res.IsCompound {
			t.Fatalf("expected IsCompound=true, got false")
		}
		if len(res.Segments) != 2 {
			t.Fatalf("expected 2 segments, got %d: %v", len(res.Segments), res.Segments)
		}
		// The dangerous rm command should be extracted as a separate segment
		foundRm := false
		for _, seg := range res.Segments {
			if strings.HasPrefix(seg, "rm -rf") {
				foundRm = true
			}
		}
		if !foundRm {
			t.Fatalf("expected to find 'rm -rf' segment in %v", res.Segments)
		}
	})

	t.Run("SQL inside quotes not split", func(t *testing.T) {
		res := NormalizeCommand(`psql -c "DELETE FROM users; DROP TABLE users;"`)
		// Should not be split because semicolons are inside quotes
		if res.IsCompound {
			t.Fatalf("expected IsCompound=false for SQL in quotes, got true with segments: %v", res.Segments)
		}
	})
}
