package config

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestDefaultRMHomeGlobProtection(t *testing.T) {
	// These are the production arrays, also exported to the offline hook.
	tiers := []struct {
		name     string
		patterns []string
	}{
		{"safe", defaultSafePatterns},
		{"critical", defaultCriticalPatterns},
		{"dangerous", defaultDangerousPatterns},
		{"caution", defaultCautionPatterns},
	}
	var compiled [][]*regexp.Regexp
	for _, tier := range tiers {
		var patterns []*regexp.Regexp
		for _, pattern := range tier.patterns {
			patterns = append(patterns, regexp.MustCompile("(?i)"+pattern))
		}
		compiled = append(compiled, patterns)
	}
	type example struct {
		Command string `json:"command"`
		Tier    string `json:"tier"`
	}
	var examples []example
	// Empty, dot and parent-of-root prefixes exercise filesystem-root wipes.
	// /?* and /[!.]* must not evade the protection of /*. Each corresponding
	// /build/<glob> remains project data, not a root-wide deletion.
	for _, home := range []string{"", "/.", "/..", "/home/ubuntu", "/Users/alice", "/root", "~", "~alice", "$HOME", "${HOME}"} {
		for _, glob := range []string{"*", "**", "**/*", ".*", "?*", "[!.]*"} {
			target := home + "/" + glob
			for _, flags := range []string{"-r", "-R", "-rf", "-fr", "-f -r", "--recursive --force"} {
				examples = append(examples,
					example{"rm " + flags + " " + target, "critical"},
					example{"rm " + flags + " -- build " + target, "critical"},
					example{"rm " + flags + " '" + target + "' build", "critical"},
				)
			}
			examples = append(examples,
				example{"rm -f " + target, "caution"},
				example{"rm -- -rf " + target, "caution"},
				example{"rm -rf " + home + "/build/" + glob, "dangerous"},
			)
		}
	}
	for _, command := range []string{`rm -rf "$HOME"`, `rm -rf "${HOME}"/*`, `rm -rf "$HOME"/**/*`} {
		examples = append(examples, example{command, "critical"})
	}
	for _, command := range []string{"rm -rf build", "rm -rf ./build", "rm -rf /home/ubuntu/build", "rm -rf /root/.cache/build",
		`rm -rf "$HOME/build"`, `rm -rf "${HOME}"/project/build`, `rm -rf $HOME_BACKUP`, `rm -rf ${HOME_BACKUP}`, `rm -rf ${HOME}suffix`} {
		examples = append(examples, example{command, "dangerous"})
	}
	for _, command := range []string{"rm -f /home/ubuntu/notes.txt", "rm -ff /etc/notes.txt"} {
		examples = append(examples, example{command, "caution"})
	}
	examples = append(examples, example{"rm -f app.log", "safe"}, example{"rm -rf /etc/ssh", "critical"})
	for _, ex := range examples {
		t.Run(ex.Command, func(t *testing.T) {
			got := ""
			for i, patterns := range compiled {
				for _, pattern := range patterns {
					if pattern.MatchString(ex.Command) {
						got = tiers[i].name
						break
					}
				}
				if got != "" {
					break
				}
			}
			if got != ex.Tier {
				t.Fatalf("got %q, want %q", got, ex.Tier)
			}
		})
	}
	t.Run("PythonRegexParity", func(t *testing.T) {
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 is not available")
		}
		patterns := make(map[string][]string)
		for _, tier := range tiers {
			patterns[tier.name] = tier.patterns
		}
		payload, err := json.Marshal(struct {
			Patterns map[string][]string `json:"patterns"`
			Examples []example           `json:"examples"`
		}{patterns, examples})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, "-c", `
import json, re, sys
payload = json.load(sys.stdin)
patterns = [(tier, [re.compile(p, re.I) for p in payload['patterns'][tier]])
            for tier in ('safe', 'critical', 'dangerous', 'caution')]
for ex in payload['examples']:
    got = next((tier for tier, rules in patterns
                if any(p.search(ex['command']) for p in rules)), '')
    assert got == ex['tier'], (ex, got)
`)
		cmd.Stdin = strings.NewReader(string(payload))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Python policy parity: %v\n%s", err, output)
		}
	})
}
