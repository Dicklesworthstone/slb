package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestDefaultConfig_Validate(t *testing.T) {
	cfg := DefaultConfig()
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate(DefaultConfig) unexpected error: %v", err)
	}
}

func TestValidate_Errors(t *testing.T) {
	cfg := DefaultConfig()
	cfg.General.MinApprovals = 0
	cfg.General.RequestTimeoutSecs = 0
	cfg.General.DifferentModelTimeoutSecs = -1
	cfg.General.ApprovalTTLMins = 0
	cfg.General.ApprovalTTLCriticalMins = 0
	cfg.General.MaxRollbackSizeMB = -1
	cfg.General.ConflictResolution = "bad"
	cfg.General.TimeoutAction = "bad"
	cfg.RateLimits.MaxPendingPerSession = -1
	cfg.RateLimits.MaxRequestsPerMinute = -1
	cfg.RateLimits.RateLimitAction = "bad"
	cfg.Notifications.DesktopDelaySecs = -1
	cfg.History.RetentionDays = -1
	cfg.Patterns.Critical.MinApprovals = -1
	cfg.Patterns.Dangerous.DynamicQuorumFloor = -1
	cfg.Patterns.Caution.AutoApproveDelaySeconds = -1
	cfg.Agents.TrustedSelfApproveDelaySecs = -1

	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	if !strings.Contains(err.Error(), "config validation failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_Precedence_DefaultsUserProjectEnvFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	project := t.TempDir()

	// User config: 3
	userPath := filepath.Join(home, ".slb", "config.toml")
	if err := WriteValue(userPath, "general.min_approvals", 3); err != nil {
		t.Fatalf("WriteValue user: %v", err)
	}

	// Project config: 4
	projectPath := filepath.Join(project, ".slb", "config.toml")
	if err := WriteValue(projectPath, "general.min_approvals", 4); err != nil {
		t.Fatalf("WriteValue project: %v", err)
	}

	// Env: 5
	t.Setenv("SLB_MIN_APPROVALS", "5")

	// Flags: 6
	cfg, err := Load(LoadOptions{
		ProjectDir: project,
		FlagOverrides: map[string]any{
			"general.min_approvals": 6,
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.General.MinApprovals != 6 {
		t.Fatalf("min_approvals=%d want 6", cfg.General.MinApprovals)
	}
}

func TestLoad_InvalidEnvValueErrors(t *testing.T) {
	t.Setenv("SLB_MIN_APPROVALS", "not-an-int")
	if _, err := Load(LoadOptions{ProjectDir: t.TempDir()}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestLoad_ProjectDirEmptyUsesCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	project := t.TempDir()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})
	if err := os.Chdir(project); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	projectPath := filepath.Join(project, ".slb", "config.toml")
	if err := WriteValue(projectPath, "general.min_approvals", 9); err != nil {
		t.Fatalf("WriteValue project: %v", err)
	}

	cfg, err := Load(LoadOptions{ProjectDir: ""})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.General.MinApprovals != 9 {
		t.Fatalf("min_approvals=%d want 9", cfg.General.MinApprovals)
	}
}

func TestMergeConfigFile(t *testing.T) {
	v := newTestViper()

	// Empty path is a no-op.
	if err := mergeConfigFile(v, ""); err != nil {
		t.Fatalf("mergeConfigFile(empty): %v", err)
	}

	// Missing file is a no-op.
	if err := mergeConfigFile(v, filepath.Join(t.TempDir(), "missing.toml")); err != nil {
		t.Fatalf("mergeConfigFile(missing): %v", err)
	}

	// Directory path is an error.
	if err := mergeConfigFile(v, t.TempDir()); err == nil {
		t.Fatalf("expected error for directory path")
	}

	// Invalid TOML is an error.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("general = [\n"), 0644); err != nil {
		t.Fatalf("write invalid toml: %v", err)
	}
	if err := mergeConfigFile(v, path); err == nil {
		t.Fatalf("expected error for invalid toml")
	}
}

func newTestViper() *viper.Viper {
	// Keep this in a helper to avoid importing viper in every test.
	// It also ensures defaults are seeded, mirroring Load().
	v := viper.New()
	setDefaults(v)
	return v
}

func TestConfigPathsAndProjectConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	u, p := ConfigPaths("/proj", "")
	if u != filepath.Join(home, ".slb", "config.toml") {
		t.Fatalf("unexpected user path: %q", u)
	}
	if p != filepath.Join("/proj", ".slb", "config.toml") {
		t.Fatalf("unexpected project path: %q", p)
	}

	if got := projectConfigPath("", ""); got != ".slb/config.toml" {
		t.Fatalf("projectConfigPath(empty)=%q", got)
	}
	if got := projectConfigPath("/proj", "/override.toml"); got != "/override.toml" {
		t.Fatalf("projectConfigPath(override)=%q", got)
	}
}

func TestParseValue(t *testing.T) {
	v, err := ParseValue("general.min_approvals", "7")
	if err != nil {
		t.Fatalf("ParseValue int: %v", err)
	}
	if v.(int) != 7 {
		t.Fatalf("unexpected value: %#v", v)
	}

	v, err = ParseValue("general.enable_dry_run", "true")
	if err != nil {
		t.Fatalf("ParseValue bool: %v", err)
	}
	if v.(bool) != true {
		t.Fatalf("unexpected value: %#v", v)
	}

	v, err = ParseValue("general.review_pool", "a, , b")
	if err != nil {
		t.Fatalf("ParseValue slice: %v", err)
	}
	if !reflect.DeepEqual(v, []string{"a", "b"}) {
		t.Fatalf("unexpected slice: %#v", v)
	}

	v, err = ParseValue("daemon.ipc_socket", "/tmp/slb.sock")
	if err != nil {
		t.Fatalf("ParseValue string: %v", err)
	}
	if v.(string) != "/tmp/slb.sock" {
		t.Fatalf("unexpected value: %#v", v)
	}

	if _, err := parseValueByKind("x", valueKind(123)); err == nil {
		t.Fatalf("expected error for unsupported value kind")
	}

	if _, err := ParseValue("nope.nope", "x"); err == nil {
		t.Fatalf("expected unsupported key error")
	}
}

func TestGetValue(t *testing.T) {
	cfg := DefaultConfig()

	cases := []struct {
		key  string
		want any
	}{
		{"general.min_approvals", cfg.General.MinApprovals},
		{"general.require_different_model", cfg.General.RequireDifferentModel},
		{"general.different_model_timeout", cfg.General.DifferentModelTimeoutSecs},
		{"general.conflict_resolution", cfg.General.ConflictResolution},
		{"general.request_timeout", cfg.General.RequestTimeoutSecs},
		{"general.approval_ttl_minutes", cfg.General.ApprovalTTLMins},
		{"general.approval_ttl_critical_minutes", cfg.General.ApprovalTTLCriticalMins},
		{"general.timeout_action", cfg.General.TimeoutAction},
		{"general.enable_dry_run", cfg.General.EnableDryRun},
		{"general.enable_rollback_capture", cfg.General.EnableRollbackCapture},
		{"general.max_rollback_size_mb", cfg.General.MaxRollbackSizeMB},
		{"general.cross_project_reviews", cfg.General.CrossProjectReviews},
		{"general.review_pool", cfg.General.ReviewPool},

		{"daemon.use_file_watcher", cfg.Daemon.UseFileWatcher},
		{"daemon.ipc_socket", cfg.Daemon.IPCSocket},
		{"daemon.tcp_addr", cfg.Daemon.TCPAddr},
		{"daemon.tcp_require_auth", cfg.Daemon.TCPRequireAuth},
		{"daemon.tcp_allowed_ips", cfg.Daemon.TCPAllowedIPs},
		{"daemon.log_level", cfg.Daemon.LogLevel},
		{"daemon.pid_file", cfg.Daemon.PIDFile},

		{"rate_limits.max_pending_per_session", cfg.RateLimits.MaxPendingPerSession},
		{"rate_limits.max_requests_per_minute", cfg.RateLimits.MaxRequestsPerMinute},
		{"rate_limits.rate_limit_action", cfg.RateLimits.RateLimitAction},

		{"notifications.desktop_enabled", cfg.Notifications.DesktopEnabled},
		{"notifications.desktop_delay_seconds", cfg.Notifications.DesktopDelaySecs},
		{"notifications.webhook_url", cfg.Notifications.WebhookURL},
		{"notifications.email_enabled", cfg.Notifications.EmailEnabled},

		{"history.database_path", cfg.History.DatabasePath},
		{"history.git_repo_path", cfg.History.GitRepoPath},
		{"history.retention_days", cfg.History.RetentionDays},
		{"history.auto_git_commit", cfg.History.AutoGitCommit},

		{"patterns.critical", cfg.Patterns.Critical},
		{"patterns.critical.min_approvals", cfg.Patterns.Critical.MinApprovals},
		{"patterns.critical.dynamic_quorum", cfg.Patterns.Critical.DynamicQuorum},
		{"patterns.critical.dynamic_quorum_floor", cfg.Patterns.Critical.DynamicQuorumFloor},
		{"patterns.critical.auto_approve_delay_seconds", cfg.Patterns.Critical.AutoApproveDelaySeconds},
		{"patterns.critical.patterns", cfg.Patterns.Critical.Patterns},

		{"patterns.dangerous", cfg.Patterns.Dangerous},
		{"patterns.dangerous.min_approvals", cfg.Patterns.Dangerous.MinApprovals},
		{"patterns.dangerous.dynamic_quorum", cfg.Patterns.Dangerous.DynamicQuorum},
		{"patterns.dangerous.dynamic_quorum_floor", cfg.Patterns.Dangerous.DynamicQuorumFloor},
		{"patterns.dangerous.auto_approve_delay_seconds", cfg.Patterns.Dangerous.AutoApproveDelaySeconds},
		{"patterns.dangerous.patterns", cfg.Patterns.Dangerous.Patterns},

		{"patterns.caution", cfg.Patterns.Caution},
		{"patterns.caution.min_approvals", cfg.Patterns.Caution.MinApprovals},
		{"patterns.caution.dynamic_quorum", cfg.Patterns.Caution.DynamicQuorum},
		{"patterns.caution.dynamic_quorum_floor", cfg.Patterns.Caution.DynamicQuorumFloor},
		{"patterns.caution.auto_approve_delay_seconds", cfg.Patterns.Caution.AutoApproveDelaySeconds},
		{"patterns.caution.patterns", cfg.Patterns.Caution.Patterns},

		{"patterns.safe", cfg.Patterns.Safe},
		{"patterns.safe.min_approvals", cfg.Patterns.Safe.MinApprovals},
		{"patterns.safe.dynamic_quorum", cfg.Patterns.Safe.DynamicQuorum},
		{"patterns.safe.dynamic_quorum_floor", cfg.Patterns.Safe.DynamicQuorumFloor},
		{"patterns.safe.auto_approve_delay_seconds", cfg.Patterns.Safe.AutoApproveDelaySeconds},
		{"patterns.safe.patterns", cfg.Patterns.Safe.Patterns},

		{"integrations.agent_mail_enabled", cfg.Integrations.AgentMailEnabled},
		{"integrations.agent_mail_thread", cfg.Integrations.AgentMailThread},
		{"integrations.claude_hooks_enabled", cfg.Integrations.ClaudeHooksEnabled},

		{"agents.trusted_self_approve", cfg.Agents.TrustedSelfApprove},
		{"agents.trusted_self_approve_delay_seconds", cfg.Agents.TrustedSelfApproveDelaySecs},
		{"agents.blocked", cfg.Agents.Blocked},

		{"general", cfg.General},
		{"daemon", cfg.Daemon},
		{"rate_limits", cfg.RateLimits},
		{"notifications", cfg.Notifications},
		{"history", cfg.History},
		{"patterns", cfg.Patterns},
		{"integrations", cfg.Integrations},
		{"agents", cfg.Agents},
	}

	for _, tc := range cases {
		got, ok := GetValue(cfg, tc.key)
		if !ok {
			t.Fatalf("GetValue(%q) not found", tc.key)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("GetValue(%q)=%#v want %#v", tc.key, got, tc.want)
		}
	}

	if _, ok := GetValue(cfg, ""); ok {
		t.Fatalf("expected empty key to be not found")
	}

	badKeys := []string{
		"nope",
		"general.nope",
		"daemon.nope",
		"rate_limits.nope",
		"notifications.nope",
		"history.nope",
		"patterns.nope",
		"patterns.critical.nope",
		"integrations.nope",
		"agents.nope",
	}
	for _, key := range badKeys {
		if _, ok := GetValue(cfg, key); ok {
			t.Fatalf("expected %q to be not found", key)
		}
	}
}

func TestWriteValue(t *testing.T) {
	if err := WriteValue("", "general.min_approvals", 2); err == nil {
		t.Fatalf("expected error for empty path")
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteValue(path, "general.min_approvals", 3); err != nil {
		t.Fatalf("WriteValue: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "[general]") || !strings.Contains(string(data), "min_approvals = 3") {
		t.Fatalf("unexpected toml: %q", string(data))
	}

	// Error when an intermediate segment is not a table.
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("general = \"oops\"\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteValue(bad, "general.min_approvals", 2); err == nil {
		t.Fatalf("expected error when general is not a table")
	}
}

func TestWriteValue_DecodeExistingInvalidTOMLErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("general = [\n"), 0644); err != nil {
		t.Fatalf("write invalid toml: %v", err)
	}
	if err := WriteValue(path, "general.min_approvals", 2); err == nil {
		t.Fatalf("expected decode error")
	} else if !strings.Contains(err.Error(), "decode config") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_DifferentModelTimeoutRequiredWhenEnabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.General.RequireDifferentModel = true
	cfg.General.DifferentModelTimeoutSecs = 0
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "different_model_timeout") {
		t.Fatalf("expected different-model timeout validation error, got %v", err)
	}
}
