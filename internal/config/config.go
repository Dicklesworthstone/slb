// Package config implements hierarchical configuration for SLB.
// Precedence: defaults < user (~/.slb/config.toml) < project (.slb/config.toml) < env (SLB_*) < flags.
package config

import "github.com/Dicklesworthstone/slb/internal/notifications"

// Config is the top-level configuration structure.
type Config struct {
	General       GeneralConfig       `toml:"general" mapstructure:"general"`
	Daemon        DaemonConfig        `toml:"daemon" mapstructure:"daemon"`
	RateLimits    RateLimitConfig     `toml:"rate_limits" mapstructure:"rate_limits"`
	Notifications NotificationsConfig `toml:"notifications" mapstructure:"notifications"`
	History       HistoryConfig       `toml:"history" mapstructure:"history"`
	Patterns      PatternsConfig      `toml:"patterns" mapstructure:"patterns"`
	Integrations  IntegrationsConfig  `toml:"integrations" mapstructure:"integrations"`
	Agents        AgentsConfig        `toml:"agents" mapstructure:"agents"`
}

// GeneralConfig holds core behavior knobs.
type GeneralConfig struct {
	MinApprovals              int      `toml:"min_approvals" mapstructure:"min_approvals"`
	RequireDifferentModel     bool     `toml:"require_different_model" mapstructure:"require_different_model"`
	DifferentModelTimeoutSecs int      `toml:"different_model_timeout" mapstructure:"different_model_timeout"`
	ConflictResolution        string   `toml:"conflict_resolution" mapstructure:"conflict_resolution"` // any_rejection_blocks | first_wins | human_breaks_tie
	RequestTimeoutSecs        int      `toml:"request_timeout" mapstructure:"request_timeout"`
	ApprovalTTLMins           int      `toml:"approval_ttl_minutes" mapstructure:"approval_ttl_minutes"`
	ApprovalTTLCriticalMins   int      `toml:"approval_ttl_critical_minutes" mapstructure:"approval_ttl_critical_minutes"`
	TimeoutAction             string   `toml:"timeout_action" mapstructure:"timeout_action"` // escalate | auto_reject | auto_approve_warn
	EnableDryRun              bool     `toml:"enable_dry_run" mapstructure:"enable_dry_run"`
	EnableRollbackCapture     bool     `toml:"enable_rollback_capture" mapstructure:"enable_rollback_capture"`
	MaxRollbackSizeMB         int      `toml:"max_rollback_size_mb" mapstructure:"max_rollback_size_mb"`
	CrossProjectReviews       bool     `toml:"cross_project_reviews" mapstructure:"cross_project_reviews"`
	ReviewPool                []string `toml:"review_pool" mapstructure:"review_pool"`
}

// DaemonConfig holds daemon process settings.
type DaemonConfig struct {
	UseFileWatcher bool     `toml:"use_file_watcher" mapstructure:"use_file_watcher"`
	IPCSocket      string   `toml:"ipc_socket" mapstructure:"ipc_socket"`
	TCPAddr        string   `toml:"tcp_addr" mapstructure:"tcp_addr"`
	TCPRequireAuth bool     `toml:"tcp_require_auth" mapstructure:"tcp_require_auth"`
	TCPAllowedIPs  []string `toml:"tcp_allowed_ips" mapstructure:"tcp_allowed_ips"`
	LogLevel       string   `toml:"log_level" mapstructure:"log_level"`
	PIDFile        string   `toml:"pid_file" mapstructure:"pid_file"`
}

// RateLimitConfig holds rate-limiting settings.
type RateLimitConfig struct {
	MaxPendingPerSession int    `toml:"max_pending_per_session" mapstructure:"max_pending_per_session"`
	MaxRequestsPerMinute int    `toml:"max_requests_per_minute" mapstructure:"max_requests_per_minute"`
	RateLimitAction      string `toml:"rate_limit_action" mapstructure:"rate_limit_action"` // reject | queue | warn
}

// NotificationsConfig holds notification settings.
type NotificationsConfig struct {
	Blocked          notifications.Config          `toml:"blocked" mapstructure:"blocked"`
	Requests         notifications.RequestSettings `toml:"requests" mapstructure:"requests"`
	DesktopEnabled   bool                          `toml:"desktop_enabled" mapstructure:"desktop_enabled"`
	DesktopDelaySecs int                           `toml:"desktop_delay_seconds" mapstructure:"desktop_delay_seconds"`
	WebhookURL       string                        `toml:"webhook_url" mapstructure:"webhook_url"`
	EmailEnabled     bool                          `toml:"email_enabled" mapstructure:"email_enabled"`
}

// HistoryConfig holds history/audit persistence settings.
type HistoryConfig struct {
	DatabasePath  string `toml:"database_path" mapstructure:"database_path"`
	GitRepoPath   string `toml:"git_repo_path" mapstructure:"git_repo_path"`
	RetentionDays int    `toml:"retention_days" mapstructure:"retention_days"`
	AutoGitCommit bool   `toml:"auto_git_commit" mapstructure:"auto_git_commit"`
}

// PatternsConfig defines tiers and patterns.
type PatternsConfig struct {
	Critical  PatternTierConfig `toml:"critical" mapstructure:"critical"`
	Dangerous PatternTierConfig `toml:"dangerous" mapstructure:"dangerous"`
	Caution   PatternTierConfig `toml:"caution" mapstructure:"caution"`
	Safe      PatternTierConfig `toml:"safe" mapstructure:"safe"`
}

// PatternTierConfig represents configuration for a risk tier.
type PatternTierConfig struct {
	MinApprovals            int      `toml:"min_approvals" mapstructure:"min_approvals"`
	DynamicQuorum           bool     `toml:"dynamic_quorum" mapstructure:"dynamic_quorum"`
	DynamicQuorumFloor      int      `toml:"dynamic_quorum_floor" mapstructure:"dynamic_quorum_floor"`
	AutoApproveDelaySeconds int      `toml:"auto_approve_delay_seconds" mapstructure:"auto_approve_delay_seconds"`
	Patterns                []string `toml:"patterns" mapstructure:"patterns"`
}

// IntegrationsConfig holds external integration toggles.
type IntegrationsConfig struct {
	AgentMailEnabled   bool   `toml:"agent_mail_enabled" mapstructure:"agent_mail_enabled"`
	AgentMailThread    string `toml:"agent_mail_thread" mapstructure:"agent_mail_thread"`
	ClaudeHooksEnabled bool   `toml:"claude_hooks_enabled" mapstructure:"claude_hooks_enabled"`
	// HookCautionAction controls Claude PreToolUse handling for CAUTION:
	// block keeps the command inside SLB's request/auto-approval workflow;
	// ask delegates the decision to Claude Code's human permission prompt.
	HookCautionAction string `toml:"hook_caution_action" mapstructure:"hook_caution_action"`
	// HookQueryTimeoutMS bounds how long the native Claude Code hook waits
	// for the project daemon before using the local fail-closed fallback.
	HookQueryTimeoutMS int `toml:"hook_query_timeout_ms" mapstructure:"hook_query_timeout_ms"`
}

// Native hook daemon-query deadline bounds. The daemon loads the project
// policy per query (typically 10-30ms); 250ms absorbs a momentary stall while
// staying far inside Claude Code's hook budget. A deadline still falls back to
// the local, fail-closed classifier.
const (
	DefaultHookQueryTimeoutMS = 250
	MinHookQueryTimeoutMS     = 10
	MaxHookQueryTimeoutMS     = 10000
)

// AgentsConfig holds agent-specific allow/deny lists.
type AgentsConfig struct {
	TrustedSelfApprove          []string `toml:"trusted_self_approve" mapstructure:"trusted_self_approve"`
	TrustedSelfApproveDelaySecs int      `toml:"trusted_self_approve_delay_seconds" mapstructure:"trusted_self_approve_delay_seconds"`
	Blocked                     []string `toml:"blocked" mapstructure:"blocked"`
}
