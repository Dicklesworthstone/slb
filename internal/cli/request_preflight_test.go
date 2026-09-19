package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/spf13/cobra"
)

func preflightCLICommand(required bool, budget time.Duration) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("require-dry-run", required, "test")
	cmd.Flags().Duration("dry-run-timeout", budget, "test")
	return cmd
}

func TestCLIRequestPreflightFlagsAndValidation(t *testing.T) {
	for _, cmd := range []*cobra.Command{requestCmd, runCmd} {
		if cmd.Flags().Lookup("require-dry-run") == nil || cmd.Flags().Lookup("dry-run-timeout") == nil || !strings.Contains(cmd.Long, "general.enable_dry_run") {
			t.Fatal("missing preflight controls or semantics")
		}
	}
	for _, budget := range []time.Duration{-time.Second, 31 * time.Second} {
		cmd := preflightCLICommand(false, budget)
		if finish, err := beginRequestCommand(cmd); err == nil || finish != nil || cmd.Context() != nil {
			t.Fatalf("invalid preview budget %v passed early validation", budget)
		}
	}
	cmd := preflightCLICommand(false, 0)
	opts := core.CreateRequestOptions{RequireDryRun: true, DryRunTimeout: 7 * time.Second}
	got, err := requestPreflightFlags(cmd, opts)
	if err != nil || !got.RequireDryRun || got.DryRunTimeout != opts.DryRunTimeout {
		t.Fatalf("default flags weakened explicit caller options: %+v %v", got, err)
	}
	if err := cmd.Flags().Set("dry-run-timeout", "15ms"); err != nil {
		t.Fatal(err)
	}
	got, err = requestPreflightFlags(cmd, opts)
	if err != nil || got.DryRunTimeout != 15*time.Millisecond || !got.RequireDryRun {
		t.Fatalf("flag override ignored: %+v %v", got, err)
	}
}

func TestCLIRequestPreflightConfigAndPersistence(t *testing.T) {
	if _, err := exec.LookPath("ls"); err != nil {
		t.Skip("requires ls preview tool")
	}
	for _, tc := range []struct {
		name              string
		enabled, required bool
		want              string
	}{
		{"enabled", true, true, "succeeded"},
		{"disabled", false, false, "disabled"},
		{"disabled-required", false, true, "preflight_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, database, opts := cliAdmissionFixture(t, core.RateLimitActionReject)
			cfg := config.DefaultConfig()
			cfg.General.EnableDryRun = tc.enabled
			cfg.Integrations.AgentMailEnabled = false
			adapted := toRequestCreatorConfig(cfg)
			if adapted.EnableDryRun != tc.enabled {
				t.Fatal("CLI configuration adapter dropped enable_dry_run")
			}
			creator := core.NewRequestCreator(database, nil, core.NewPatternEngine(), adapted)
			target := filepath.Join(opts.ProjectPath, "keep-me")
			if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			opts.Command, opts.Shell = "rm -rf ./keep-me", true
			cmd := preflightCLICommand(tc.required, time.Second)
			finish, err := beginRequestCommand(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			result, err := submitRequestWithCapacity(cmd, creator, opts)
			if tc.want == "preflight_required" {
				if !errors.Is(err, core.ErrPreflightRequired) || result != nil {
					t.Fatalf("disabled required preview admitted: %+v %v", result, err)
				}
				response := requestAdmissionFailureResponse(err)
				if response["code"] != tc.want || response["created"] != false || response["executed"] != false || response["request_id"] != nil {
					t.Fatalf("invalid preflight failure response: %+v", response)
				}
				count, err := database.CountPendingBySession(opts.SessionID)
				if err != nil || count != 0 {
					t.Fatalf("required preflight failed but request exists: %d %v", count, err)
				}
				return
			}
			if err != nil || result.Preflight == nil || result.Preflight.Status != tc.want {
				t.Fatalf("incorrect preflight result: %+v %v", result, err)
			}
			response := map[string]any{"request_id": result.Request.ID}
			addRequestAdmissionMetadata(response, result)
			data, err := json.Marshal(response)
			if err != nil || !strings.Contains(string(data), `"preflight"`) || !strings.Contains(string(data), `"command_hash":"`+result.Request.Command.Hash+`"`) {
				t.Fatalf("preflight report missing from structured response: %s %v", data, err)
			}
			var attachments string
			if err := database.QueryRow(`SELECT attachments_json FROM requests WHERE id=?`, result.Request.ID).Scan(&attachments); err != nil || !strings.Contains(attachments, tc.want) {
				t.Fatalf("preflight not available to reviewers: %s %v", attachments, err)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
				t.Fatal("submission executed the original command")
			}
		})
	}
}

func TestCLIRequestPreflightTimeoutsDoNotBecomeApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX preview fixture")
	}
	for _, mode := range []string{"advisory-timeout", "required-timeout", "admission-timeout", "caller-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			creator, database, opts := cliAdmissionFixture(t, core.RateLimitActionReject)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ls"), []byte("#!/bin/sh\nsleep 5\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd := preflightCLICommand(mode == "required-timeout", 50*time.Millisecond)
			if mode == "admission-timeout" {
				if err := cmd.Flags().Set("dry-run-timeout", "1s"); err != nil {
					t.Fatal(err)
				}
				cmd.Flags().Duration("queue-timeout", 25*time.Millisecond, "test")
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd.SetContext(parent)
			finish, err := beginRequestCommand(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if mode == "caller-cancelled" {
				cancel()
			}
			result, err := submitRequestWithCapacity(cmd, creator, opts)
			wantCount := 0
			if mode == "advisory-timeout" {
				if err != nil || result.Preflight.Status != "timed_out" || string(result.Request.Status) != "pending" {
					t.Fatalf("preview timeout became execution/approval: %+v %v", result, err)
				}
				wantCount = 1
			} else {
				if err == nil || result != nil {
					t.Fatalf("failed preflight admitted: %+v %v", result, err)
				}
				want := map[string]string{"required-timeout": "preflight_required", "admission-timeout": "admission_timeout", "caller-cancelled": "admission_cancelled"}[mode]
				if response := requestAdmissionFailureResponse(err); response["code"] != want {
					t.Fatalf("wrong failure classification: %+v", response)
				}
			}
			count, err := database.CountPendingBySession(opts.SessionID)
			if err != nil || count != wantCount {
				t.Fatalf("unexpected admitted count: %d want=%d err=%v", count, wantCount, err)
			}
			if mode != "caller-cancelled" && cmd.Context().Err() != nil {
				t.Fatal("preflight timeout leaked into the command context")
			}
		})
	}
}

func TestCLIRequestPreflightErrorsRemainTyped(t *testing.T) {
	err := fmt.Errorf("collecting preview: %w", core.ErrPreflightRequired)
	response := requestAdmissionFailureResponse(err)
	if response["code"] != "preflight_required" || response["status"] != "request_failed" || response["created"] != false {
		t.Fatal(response)
	}
}
