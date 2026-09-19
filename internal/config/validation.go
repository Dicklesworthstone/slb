package config

import (
	"fmt"
	"strings"
)

// Validate checks the configuration for semantic errors.
func Validate(cfg Config) error {
	var errs []string

	if cfg.General.MinApprovals < 1 {
		errs = append(errs, "general.min_approvals must be >= 1")
	}
	if cfg.General.RequestTimeoutSecs <= 0 {
		errs = append(errs, "general.request_timeout must be > 0 seconds")
	}
	if cfg.General.DifferentModelTimeoutSecs < 0 {
		errs = append(errs, "general.different_model_timeout cannot be negative")
	}
	if cfg.General.RequireDifferentModel && cfg.General.DifferentModelTimeoutSecs == 0 {
		errs = append(errs, "general.different_model_timeout must be > 0 when require_different_model is enabled")
	}
	if cfg.General.ApprovalTTLMins <= 0 {
		errs = append(errs, "general.approval_ttl_minutes must be > 0")
	}
	if cfg.General.ApprovalTTLCriticalMins <= 0 {
		errs = append(errs, "general.approval_ttl_critical_minutes must be > 0")
	}
	if cfg.General.MaxRollbackSizeMB < 0 {
		errs = append(errs, "general.max_rollback_size_mb cannot be negative")
	}
	if !oneOf(cfg.General.ConflictResolution, "any_rejection_blocks", "first_wins", "human_breaks_tie") {
		errs = append(errs, "general.conflict_resolution must be one of any_rejection_blocks|first_wins|human_breaks_tie")
	}
	if !oneOf(cfg.General.TimeoutAction, "escalate", "auto_reject", "auto_approve_warn") {
		errs = append(errs, "general.timeout_action must be one of escalate|auto_reject|auto_approve_warn")
	}

	if cfg.RateLimits.MaxPendingPerSession < 0 {
		errs = append(errs, "rate_limits.max_pending_per_session cannot be negative")
	}
	if cfg.RateLimits.MaxRequestsPerMinute < 0 {
		errs = append(errs, "rate_limits.max_requests_per_minute cannot be negative")
	}
	if !oneOf(cfg.RateLimits.RateLimitAction, "reject", "queue", "warn") {
		errs = append(errs, "rate_limits.rate_limit_action must be one of reject|queue|warn")
	}

	if cfg.Notifications.DesktopDelaySecs < 0 {
		errs = append(errs, "notifications.desktop_delay_seconds cannot be negative")
	}

	if cfg.History.RetentionDays < 0 {
		errs = append(errs, "history.retention_days cannot be negative")
	}

	validateTier := func(name string, tier PatternTierConfig) {
		if tier.MinApprovals < 0 {
			errs = append(errs, fmt.Sprintf("patterns.%s.min_approvals cannot be negative", name))
		}
		if tier.DynamicQuorumFloor < 0 {
			errs = append(errs, fmt.Sprintf("patterns.%s.dynamic_quorum_floor cannot be negative", name))
		}
		if tier.AutoApproveDelaySeconds < 0 {
			errs = append(errs, fmt.Sprintf("patterns.%s.auto_approve_delay_seconds cannot be negative", name))
		}
	}
	validateTier("critical", cfg.Patterns.Critical)
	validateTier("dangerous", cfg.Patterns.Dangerous)
	validateTier("caution", cfg.Patterns.Caution)
	validateTier("safe", cfg.Patterns.Safe)

	if cfg.Agents.TrustedSelfApproveDelaySecs < 0 {
		errs = append(errs, "agents.trusted_self_approve_delay_seconds cannot be negative")
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func oneOf(val string, options ...string) bool {
	for _, opt := range options {
		if val == opt {
			return true
		}
	}
	return false
}
