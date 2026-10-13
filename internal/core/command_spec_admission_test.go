package core

import (
	"reflect"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestCommandSpecAdmissionRejectsPartialDirectInput(t *testing.T) {
	for _, command := range []string{
		"git stash; printf ignored", "git stash > ignored", "printf x | printf y",
		"printf x && printf y", "printf x & printf y", "printf 'unterminated",
		" \t", "'' operand", "printf \x00",
	} {
		t.Run(command, func(t *testing.T) {
			database, original, session := policyExecutionFixture(t)
			cfg := DefaultRequestCreatorConfig()
			cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
			creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
			var before, after int
			if err := database.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			result, err := creator.CreateRequest(CreateRequestOptions{
				SessionID: session.ID, Command: command, Cwd: original.ProjectPath,
				Justification: Justification{Reason: "test complete direct command parsing"},
			})
			if err == nil || result != nil {
				t.Fatalf("partial command reached admission or SAFE shortcut: result=%+v error=%v", result, err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("rejected input created a request: before=%d after=%d", before, after)
			}
		})
	}
}

func TestCommandSpecAdmissionClassifiesSelectedExecutionMode(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		shell     bool
		argv      []string
		tier      RiskTier
		quorum    int
	}{
		{
			"literal direct argument", `printf "%s" "$(printf command-spec-probe)"`, false,
			[]string{"printf", "%s", "$(printf command-spec-probe)"}, RiskTierDangerous, 1,
		},
		{
			"executed shell substitution", `printf "%s" "$(printf command-spec-probe)"`, true,
			[]string{"printf", "%s", "$(printf command-spec-probe)"}, RiskTierCritical, 2,
		},
		{
			"complete shell script", `printf first; printf command-spec-probe`, true,
			nil, RiskTierCritical, 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, original, session := policyExecutionFixture(t)
			engine := NewPatternEngine()
			if err := engine.AddPattern(RiskTierCritical, `^printf\s+command-spec-probe$`, "test script invocation", "human"); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultRequestCreatorConfig()
			// Even the regression path cannot launch a preview or notification.
			cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
			creator := NewRequestCreator(database, nil, engine, cfg)
			result, err := creator.CreateRequest(CreateRequestOptions{
				SessionID: session.ID, Command: tc.raw, Cwd: original.ProjectPath, Shell: tc.shell,
				Justification: Justification{Reason: "test authoritative command admission"},
			})
			if err != nil || result == nil || result.Request == nil || result.Skipped {
				t.Fatalf("admission failed: result=%+v error=%v", result, err)
			}
			request := result.Request
			if request.RiskTier != tc.tier || request.MinApprovals != tc.quorum {
				t.Fatalf("wrong invocation classified: tier=%s quorum=%d, want %s/%d", request.RiskTier, request.MinApprovals, tc.tier, tc.quorum)
			}
			if request.Command.Raw != tc.raw || request.Command.Shell != tc.shell || !reflect.DeepEqual(request.Command.Argv, tc.argv) {
				t.Fatalf("admission changed executable specification: %+v", request.Command)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Command.Hash == "" || stored.Command.Hash != db.ComputeCommandHash(stored.Command) {
				t.Fatalf("executable specification is not hash-bound: %+v error=%v", stored, err)
			}
		})
	}
}
