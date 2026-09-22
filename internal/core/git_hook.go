package core

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
)

// GitHookIntent is an approval-only action. It must never execute a second Git
// command from inside a hook. The snapshot is computed from Git's index tree or
// complete pre-push ref-update protocol; Evidence is display-only context.
type GitHookIntent struct {
	Operation   string `json:"operation"`
	ProjectPath string `json:"project_path"`
	Snapshot    string `json:"snapshot"`
	Evidence    string `json:"evidence"`
}

func (intent GitHookIntent) validate() error {
	if intent.Operation != "pre-commit" && intent.Operation != "pre-push" {
		return errors.New("unsupported Git hook operation")
	}
	decoded, err := hex.DecodeString(intent.Snapshot)
	if err != nil || len(decoded) != 32 || strings.ToLower(intent.Snapshot) != intent.Snapshot {
		return errors.New("Git hook requires a canonical SHA-256 snapshot")
	}
	if !filepath.IsAbs(intent.ProjectPath) {
		return errors.New("Git hook project must be absolute")
	}
	return nil
}

// CommandSpec is a review token, not a shell command granting Git permission.
// Invoking it through slb execute deliberately does not release a Git hook.
func (intent GitHookIntent) CommandSpec() db.CommandSpec {
	argv := []string{"slb", "git-hooks", "authorize", intent.Operation, intent.Snapshot}
	spec := db.CommandSpec{Raw: strings.Join(argv, " "), Argv: argv, Cwd: intent.ProjectPath, Shell: false}
	spec.Hash = db.ComputeCommandHash(spec)
	return spec
}

func (intent GitHookIntent) riskFloor() db.RiskTier {
	if intent.Operation == "pre-push" {
		return db.RiskTierCritical
	}
	return db.RiskTierDangerous
}

func (intent GitHookIntent) effectiveTier(engine *PatternEngine) db.RiskTier {
	tier := intent.riskFloor()
	classification := engine.ClassifyCommand(intent.CommandSpec().Raw, intent.ProjectPath)
	if tierHigher(classification.Tier, tier) {
		tier = classification.Tier
	}
	return tier
}

// CreateGitHookRequest uses the same atomic quota admission as ordinary
// requests, but SAFE patterns cannot exempt externally detected Git damage.
// No preflight subprocess is appropriate: the Git assessment IS the preview.
func (rc *RequestCreator) CreateGitHookRequest(ctx context.Context, intent GitHookIntent, sessionID, reason string) (*db.Request, error) {
	if err := intent.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := rc.db.GetSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("getting Git hook session: %w", err)
	}
	if !session.IsActive() {
		return nil, ErrSessionInactive
	}
	if session.ProjectPath != intent.ProjectPath {
		return nil, db.ErrExecutionAuthentication
	}
	if rc.isAgentBlocked(session.AgentName) {
		return nil, ErrAgentBlocked
	}
	available, err := CountPolicyReviewers(rc.db, intent.ProjectPath, session.ID, session.AgentName)
	if err != nil {
		return nil, err
	}
	tier := intent.effectiveTier(rc.patternEngine)
	expiry := time.Now().UTC().Add(rc.requestLifetime())
	if strings.TrimSpace(reason) == "" {
		reason = "Native Git hook detected a review-required change. No operator rationale was supplied; review the exact snapshot before approving."
	}
	request := &db.Request{
		ProjectPath: intent.ProjectPath, Command: intent.CommandSpec(), RiskTier: tier,
		RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Status: db.StatusPending, MinApprovals: max(1, rc.patternEngine.RequiredApprovals(tier, available)),
		RequireDifferentModel: rc.patternEngine.RequiresDifferentModel(), ExpiresAt: &expiry,
		Justification: db.Justification{
			Reason: ApplyRedaction(reason, nil), ExpectedEffect: intent.Evidence,
			Goal:           "Authorize exactly one native Git hook invocation for this snapshot.",
			SafetyArgument: "Approval permits the hook to return, not proof that Git succeeded. Changed snapshots require a new review. Retry Git after approval; do not run slb execute on this token.",
		},
	}
	if _, err := rc.rateLimiter.AdmitRequest(ctx, request); err != nil {
		return nil, err
	}
	notifier := rc.notifier
	if rc.config.AgentMailEnabled {
		notifier = integrations.NewAgentMailClient(intent.ProjectPath, rc.config.AgentMailThread, rc.config.AgentMailSender)
	}
	_ = notifier.NotifyNewRequest(request)
	return request, nil
}

func (intent GitHookIntent) checkRequest(request *db.Request, sessionID string) error {
	expected := intent.CommandSpec()
	if request.ProjectPath != intent.ProjectPath || request.RequestorSessionID != sessionID ||
		request.Command.Raw != expected.Raw || request.Command.Cwd != expected.Cwd || request.Command.Shell ||
		request.Command.Hash != expected.Hash || db.ComputeCommandHash(request.Command) != expected.Hash {
		return ErrCommandHashMismatch
	}
	return nil
}

func (intent GitHookIntent) checkPolicy(reader PolicyReader, request *db.Request, configPath string) error {
	engine, err := LoadCommandPolicy(reader, config.LoadOptions{ProjectDir: intent.ProjectPath, ConfigPath: configPath})
	if err != nil {
		return err
	}
	tier := intent.effectiveTier(engine)
	if tierHigher(tier, request.RiskTier) {
		return ErrTierEscalated
	}
	available, err := CountPolicyReviewers(reader, intent.ProjectPath, request.RequestorSessionID, request.RequestorAgent)
	if err != nil {
		return err
	}
	// An allowlist match cannot short-circuit the native Git review floor.
	if request.MinApprovals < max(1, engine.RequiredApprovals(tier, available)) || (engine.RequiresDifferentModel() && !request.RequireDifferentModel) {
		return ErrApprovalPolicyChanged
	}
	return nil
}

// AuthorizeGitHook consumes signed approval exactly once, without running a
// subprocess. revalidate must compare the live Git snapshot just before the
// claim commits. Failed revalidation rolls the claim back. Git can still fail
// after a successful hook; the recorded outcome is ONLY hook authorization.
func (e *Executor) AuthorizeGitHook(ctx context.Context, intent GitHookIntent, requestID, sessionID string, revalidate func(context.Context) error) (*db.Request, error) {
	if err := intent.validate(); err != nil {
		return nil, err
	}
	if revalidate == nil {
		return nil, errors.New("Git snapshot revalidation is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, err := e.db.VerifyRequestApproval(requestID)
	if err != nil {
		return nil, err
	}
	if err := intent.checkRequest(request, sessionID); err != nil {
		return nil, err
	}
	session, err := e.db.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if !session.IsActive() || session.ProjectPath != intent.ProjectPath {
		return nil, db.ErrExecutionAuthentication
	}
	if err := intent.checkPolicy(e.db, request, e.configPath); err != nil {
		return nil, err
	}
	logPath, err := e.createLogFile(filepath.Join(intent.ProjectPath, ".slb", "logs"), requestID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// A prepared receipt is not a permit: only successful durable completion
	// below allows the CLI to return zero to Git.
	receipt, err := json.Marshal(map[string]any{"event": "git_hook_authorization", "phase": "prepared", "request_id": requestID, "session_id": sessionID, "intent": intent, "prepared_at": now, "git_outcome": "not_observed"})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(logPath, append(receipt, '\n'), 0600); err != nil {
		return nil, err
	}
	execution := &db.Execution{ExecutedAt: &now, ExecutedBySessionID: session.ID, ExecutedByAgent: session.AgentName, ExecutedByModel: session.Model, LogPath: logPath}
	guard := func(tx *sql.Tx, current *db.Request) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := intent.checkRequest(current, sessionID); err != nil {
			return err
		}
		if err := intent.checkPolicy(tx, current, e.configPath); err != nil {
			return err
		}
		if err := revalidate(ctx); err != nil {
			return err
		}
		return ctx.Err()
	}
	if err := e.db.ClaimRequestExecution(request, execution, guard); err != nil {
		return nil, err
	}
	// Completion marks the approval-only action as consumed, not git commit or
	// git push as successful. Failure here is ambiguous and therefore blocks Git.
	zero := 0
	elapsed := time.Since(now).Milliseconds()
	execution.ExitCode = &zero
	execution.DurationMs = &elapsed
	if err := e.db.CompleteRequestExecution(requestID, db.StatusExecuted, execution); err != nil {
		return nil, fmt.Errorf("recording Git hook authorization: %w", err)
	}
	request.Status = db.StatusExecuted
	request.Execution = execution
	return request, nil
}
