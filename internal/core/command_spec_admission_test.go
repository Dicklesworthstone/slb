package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestCommandSpecAdmissionRejectsForeignProjectBeforeSafeShortcut(t *testing.T) {
	for _, shell := range []bool{false, true} {
		for _, command := range []string{"git stash", "git reset --hard HEAD~1"} {
			t.Run(fmt.Sprintf("shell=%t/%s", shell, command), func(t *testing.T) {
				database, original, session := policyExecutionFixture(t)
				cfg := DefaultRequestCreatorConfig()
				cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
				creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
				for _, project := range []string{t.TempDir(), original.ProjectPath + string(filepath.Separator) + "."} {
					var before, after int
					if err := database.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&before); err != nil {
						t.Fatal(err)
					}
					result, err := creator.CreateRequest(CreateRequestOptions{
						SessionID: session.ID, Command: command, Shell: shell,
						Cwd: original.ProjectPath, ProjectPath: project,
					})
					if result != nil || !errors.Is(err, db.ErrRequestAdmissionIdentity) {
						t.Fatalf("foreign scope acquired admission or a SAFE result: %+v %v", result, err)
					}
					if err := database.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&after); err != nil {
						t.Fatal(err)
					}
					if after != before {
						t.Fatalf("scope rejection inserted a request: %d -> %d", before, after)
					}
				}
			})
		}
	}
}

func TestCommandSpecAdmissionRejectsForeignProjectBeforePreview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX preview marker script")
	}
	for _, shell := range []bool{false, true} {
		t.Run(fmt.Sprintf("shell=%t", shell), func(t *testing.T) {
			database, original, session := policyExecutionFixture(t)
			bin := t.TempDir()
			marker := filepath.Join(bin, "preview-ran")
			// Only the generated preview could reach this harmless fake Git.
			// The requested reset is never executed by RequestCreator.
			script := "#!/bin/sh\nprintf preview > '" + strings.ReplaceAll(marker, "'", `'\''`) + "'\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := DefaultRequestCreatorConfig()
			cfg.EnableDryRun, cfg.AgentMailEnabled = true, false
			creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
			result, err := creator.CreateRequest(CreateRequestOptions{
				SessionID: session.ID, Command: "git reset --hard HEAD~1", Shell: shell,
				Cwd: original.ProjectPath, ProjectPath: t.TempDir(), RequireDryRun: true,
			})
			if result != nil || !errors.Is(err, db.ErrRequestAdmissionIdentity) {
				t.Fatalf("foreign scope was not rejected: %+v %v", result, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("rejected scope ran an external preview: %v", err)
			}
		})
	}
}

func TestCommandSpecAdmissionRejectsUnscopedSession(t *testing.T) {
	for _, query := range []string{
		`UPDATE sessions SET project_path = '' WHERE id = ?`,
		`UPDATE sessions SET agent_name = '' WHERE id = ?`,
	} {
		t.Run(query, func(t *testing.T) {
			database, original, session := policyExecutionFixture(t)
			if _, err := database.Exec(query, session.ID); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultRequestCreatorConfig()
			cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
			creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
			result, err := creator.CreateRequest(CreateRequestOptions{
				SessionID: session.ID, Command: "git stash", Cwd: original.ProjectPath,
			})
			if result != nil || !errors.Is(err, db.ErrRequestAdmissionIdentity) {
				t.Fatalf("unscoped session acquired a SAFE result: %+v %v", result, err)
			}
		})
	}
}

func TestCommandSpecAdmissionPreservesOwnProject(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, command := range []string{"git stash", "git reset --hard HEAD~1"} {
			t.Run(fmt.Sprintf("explicit=%t/%s", explicit, command), func(t *testing.T) {
				database, original, session := policyExecutionFixture(t)
				cfg := DefaultRequestCreatorConfig()
				cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
				creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
				project := ""
				if explicit {
					project = original.ProjectPath
				}
				result, err := creator.CreateRequest(CreateRequestOptions{
					SessionID: session.ID, Command: command, Cwd: original.ProjectPath, ProjectPath: project,
				})
				if err != nil || result == nil {
					t.Fatalf("own project rejected: %+v %v", result, err)
				}
				if command == "git stash" {
					if !result.Skipped || result.Request != nil {
						t.Fatalf("SAFE behavior changed: %+v", result)
					}
				} else if result.Request == nil || result.Skipped || result.Request.ProjectPath != original.ProjectPath ||
					result.Request.RiskTier != RiskTierDangerous || result.Request.MinApprovals != 1 {
					t.Fatalf("own-project scope or quorum changed: %+v", result)
				}
			})
		}
	}
}

func TestCommandSpecAdmissionBindsWorkingDirectory(t *testing.T) {
	database, _, session := policyExecutionFixture(t)
	cfg := DefaultRequestCreatorConfig()
	cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
	creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	result, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID, Command: "printf cwd-probe",
		Justification: Justification{Reason: "bind ambient cwd before review"},
	})
	if err != nil || result == nil || result.Request == nil {
		t.Fatalf("admission: %+v %v", result, err)
	}
	stored, err := database.GetRequest(result.Request.ID)
	if err != nil || stored.Command.Cwd != cwd || stored.Command.Hash != db.ComputeCommandHash(stored.Command) {
		t.Fatalf("cwd not included in durable command specification: %+v %v", stored, err)
	}
	// Even a SAFE command may not bypass validation of explicit context.
	for _, input := range []string{".", "relative", cwd + "\x00"} {
		result, err := creator.CreateRequest(CreateRequestOptions{
			SessionID: session.ID, Command: "git stash", Cwd: input,
		})
		if result != nil || !errors.Is(err, ErrUnboundCommandCWD) {
			t.Fatalf("SAFE shortcut accepted ambiguous cwd %q: %+v %v", input, result, err)
		}
	}
}

func TestCommandSpecAdmissionMissingCWDUsesHomeRootQuorum(t *testing.T) {
	database, original, session := policyExecutionFixture(t)
	t.Setenv("HOME", original.ProjectPath)
	t.Setenv("USERPROFILE", original.ProjectPath)
	t.Chdir(original.ProjectPath)
	cfg := DefaultRequestCreatorConfig()
	cfg.EnableDryRun, cfg.AgentMailEnabled = false, false
	creator := NewRequestCreator(database, nil, NewPatternEngine(), cfg)
	// These strings only enter classification/admission, never an executor.
	for _, tc := range []struct {
		command string
		tier    RiskTier
		quorum  int
	}{
		{"rm -rf .", RiskTierCritical, 2},
		{"rm -rf build", RiskTierDangerous, 1},
		{"rm -rf ./build", RiskTierDangerous, 1},
		{"rm -f notes.txt", RiskTierCaution, 0},
	} {
		result, err := creator.CreateRequest(CreateRequestOptions{
			SessionID: session.ID, Command: tc.command,
			Justification: Justification{Reason: "test cwd-dependent approval requirements"},
		})
		if err != nil || result == nil || result.Request == nil {
			t.Fatalf("%q: %+v %v", tc.command, result, err)
		}
		if result.Request.RiskTier != tc.tier || result.Request.MinApprovals != tc.quorum {
			t.Fatalf("%q: tier=%s quorum=%d, want %s/%d", tc.command,
				result.Request.RiskTier, result.Request.MinApprovals, tc.tier, tc.quorum)
		}
	}
}

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
