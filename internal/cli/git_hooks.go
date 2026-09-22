package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	gitutil "github.com/Dicklesworthstone/slb/internal/git"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newGitHooksCmd(), newGitCheckCmd())
	hookCmd.AddCommand(newNativeGitHookCmd("pre-commit"), newNativeGitHookCmd("pre-push"))
}

func newGitHooksCmd() *cobra.Command {
	command := &cobra.Command{Use: "git-hooks", Short: "Install native Git approval hooks", Long: `Protect staged deletions/type changes and destructive or protected-ref pushes.
Uses Git's effective hooks directory, including worktrees and core.hooksPath.
Existing foreign hooks are never overwritten. Uninstall preserves disabled backups.
Git must be able to find slb on PATH; missing SLB blocks the operation.

These hooks do not intercept checkout, reset or clean. Client-side hooks are not
an access-control boundary. Run 'slb hook install' for Claude Code interception.`}
	for _, action := range []string{"install", "status", "uninstall"} {
		action := action
		var names []string
		child := &cobra.Command{Use: action, Short: action + " native Git hooks", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := os.Getwd()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			var statuses []gitutil.HookStatus
			switch action {
			case "install":
				statuses, err = gitutil.InstallNativeHooks(ctx, repo, names)
			case "status":
				statuses, err = gitutil.NativeHookStatus(ctx, repo, names)
			case "uninstall":
				statuses, err = gitutil.UninstallNativeHooks(ctx, repo, names)
			}
			if err != nil {
				return err
			}
			return writeGitHookOutput(cmd, statuses)
		}}
		child.Flags().StringSliceVar(&names, "hooks", []string{"pre-commit", "pre-push"}, "selected native Git hooks")
		command.AddCommand(child)
	}
	// This token can appear in a reviewed request, but never releases a hook
	// merely by being run through the regular command executor.
	command.AddCommand(&cobra.Command{Use: "authorize", Hidden: true, RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("Git authorization tokens are consumed only by the native hook; retry the original Git operation, not slb execute")
	}})
	return command
}

func newGitCheckCmd() *cobra.Command {
	var operation, remote, location, reason string
	var request, exitCode, newRequest bool
	command := &cobra.Command{Use: "git-check", Short: "Inspect a native Git operation without running it", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		assessment, revalidate, err := assessNativeGit(ctx, operation, remote, location, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if newRequest && !request {
			return errors.New("--new-request requires --request")
		}
		if request {
			decision, err := processNativeGitHook(ctx, assessment, revalidate, false, newRequest, reason)
			if err != nil {
				return err
			}
			return writeGitHookOutput(cmd, decision)
		}
		if err := writeGitHookOutput(cmd, assessment); err != nil {
			return err
		}
		if exitCode && assessment.RequiresApproval {
			return errors.New("Git operation requires approval")
		}
		return nil
	}}
	command.Flags().StringVar(&operation, "operation", "commit", "commit or push (push reads Git's pre-push protocol from stdin)")
	command.Flags().StringVar(&remote, "remote", "", "pre-push remote name")
	command.Flags().StringVar(&location, "location", "", "pre-push remote location (stored only as a hash)")
	command.Flags().StringVar(&reason, "reason", "", "rationale for a requested Git authorization")
	command.Flags().BoolVar(&request, "request", false, "submit or show approval for this exact snapshot without consuming it")
	command.Flags().BoolVar(&newRequest, "new-request", false, "explicitly submit a new review after rejection or expiry")
	command.Flags().BoolVar(&exitCode, "exit-code", false, "exit nonzero when the inspected operation needs approval")
	return command
}

func newNativeGitHookCmd(operation string) *cobra.Command {
	command := &cobra.Command{Use: operation, Short: "Native Git " + operation + " authorization gate", Args: cobra.NoArgs}
	if operation == "pre-push" {
		command.Args = cobra.ExactArgs(2)
		command.Use += " <remote> <location>"
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		remote, location := "", ""
		if len(args) == 2 {
			remote, location = args[0], args[1]
		}
		assessment, revalidate, err := assessNativeGit(ctx, operation, remote, location, cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("SLB: unable to assess Git operation; blocked: %w", err)
		}
		decision, err := processNativeGitHook(ctx, assessment, revalidate, true, os.Getenv("SLB_GIT_NEW_REQUEST") == "1", os.Getenv("SLB_GIT_REASON"))
		if err != nil {
			return fmt.Errorf("SLB: Git operation blocked: %w", err)
		}
		if !decision.Allowed {
			return fmt.Errorf("SLB: %s (request %s). Review with 'slb review %s'; approve from an independent session, then retry Git. Do not use slb execute on this token", decision.Message, decision.RequestID, decision.RequestID)
		}
		if decision.RequestID != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "SLB: consumed Git hook approval %s; Git's eventual outcome is not recorded by this hook.\n", decision.RequestID)
		}
		return nil // Native hooks never emit machine protocol on Git's stdout.
	}
	return command
}

func writeGitHookOutput(cmd *cobra.Command, value any) error {
	format := GetOutput()
	if format == "text" || format == "json" || format == "" {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
	}
	return output.New(output.Format(format)).Write(value)
}

func assessNativeGit(ctx context.Context, operation, remote, location string, input io.Reader) (*gitutil.GitAssessment, func(context.Context) error, error) {
	repo, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	var assess func(context.Context) (*gitutil.GitAssessment, error)
	switch operation {
	case "commit", "pre-commit":
		assess = func(ctx context.Context) (*gitutil.GitAssessment, error) { return gitutil.AssessCommit(ctx, repo) }
	case "push", "pre-push":
		if input == nil {
			return nil, nil, errors.New("pre-push stdin is required")
		}
		protocol, err := io.ReadAll(io.LimitReader(input, 1024*1024+1))
		if err != nil {
			return nil, nil, err
		}
		if len(protocol) > 1024*1024 {
			return nil, nil, errors.New("pre-push input exceeds 1 MiB")
		}
		assess = func(ctx context.Context) (*gitutil.GitAssessment, error) {
			protected, err := gitutil.NativeProtectedBranches(ctx, repo)
			if err != nil {
				return nil, err
			}
			return gitutil.AssessPush(ctx, repo, remote, location, bytes.NewReader(protocol), protected)
		}
	default:
		return nil, nil, errors.New("operation must be commit or push")
	}
	assessment, err := assess(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Recheck captured input/index under the approval claim's writer reservation.
	revalidate := func(ctx context.Context) error {
		current, err := assess(ctx)
		if err != nil {
			return err
		}
		if current.Snapshot != assessment.Snapshot {
			return errors.New("Git snapshot changed; review the new operation")
		}
		return nil
	}
	return assessment, revalidate, nil
}

type gitHookDecision struct {
	Allowed    bool                   `json:"allowed"`
	RequestID  string                 `json:"request_id,omitempty"`
	Status     db.RequestStatus       `json:"status,omitempty"`
	Message    string                 `json:"message"`
	Assessment *gitutil.GitAssessment `json:"assessment"`
}

func nativeGitSession(conn *db.DB, project string) (*db.Session, error) {
	id := flagSessionID
	if id == "" {
		id = os.Getenv("SLB_SESSION_ID")
	}
	if id != "" {
		session, err := conn.GetSession(id)
		if err != nil {
			return nil, err
		}
		if !session.IsActive() || session.ProjectPath != project {
			return nil, db.ErrExecutionAuthentication
		}
		return session, nil
	}
	actor := flagActor
	if actor == "" {
		actor = os.Getenv("AGENT_NAME")
	}
	if actor == "" {
		actor = os.Getenv("SLB_ACTOR")
	}
	if actor != "" {
		sessions, err := conn.ListActiveSessions(project)
		if err != nil {
			return nil, err
		}
		var found *db.Session
		for _, session := range sessions {
			if session.AgentName == actor {
				if found != nil {
					return nil, errors.New("multiple active agent sessions; set SLB_SESSION_ID explicitly")
				}
				found = session
			}
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, errors.New("start an SLB session and export SLB_SESSION_ID, or set AGENT_NAME to an existing active project agent")
}

func processNativeGitHook(ctx context.Context, assessment *gitutil.GitAssessment, revalidate func(context.Context) error, consume, newRequest bool, reason string) (*gitHookDecision, error) {
	decision := &gitHookDecision{Assessment: assessment, Allowed: !assessment.RequiresApproval, Message: "no native Git risk detected"}
	if decision.Allowed {
		return decision, nil
	}
	path := filepath.Join(assessment.Repository, ".slb", "state.db")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("repository SLB database unavailable; run slb init in the worktree first")
	}
	if flagDB != "" {
		absolute, err := filepath.Abs(flagDB)
		if err != nil {
			return nil, err
		}
		if absolute != path {
			return nil, errors.New("native Git hooks require this worktree's .slb/state.db; a different --db is not allowed")
		}
	}
	conn, err := db.OpenAndMigrate(path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	session, err := nativeGitSession(conn, assessment.Repository)
	if err != nil {
		return nil, err
	}
	options := config.LoadOptions{ProjectDir: assessment.Repository, ConfigPath: flagConfig}
	cfg, err := config.Load(options)
	if err != nil {
		return nil, err
	}
	engine, err := core.LoadCommandPolicy(conn, options)
	if err != nil {
		return nil, err
	}
	evidence, err := json.Marshal(assessment)
	if err != nil {
		return nil, err
	}
	intent := core.GitHookIntent{Operation: assessment.Operation, ProjectPath: assessment.Repository, Snapshot: assessment.Snapshot, Evidence: string(evidence)}
	var request *db.Request
	if !newRequest {
		var id string
		err := conn.QueryRow(`SELECT id FROM requests WHERE project_path = ? AND command_raw = ? AND command_cwd = ? AND requestor_session_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, assessment.Repository, intent.CommandSpec().Raw, assessment.Repository, session.ID).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			request, err = conn.GetRequest(id)
			if err != nil {
				return nil, err
			}
		}
	}
	if request == nil || request.Status == db.StatusExecuted || request.Status == db.StatusExecutionFailed {
		creator := core.NewRequestCreator(conn, core.NewRateLimiter(conn, toRateLimitConfig(cfg)), engine, toRequestCreatorConfig(cfg))
		request, err = creator.CreateGitHookRequest(ctx, intent, session.ID, reason)
		if err != nil {
			return nil, err
		}
	}
	decision.RequestID = request.ID
	decision.Status = request.Status
	decision.Message = "awaiting independent approval"
	if request.Status != db.StatusApproved {
		if request.Status != db.StatusPending {
			decision.Message = "prior request is " + string(request.Status) + "; explicitly resubmit with SLB_GIT_NEW_REQUEST=1 and a rationale"
		}
		if request.ExpiresAt != nil && time.Now().After(*request.ExpiresAt) {
			decision.Message = "request expired; explicitly resubmit with SLB_GIT_NEW_REQUEST=1"
		}
		return decision, nil
	}
	if !consume {
		decision.Message = "approval recorded; the native hook will recheck current policy and snapshot before consuming it"
		return decision, nil
	}
	requestID := request.ID
	request, err = core.NewExecutor(conn, engine).AuthorizeGitHook(ctx, intent, requestID, session.ID, revalidate)
	if err != nil {
		return nil, fmt.Errorf("approval %s cannot authorize this snapshot: %w; submit a fresh review with SLB_GIT_NEW_REQUEST=1 if expired or superseded", requestID, err)
	}
	decision.Allowed = true
	decision.Status = request.Status
	decision.Message = "Git hook authorized; Git outcome not observed"
	return decision, nil
}
