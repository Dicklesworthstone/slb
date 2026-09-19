package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// PendingOptions controls which timer decisions an entry point may make.
type PendingOptions struct {
	ConfigPath      string
	TimeoutAction   string // Empty uses the current project configuration.
	OnlyAutoApprove bool   // Watch mode must not change unrelated timeout policy.
	ExpiredOnly     bool   // An explicit timeout operation must not approve early.
}

// AdvancePendingRequest applies due policy without running the command. CLI
// waiters and the daemon use this same path, so CAUTION works without a daemon.
// Every invocation rereads the request, rules, reviews and requester under a
// writer reservation. Notifications belong after the returned committed change.
func AdvancePendingRequest(ctx context.Context, database *db.DB, id string, opts PendingOptions) (*db.PendingResolution, error) {
	return database.ResolvePendingRequest(ctx, id, func(tx *sql.Tx, request *db.Request, now time.Time) (*db.PendingDecision, error) {
		expired := request.ExpiresAt != nil && !now.Before(*request.ExpiresAt)
		if (opts.ExpiredOnly && !expired) || (opts.OnlyAutoApprove && expired) {
			return nil, nil
		}
		engine, cfg, err := loadCommandPolicy(tx, config.LoadOptions{ProjectDir: request.ProjectPath, ConfigPath: opts.ConfigPath})
		if err != nil {
			return nil, err // Broken policy never creates permission.
		}
		action := opts.TimeoutAction
		if action == "" {
			action = cfg.General.TimeoutAction
		}
		if action != "escalate" && action != "auto_reject" && action != "auto_approve_warn" {
			return nil, fmt.Errorf("invalid timeout action %q", action)
		}
		if expired && action != "auto_approve_warn" {
			return timeoutDecision(action), nil
		}

		// A stored different-model requirement is part of the request's review
		// contract. Escalate for human attention when its wait expires instead
		// of leaving the request indefinitely pending. The DB rechecks reviewer
		// availability under the same writer reservation as the transition.
		if request.RequireDifferentModel && !opts.OnlyAutoApprove {
			modelTimeout, err := pendingDuration(cfg.General.DifferentModelTimeoutSecs, time.Second)
			if err != nil || modelTimeout == 0 {
				return nil, errors.New("different-model review requires a positive, representable timeout")
			}
			if !request.CreatedAt.IsZero() && !request.CreatedAt.After(now) &&
				now.Sub(request.CreatedAt) >= modelTimeout {
				return &db.PendingDecision{Status: db.StatusEscalated, DifferentModelTimeout: modelTimeout}, nil
			}
		}

		eligible := request.RiskTier == db.RiskTierCaution && request.MinApprovals == 0 && !request.RequireDifferentModel
		classification := engine.ClassifyCommand(request.Command.Raw, request.Command.Cwd)
		eligible = eligible && !classification.ParseError && !classification.HasUnmatchedSegment &&
			(classification.Tier == RiskTierCaution || classification.IsSafe) &&
			engine.RequiredApprovals(RiskTierCaution, -1) == 0 && !engine.RequiresDifferentModel()
		for _, blocked := range cfg.Agents.Blocked {
			if strings.EqualFold(blocked, request.RequestorAgent) {
				eligible = false
			}
		}
		var reviews, active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reviews WHERE request_id = ?`, request.ID).Scan(&reviews); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ? AND project_path = ?
			AND agent_name = ? AND model = ? AND ended_at IS NULL`, request.RequestorSessionID,
			request.ProjectPath, request.RequestorAgent, request.RequestorModel).Scan(&active); err != nil {
			return nil, err
		}
		eligible = eligible && reviews == 0 && active == 1 && request.Command.Hash != "" &&
			db.ComputeCommandHash(request.Command) == request.Command.Hash && !request.CreatedAt.IsZero() && !request.CreatedAt.After(now)
		delay, err := pendingDuration(cfg.Patterns.Caution.AutoApproveDelaySeconds, time.Second)
		if err != nil {
			return nil, fmt.Errorf("caution approval delay: %w", err)
		}
		eligible = eligible && now.Sub(request.CreatedAt) >= delay
		if eligible {
			ttl, err := pendingDuration(cfg.General.ApprovalTTLMins, time.Minute)
			if err != nil || ttl == 0 {
				return nil, errors.New("automatic approval requires a positive, representable approval TTL")
			}
			return &db.PendingDecision{Status: db.StatusApproved, ApprovalTTL: ttl, AllowExpired: expired}, nil
		}
		if expired {
			return timeoutDecision("escalate"), nil
		}
		return nil, nil
	})
}

func timeoutDecision(action string) *db.PendingDecision {
	status := db.StatusEscalated
	if action == "auto_reject" {
		status = db.StatusTimeout // Existing terminal timeout/deny representation.
	}
	return &db.PendingDecision{Status: status}
}

func pendingDuration(value int, unit time.Duration) (time.Duration, error) {
	if value < 0 || int64(value) > int64((1<<63-1)/unit) {
		return 0, errors.New("duration is out of range")
	}
	return time.Duration(value) * unit, nil
}
