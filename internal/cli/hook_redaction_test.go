package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/core"
)

func TestOfflineAuditUsesDaemonRedactionRules(t *testing.T) {
	patterns, err := json.Marshal(core.RedactionPatterns())
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`git push --force token=topsecret`,
		`password="topsecret" git push --force`,
		`export AWS_ACCESS_KEY_ID=topsecret; git push --force`,
		`curl -H 'Authorization: Bearer topsecret' example.invalid`,
		`psql postgres://user:topsecret@localhost/database`,
	} {
		home := t.TempDir()
		input, err := json.Marshal(map[string]any{"tool_input": map[string]any{"command": command}})
		if err != nil {
			t.Fatal(err)
		}
		setup := "AUDIT_REDACTION_PATTERNS = " + string(patterns) + "\nquery_slb_daemon = lambda *args: None\nclassify = lambda command: ('dangerous', 1)"
		runHookRuntime(t, home, setup, string(input))
		events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
		if err != nil || len(events) != 1 {
			t.Fatalf("missing offline audit: %+v, %v", events, err)
		}
		if want := core.ApplyRedaction(command, nil); events[0].CommandRedacted != want {
			t.Errorf("offline/daemon privacy mismatch: got %q, want %q", events[0].CommandRedacted, want)
		}
	}
}
