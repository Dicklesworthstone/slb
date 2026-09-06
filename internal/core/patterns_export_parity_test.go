package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The builtin patterns are shared with the Python fallback classifier
// emitted by ExportClaudeHook, so every builtin must be valid under
// Python's `re` and classify the same way there as it does in Go.
// This test runs the exported module under python3 (skipped when no
// interpreter is available) and compares tiers for the boundary cases
// from issue #11 and its sibling fixes. Plain single commands only:
// the Go side normalizes wrappers/quotes and the Python side does not.
func TestExportClaudeHook_ParityWithGoClassifier(t *testing.T) {
	python := findPython3(t)

	engine := NewPatternEngine()
	hookPath := filepath.Join(t.TempDir(), "hook.py")
	if err := os.WriteFile(hookPath, []byte(engine.ExportClaudeHook()), 0o600); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	commands := []string{
		"chmod 777 /etc/passwd",
		"chmod 777 '/etc/passwd'",
		`chmod 777 "/etc/passwd"`,
		"chmod +x /usr/bin/x",
		"chmod 4755 /sbin/x",
		"chown -R www-data /var/www",
		"chown root: /bin/sh",
		"chmod +x /path/to/project/bin/mytool",
		"chmod +x /path/to/project/binary",
		"chmod +x ./bin/x",
		"chmod +x /home/u/project/bin/x",
		"chmod +x /opt/app/binary",
		"chmod 644 /data/etc-configs/x",
		"chmod -R 755 ./dist",
		"rm -rf /etc",
		"rm -rf /homework",
		"rm -rf / foo.log",
		"rm app.log",
		"rm -f a.log b.log",
		"rm important.db app.log",
		"git push -f origin main",
		"git push -fu origin main",
		"git push origin fix-f",
		"git push --force-with-lease origin main",
		"gcloud projects undelete p --quiet",
		"gcloud projects delete p --quiet",
		"git status",
	}

	// Python prints "<tier>\t<command>" per line; map Go tiers onto the
	// same vocabulary ("" -> unknown, RiskSafe -> safe).
	script := `import importlib.util, sys
spec = importlib.util.spec_from_file_location("hook", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
for line in sys.stdin.read().split("\n"):
    if line:
        print(m.classify(line)[0] + "\t" + line)
`
	cmd := exec.Command(python, "-c", script, hookPath)
	cmd.Stdin = strings.NewReader(strings.Join(commands, "\n") + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 failed (exported hook is not valid Python?): %v\n%s", err, out)
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		tier, command, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("unexpected python output line %q", line)
		}
		got[command] = tier
	}

	for _, command := range commands {
		res := engine.ClassifyCommand(command, "")
		want := string(res.Tier)
		switch res.Tier {
		case "":
			want = "unknown"
		case RiskTier(RiskSafe):
			want = "safe"
		}
		if got[command] != want {
			t.Errorf("%q: python=%q go=%q (go pattern %q)", command, got[command], want, res.MatchedPattern)
		}
	}
	if len(got) != len(commands) {
		t.Errorf("python classified %d commands, want %d", len(got), len(commands))
	}
}

// findPython3 returns a working Python 3 interpreter or skips the test.
// A bare LookPath is not enough: some platforms ship a `python3` stub
// that only prints install instructions.
func findPython3(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		probe := exec.Command(path, "-c", "import sys; sys.exit(0 if sys.version_info[0] == 3 else 1)")
		if probe.Run() == nil {
			return path
		}
	}
	t.Skip("no working python3 interpreter; skipping Go/Python parity check")
	return ""
}
