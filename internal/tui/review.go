package tui

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
	tea "github.com/charmbracelet/bubbletea"
)

// Capture options and the displayed command before starting a worker. A worker
// must never read the mutable Bubble Tea model, or navigate a subsequently opened
// request when its result arrives. SQLite, not UI eligibility, decides authority.
func (m *Model) submitReview(requestID string, decision db.Decision, comments string) tea.Cmd {
	opts := m.options
	commandHash := ""
	if m.detail != nil && m.detail.Request != nil && m.detail.Request.ID == requestID {
		commandHash = m.detail.Request.Command.Hash
	}
	return func() tea.Msg {
		return submitInteractiveReview(opts, requestID, commandHash, decision, comments)
	}
}

func (m *Model) approveRequest(requestID, comments string) tea.Cmd {
	return m.submitReview(requestID, db.DecisionApprove, comments)
}

func (m *Model) rejectRequest(requestID, reason string) tea.Cmd {
	return m.submitReview(requestID, db.DecisionReject, reason)
}

func submitInteractiveReview(opts Options, id, commandHash string, decision db.Decision, comments string) request.ReviewSubmittedMsg {
	result := request.ReviewSubmittedMsg{RequestID: id, Decision: decision}
	fail := func(err error) request.ReviewSubmittedMsg {
		result.Err = err
		return result
	}
	if opts.SessionID == "" || opts.SessionKey == "" {
		return fail(errors.New("review requires --session-id and --session-key; dashboard is read-only"))
	}
	if commandHash == "" {
		return fail(errors.New("reload the request before reviewing: displayed command hash is missing"))
	}
	database, err := db.OpenWithOptions(filepath.Join(opts.ProjectPath, ".slb", "state.db"), db.OpenOptions{})
	if err != nil {
		return fail(fmt.Errorf("opening review database: %w", err))
	}
	defer database.Close()
	target, err := database.GetRequest(id)
	if err != nil {
		return fail(fmt.Errorf("loading review request: %w", err))
	}
	if target.ProjectPath != opts.ProjectPath {
		return fail(errors.New("request belongs to another project"))
	}
	cfg, err := config.Load(config.LoadOptions{ProjectDir: target.ProjectPath, ConfigPath: opts.ConfigPath})
	if err != nil {
		return fail(fmt.Errorf("loading review policy: %w", err))
	}
	policy, err := interactiveReviewPolicy(cfg)
	if err != nil {
		return fail(err)
	}
	policy.ExpectedCommandHash = commandHash
	policy.ExpectedProjectPath = target.ProjectPath
	review := &db.Review{RequestID: id, ReviewerSessionID: opts.SessionID, Decision: decision, Comments: comments}
	outcome, err := database.ApplyReview(review, opts.SessionKey, policy)
	if err != nil {
		return fail(fmt.Errorf("review was not recorded: %w", err))
	}
	// Report the commit even if a later refresh or best-effort notification
	// fails. Retrying a successful review is not a recovery mechanism.
	result.Recorded = true
	result.Request = outcome.Request
	result.Review = outcome.Review
	result.Approvals, result.Rejections = outcome.Approvals, outcome.Rejections
	if cfg.Integrations.AgentMailEnabled {
		notifier := integrations.NewAgentMailClient(target.ProjectPath, cfg.Integrations.AgentMailThread, "")
		if decision == db.DecisionApprove {
			_ = notifier.NotifyRequestApproved(outcome.Request, outcome.Review)
		} else {
			_ = notifier.NotifyRequestRejected(outcome.Request, outcome.Review)
		}
	}
	return result
}

func interactiveReviewPolicy(cfg config.Config) (db.ReviewPolicy, error) {
	policy := db.ReviewPolicy{
		ConflictResolution: cfg.General.ConflictResolution,
		TrustedSelfApprove: cfg.Agents.TrustedSelfApprove,
	}
	for _, value := range []struct {
		name   string
		value  int
		unit   time.Duration
		target *time.Duration
	}{
		{"trusted self-approve delay", cfg.Agents.TrustedSelfApproveDelaySecs, time.Second, &policy.TrustedSelfApproveDelay},
		{"approval TTL", cfg.General.ApprovalTTLMins, time.Minute, &policy.ApprovalTTL},
		{"critical approval TTL", cfg.General.ApprovalTTLCriticalMins, time.Minute, &policy.CriticalApprovalTTL},
	} {
		if value.value < 0 || int64(value.value) > int64((1<<63-1)/value.unit) {
			return db.ReviewPolicy{}, fmt.Errorf("invalid %s", value.name)
		}
		*value.target = time.Duration(value.value) * value.unit
	}
	return policy, nil
}
