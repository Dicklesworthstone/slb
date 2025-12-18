package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/viper"
)

// LoadOptions controls configuration loading.
type LoadOptions struct {
	// ProjectDir is used to locate .slb/config.toml. Defaults to CWD when empty.
	ProjectDir string
	// ConfigPath overrides the project config path if provided.
	ConfigPath string
	// FlagOverrides are highest-priority overrides from CLI flags (dot-notated keys).
	FlagOverrides map[string]any
}

// Load returns the effective configuration after applying precedence:
// defaults < user (~/.slb/config.toml) < project (.slb/config.toml) < env (SLB_*) < flags.
func Load(opts LoadOptions) (Config, error) {
	v := viper.New()
	setDefaults(v)

	projectDir := opts.ProjectDir
	if projectDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectDir = cwd
		}
	}

	// 1) User config
	if err := mergeConfigFile(v, userConfigPath()); err != nil {
		return Config{}, err
	}
	// 2) Project config
	if err := mergeConfigFile(v, projectConfigPath(projectDir, opts.ConfigPath)); err != nil {
		return Config{}, err
	}
	// 3) Environment variables
	if err := applyEnvOverrides(v); err != nil {
		return Config{}, err
	}
	// 4) CLI flags (highest)
	applyFlagOverrides(v, opts.FlagOverrides)

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// setDefaults seeds viper with built-in defaults.
func setDefaults(v *viper.Viper) {
	def := DefaultConfig()

	v.SetDefault("general.min_approvals", def.General.MinApprovals)
	v.SetDefault("general.require_different_model", def.General.RequireDifferentModel)
	v.SetDefault("general.different_model_timeout", def.General.DifferentModelTimeoutSecs)
	v.SetDefault("general.conflict_resolution", def.General.ConflictResolution)
	v.SetDefault("general.request_timeout", def.General.RequestTimeoutSecs)
	v.SetDefault("general.approval_ttl_minutes", def.General.ApprovalTTLMins)
	v.SetDefault("general.approval_ttl_critical_minutes", def.General.ApprovalTTLCriticalMins)
	v.SetDefault("general.timeout_action", def.General.TimeoutAction)
	v.SetDefault("general.enable_dry_run", def.General.EnableDryRun)
	v.SetDefault("general.enable_rollback_capture", def.General.EnableRollbackCapture)
	v.SetDefault("general.max_rollback_size_mb", def.General.MaxRollbackSizeMB)
	v.SetDefault("general.cross_project_reviews", def.General.CrossProjectReviews)
	v.SetDefault("general.review_pool", def.General.ReviewPool)

	v.SetDefault("daemon.use_file_watcher", def.Daemon.UseFileWatcher)
	v.SetDefault("daemon.ipc_socket", def.Daemon.IPCSocket)
	v.SetDefault("daemon.tcp_addr", def.Daemon.TCPAddr)
	v.SetDefault("daemon.tcp_require_auth", def.Daemon.TCPRequireAuth)
	v.SetDefault("daemon.tcp_allowed_ips", def.Daemon.TCPAllowedIPs)
	v.SetDefault("daemon.log_level", def.Daemon.LogLevel)
	v.SetDefault("daemon.pid_file", def.Daemon.PIDFile)

	v.SetDefault("rate_limits.max_pending_per_session", def.RateLimits.MaxPendingPerSession)
	v.SetDefault("rate_limits.max_requests_per_minute", def.RateLimits.MaxRequestsPerMinute)
	v.SetDefault("rate_limits.rate_limit_action", def.RateLimits.RateLimitAction)

	v.SetDefault("notifications.desktop_enabled", def.Notifications.DesktopEnabled)
	v.SetDefault("notifications.desktop_delay_seconds", def.Notifications.DesktopDelaySecs)
	v.SetDefault("notifications.webhook_url", def.Notifications.WebhookURL)
	v.SetDefault("notifications.email_enabled", def.Notifications.EmailEnabled)

	v.SetDefault("history.database_path", def.History.DatabasePath)
	v.SetDefault("history.git_repo_path", def.History.GitRepoPath)
	v.SetDefault("history.retention_days", def.History.RetentionDays)
	v.SetDefault("history.auto_git_commit", def.History.AutoGitCommit)

	// Pattern tiers
	setTierDefaults(v, "patterns.critical", def.Patterns.Critical)
	setTierDefaults(v, "patterns.dangerous", def.Patterns.Dangerous)
	setTierDefaults(v, "patterns.caution", def.Patterns.Caution)
	setTierDefaults(v, "patterns.safe", def.Patterns.Safe)

	v.SetDefault("integrations.agent_mail_enabled", def.Integrations.AgentMailEnabled)
	v.SetDefault("integrations.agent_mail_thread", def.Integrations.AgentMailThread)
	v.SetDefault("integrations.claude_hooks_enabled", def.Integrations.ClaudeHooksEnabled)

	v.SetDefault("agents.trusted_self_approve", def.Agents.TrustedSelfApprove)
	v.SetDefault("agents.trusted_self_approve_delay_seconds", def.Agents.TrustedSelfApproveDelaySecs)
	v.SetDefault("agents.blocked", def.Agents.Blocked)
}

func setTierDefaults(v *viper.Viper, prefix string, tier PatternTierConfig) {
	v.SetDefault(prefix+".min_approvals", tier.MinApprovals)
	v.SetDefault(prefix+".dynamic_quorum", tier.DynamicQuorum)
	v.SetDefault(prefix+".dynamic_quorum_floor", tier.DynamicQuorumFloor)
	v.SetDefault(prefix+".auto_approve_delay_seconds", tier.AutoApproveDelaySeconds)
	v.SetDefault(prefix+".patterns", tier.Patterns)
}

// mergeConfigFile merges the TOML config file if it exists.
func mergeConfigFile(v *viper.Viper, path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat config %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("config path %s is a directory", path)
	}
	v.SetConfigFile(path)
	if err := v.MergeInConfig(); err != nil {
		return fmt.Errorf("merge config %s: %w", path, err)
	}
	return nil
}

// applyEnvOverrides reads SLB_* env vars and applies them.
func applyEnvOverrides(v *viper.Viper) error {
	for _, binding := range envBindings {
		val := os.Getenv(binding.Env)
		if val == "" {
			continue
		}
		parsed, err := parseValueByKind(val, binding.Kind)
		if err != nil {
			return fmt.Errorf("env %s: %w", binding.Env, err)
		}
		v.Set(binding.Key, parsed)
	}
	return nil
}

// applyFlagOverrides applies CLI overrides as highest-precedence values.
func applyFlagOverrides(v *viper.Viper, overrides map[string]any) {
	for k, val := range overrides {
		v.Set(k, val)
	}
}

// ConfigPaths returns the user and project config file paths.
func ConfigPaths(projectDir, configOverride string) (string, string) {
	return userConfigPath(), projectConfigPath(projectDir, configOverride)
}

func userConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".slb", "config.toml")
}

func projectConfigPath(projectDir, override string) string {
	if override != "" {
		return override
	}
	if projectDir == "" {
		return ".slb/config.toml"
	}
	return filepath.Join(projectDir, ".slb", "config.toml")
}

// ParseValue parses a raw string into the expected type for a given config key.
func ParseValue(key, raw string) (any, error) {
	kind, ok := keyKinds[key]
	if !ok {
		return nil, fmt.Errorf("unsupported key %q", key)
	}
	return parseValueByKind(raw, kind)
}

// GetValue retrieves a dot-notated value from the Config.
func GetValue(cfg Config, key string) (any, bool) {
	segments := strings.Split(key, ".")
	if len(segments) == 0 {
		return nil, false
	}
	var current any = cfg
	for _, seg := range segments {
		switch c := current.(type) {
		case Config:
			switch seg {
			case "general":
				current = c.General
			case "daemon":
				current = c.Daemon
			case "rate_limits":
				current = c.RateLimits
			case "notifications":
				current = c.Notifications
			case "history":
				current = c.History
			case "patterns":
				current = c.Patterns
			case "integrations":
				current = c.Integrations
			case "agents":
				current = c.Agents
			default:
				return nil, false
			}
		case GeneralConfig:
			switch seg {
			case "min_approvals":
				return c.MinApprovals, true
			case "require_different_model":
				return c.RequireDifferentModel, true
			case "different_model_timeout":
				return c.DifferentModelTimeoutSecs, true
			case "conflict_resolution":
				return c.ConflictResolution, true
			case "request_timeout":
				return c.RequestTimeoutSecs, true
			case "approval_ttl_minutes":
				return c.ApprovalTTLMins, true
			case "approval_ttl_critical_minutes":
				return c.ApprovalTTLCriticalMins, true
			case "timeout_action":
				return c.TimeoutAction, true
			case "enable_dry_run":
				return c.EnableDryRun, true
			case "enable_rollback_capture":
				return c.EnableRollbackCapture, true
			case "max_rollback_size_mb":
				return c.MaxRollbackSizeMB, true
			case "cross_project_reviews":
				return c.CrossProjectReviews, true
			case "review_pool":
				return c.ReviewPool, true
			default:
				return nil, false
			}
		case DaemonConfig:
			switch seg {
			case "use_file_watcher":
				return c.UseFileWatcher, true
			case "ipc_socket":
				return c.IPCSocket, true
			case "tcp_addr":
				return c.TCPAddr, true
			case "tcp_require_auth":
				return c.TCPRequireAuth, true
			case "tcp_allowed_ips":
				return c.TCPAllowedIPs, true
			case "log_level":
				return c.LogLevel, true
			case "pid_file":
				return c.PIDFile, true
			default:
				return nil, false
			}
		case RateLimitConfig:
			switch seg {
			case "max_pending_per_session":
				return c.MaxPendingPerSession, true
			case "max_requests_per_minute":
				return c.MaxRequestsPerMinute, true
			case "rate_limit_action":
				return c.RateLimitAction, true
			default:
				return nil, false
			}
		case NotificationsConfig:
			switch seg {
			case "desktop_enabled":
				return c.DesktopEnabled, true
			case "desktop_delay_seconds":
				return c.DesktopDelaySecs, true
			case "webhook_url":
				return c.WebhookURL, true
			case "email_enabled":
				return c.EmailEnabled, true
			default:
				return nil, false
			}
		case HistoryConfig:
			switch seg {
			case "database_path":
				return c.DatabasePath, true
			case "git_repo_path":
				return c.GitRepoPath, true
			case "retention_days":
				return c.RetentionDays, true
			case "auto_git_commit":
				return c.AutoGitCommit, true
			default:
				return nil, false
			}
		case PatternsConfig:
			switch seg {
			case "critical":
				current = c.Critical
			case "dangerous":
				current = c.Dangerous
			case "caution":
				current = c.Caution
			case "safe":
				current = c.Safe
			default:
				return nil, false
			}
		case PatternTierConfig:
			switch seg {
			case "min_approvals":
				return c.MinApprovals, true
			case "dynamic_quorum":
				return c.DynamicQuorum, true
			case "dynamic_quorum_floor":
				return c.DynamicQuorumFloor, true
			case "auto_approve_delay_seconds":
				return c.AutoApproveDelaySeconds, true
			case "patterns":
				return c.Patterns, true
			default:
				return nil, false
			}
		case IntegrationsConfig:
			switch seg {
			case "agent_mail_enabled":
				return c.AgentMailEnabled, true
			case "agent_mail_thread":
				return c.AgentMailThread, true
			case "claude_hooks_enabled":
				return c.ClaudeHooksEnabled, true
			default:
				return nil, false
			}
		case AgentsConfig:
			switch seg {
			case "trusted_self_approve":
				return c.TrustedSelfApprove, true
			case "trusted_self_approve_delay_seconds":
				return c.TrustedSelfApproveDelaySecs, true
			case "blocked":
				return c.Blocked, true
			default:
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return current, true
}

// WriteValue sets a single key/value into the specified TOML config file (creating it if needed).
func WriteValue(path, key string, value any) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	var existing map[string]any
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &existing); err != nil {
			return fmt.Errorf("decode config: %w", err)
		}
		if existing == nil {
			existing = map[string]any{}
		}
	} else {
		existing = map[string]any{}
	}

	if err := setNested(existing, key, value); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create config %s: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	enc.Indent = "  "
	if err := enc.Encode(existing); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}

func setNested(m map[string]any, key string, value any) error {
	parts := strings.Split(key, ".")
	if len(parts) == 0 {
		return fmt.Errorf("invalid key %q", key)
	}
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			return nil
		}
		next, ok := cur[p]
		if !ok {
			child := map[string]any{}
			cur[p] = child
			cur = child
			continue
		}
		childMap, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot set %s: %s is not a table", key, strings.Join(parts[:i+1], "."))
		}
		cur = childMap
	}
	return nil
}

// Helpers for env + parsing ---------------------------------------------------

type valueKind int

const (
	kindString valueKind = iota
	kindBool
	kindInt
	kindStringSlice
)

var keyKinds = map[string]valueKind{
	"general.min_approvals":                 kindInt,
	"general.require_different_model":       kindBool,
	"general.different_model_timeout":       kindInt,
	"general.conflict_resolution":           kindString,
	"general.request_timeout":               kindInt,
	"general.approval_ttl_minutes":          kindInt,
	"general.approval_ttl_critical_minutes": kindInt,
	"general.timeout_action":                kindString,
	"general.enable_dry_run":                kindBool,
	"general.enable_rollback_capture":       kindBool,
	"general.max_rollback_size_mb":          kindInt,
	"general.cross_project_reviews":         kindBool,
	"general.review_pool":                   kindStringSlice,

	"daemon.use_file_watcher": kindBool,
	"daemon.ipc_socket":       kindString,
	"daemon.tcp_addr":         kindString,
	"daemon.tcp_require_auth": kindBool,
	"daemon.tcp_allowed_ips":  kindStringSlice,
	"daemon.log_level":        kindString,
	"daemon.pid_file":         kindString,

	"rate_limits.max_pending_per_session": kindInt,
	"rate_limits.max_requests_per_minute": kindInt,
	"rate_limits.rate_limit_action":       kindString,

	"notifications.desktop_enabled":       kindBool,
	"notifications.desktop_delay_seconds": kindInt,
	"notifications.webhook_url":           kindString,
	"notifications.email_enabled":         kindBool,

	"history.database_path":   kindString,
	"history.git_repo_path":   kindString,
	"history.retention_days":  kindInt,
	"history.auto_git_commit": kindBool,

	"patterns.critical.min_approvals":              kindInt,
	"patterns.critical.dynamic_quorum":             kindBool,
	"patterns.critical.dynamic_quorum_floor":       kindInt,
	"patterns.critical.auto_approve_delay_seconds": kindInt,
	"patterns.critical.patterns":                   kindStringSlice,

	"patterns.dangerous.min_approvals":              kindInt,
	"patterns.dangerous.dynamic_quorum":             kindBool,
	"patterns.dangerous.dynamic_quorum_floor":       kindInt,
	"patterns.dangerous.auto_approve_delay_seconds": kindInt,
	"patterns.dangerous.patterns":                   kindStringSlice,

	"patterns.caution.min_approvals":              kindInt,
	"patterns.caution.dynamic_quorum":             kindBool,
	"patterns.caution.dynamic_quorum_floor":       kindInt,
	"patterns.caution.auto_approve_delay_seconds": kindInt,
	"patterns.caution.patterns":                   kindStringSlice,

	"patterns.safe.min_approvals":              kindInt,
	"patterns.safe.dynamic_quorum":             kindBool,
	"patterns.safe.dynamic_quorum_floor":       kindInt,
	"patterns.safe.auto_approve_delay_seconds": kindInt,
	"patterns.safe.patterns":                   kindStringSlice,

	"integrations.agent_mail_enabled":   kindBool,
	"integrations.agent_mail_thread":    kindString,
	"integrations.claude_hooks_enabled": kindBool,

	"agents.trusted_self_approve":               kindStringSlice,
	"agents.trusted_self_approve_delay_seconds": kindInt,
	"agents.blocked":                            kindStringSlice,
}

var envBindings = []struct {
	Env  string
	Key  string
	Kind valueKind
}{
	{"SLB_MIN_APPROVALS", "general.min_approvals", kindInt},
	{"SLB_REQUIRE_DIFFERENT_MODEL", "general.require_different_model", kindBool},
	{"SLB_DIFFERENT_MODEL_TIMEOUT", "general.different_model_timeout", kindInt},
	{"SLB_CONFLICT_RESOLUTION", "general.conflict_resolution", kindString},
	{"SLB_REQUEST_TIMEOUT", "general.request_timeout", kindInt},
	{"SLB_APPROVAL_TTL_MINUTES", "general.approval_ttl_minutes", kindInt},
	{"SLB_APPROVAL_TTL_CRITICAL_MINUTES", "general.approval_ttl_critical_minutes", kindInt},
	{"SLB_TIMEOUT_ACTION", "general.timeout_action", kindString},
	{"SLB_ENABLE_DRY_RUN", "general.enable_dry_run", kindBool},
	{"SLB_ENABLE_ROLLBACK_CAPTURE", "general.enable_rollback_capture", kindBool},
	{"SLB_MAX_ROLLBACK_SIZE_MB", "general.max_rollback_size_mb", kindInt},
	{"SLB_CROSS_PROJECT_REVIEWS", "general.cross_project_reviews", kindBool},
	{"SLB_REVIEW_POOL", "general.review_pool", kindStringSlice},

	{"SLB_DAEMON_USE_FILE_WATCHER", "daemon.use_file_watcher", kindBool},
	{"SLB_DAEMON_IPC_SOCKET", "daemon.ipc_socket", kindString},
	{"SLB_DAEMON_TCP_ADDR", "daemon.tcp_addr", kindString},
	{"SLB_DAEMON_TCP_REQUIRE_AUTH", "daemon.tcp_require_auth", kindBool},
	{"SLB_DAEMON_TCP_ALLOWED_IPS", "daemon.tcp_allowed_ips", kindStringSlice},
	{"SLB_DAEMON_LOG_LEVEL", "daemon.log_level", kindString},
	{"SLB_DAEMON_PID_FILE", "daemon.pid_file", kindString},

	{"SLB_MAX_PENDING_PER_SESSION", "rate_limits.max_pending_per_session", kindInt},
	{"SLB_MAX_REQUESTS_PER_MINUTE", "rate_limits.max_requests_per_minute", kindInt},
	{"SLB_RATE_LIMIT_ACTION", "rate_limits.rate_limit_action", kindString},

	{"SLB_DESKTOP_NOTIFICATIONS", "notifications.desktop_enabled", kindBool},
	{"SLB_DESKTOP_DELAY_SECONDS", "notifications.desktop_delay_seconds", kindInt},
	{"SLB_WEBHOOK_URL", "notifications.webhook_url", kindString},
	{"SLB_EMAIL_ENABLED", "notifications.email_enabled", kindBool},

	{"SLB_HISTORY_DB_PATH", "history.database_path", kindString},
	{"SLB_HISTORY_GIT_PATH", "history.git_repo_path", kindString},
	{"SLB_HISTORY_RETENTION_DAYS", "history.retention_days", kindInt},
	{"SLB_HISTORY_AUTO_GIT_COMMIT", "history.auto_git_commit", kindBool},

	{"SLB_AGENT_MAIL_ENABLED", "integrations.agent_mail_enabled", kindBool},
	{"SLB_AGENT_MAIL_THREAD", "integrations.agent_mail_thread", kindString},
	{"SLB_CLAUDE_HOOKS_ENABLED", "integrations.claude_hooks_enabled", kindBool},

	{"SLB_TRUSTED_SELF_APPROVE", "agents.trusted_self_approve", kindStringSlice},
	{"SLB_TRUSTED_SELF_APPROVE_DELAY_SECONDS", "agents.trusted_self_approve_delay_seconds", kindInt},
	{"SLB_BLOCKED_AGENTS", "agents.blocked", kindStringSlice},
}

func parseValueByKind(raw string, kind valueKind) (any, error) {
	switch kind {
	case kindString:
		return raw, nil
	case kindBool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("expected boolean: %w", err)
		}
		return v, nil
	case kindInt:
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("expected integer: %w", err)
		}
		return v, nil
	case kindStringSlice:
		parts := strings.Split(raw, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported value kind")
	}
}
