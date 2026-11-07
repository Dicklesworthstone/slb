package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestDefaultConfig_Validate(t *testing.T) {
	cfg := DefaultConfig()
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate(DefaultConfig) unexpected error: %v", err)
	}
}

func TestApprovalPolicyRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SLB_APPROVAL_POLICY", "")
	project := t.TempDir()
	opts := LoadOptions{ProjectDir: project}
	cfg, err := Load(opts)
	if err != nil || cfg.General.ApprovalPolicy != "independent" {
		t.Fatalf("secure default = %q, %v", cfg.General.ApprovalPolicy, err)
	}
	value, err := ParseValue("general.approval_policy", "same_host")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteValue(filepath.Join(project, ".slb", "config.toml"), "general.approval_policy", value); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(opts)
	if err != nil || cfg.General.ApprovalPolicy != "same_host" {
		t.Fatalf("explicit opt-in = %q, %v", cfg.General.ApprovalPolicy, err)
	}
	before, err := ReadSources(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLB_APPROVAL_POLICY", "independent")
	after, err := ReadSources(opts)
	if err != nil || before.Equal(after) {
		t.Fatalf("approval policy change did not invalidate cached config: %v", err)
	}
	cfg, err = Load(opts)
	if err != nil || cfg.General.ApprovalPolicy != "independent" {
		t.Fatalf("environment revocation = %q, %v", cfg.General.ApprovalPolicy, err)
	}
	for _, invalid := range []string{"same-host", "true", "authenticated", "SAME_HOST"} {
		t.Setenv("SLB_APPROVAL_POLICY", invalid)
		if _, err := Load(opts); err == nil {
			t.Fatalf("invalid policy %q was accepted", invalid)
		}
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
	cfg.Integrations.HookCautionAction = "bad"
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

func TestRequestTimeoutPreservesRepresentableSeconds(t *testing.T) {
	for _, seconds := range []int{1, 2, 59, 61} {
		cfg := DefaultConfig()
		cfg.General.RequestTimeoutSecs = seconds
		if err := Validate(cfg); err != nil {
			t.Fatalf("valid seconds deadline %d was rejected: %v", seconds, err)
		}
	}
	if strconv.IntSize < 64 {
		return // An int on these platforms cannot exceed time.Duration's second range.
	}
	maxSeconds := int64((1<<63 - 1) / time.Second)
	cfg := DefaultConfig()
	cfg.General.RequestTimeoutSecs = int(maxSeconds)
	if err := Validate(cfg); err != nil {
		t.Fatalf("representable upper bound rejected: %v", err)
	}
	cfg.General.RequestTimeoutSecs++
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "request_timeout") {
		t.Fatalf("overflowing request lifetime accepted: %v", err)
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

func TestReadSourcesTracksEveryLoadInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SLB_HOOK_CAUTION_ACTION", "")
	project := t.TempDir()
	opts := LoadOptions{ProjectDir: project}
	read := func() Sources {
		t.Helper()
		sources, err := ReadSources(opts)
		if err != nil {
			t.Fatal(err)
		}
		return sources
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	base := read()
	if !base.Equal(read()) {
		t.Fatal("two reads of unchanged inputs differ")
	}
	projectConfig := filepath.Join(project, ".slb", "config.toml")
	userConfig := filepath.Join(home, ".slb", "config.toml")
	steps := []struct {
		name   string
		change func()
	}{
		{"empty project config appears", func() { write(projectConfig, "") }},
		{"project config edited", func() { write(projectConfig, "[general]\nmin_approvals = 3\n") }},
		{"same-length project edit", func() { write(projectConfig, "[general]\nmin_approvals = 4\n") }},
		{"user config appears", func() { write(userConfig, "[general]\nmin_approvals = 2\n") }},
		{"environment override set", func() { t.Setenv("SLB_HOOK_CAUTION_ACTION", "ask") }},
	}
	previous := base
	for _, step := range steps {
		step.change()
		current := read()
		if current.Equal(previous) {
			t.Fatalf("%s: change not detected", step.name)
		}
		previous = current
	}

	if _, err := ReadSources(LoadOptions{ProjectDir: project, FlagOverrides: map[string]any{"general.min_approvals": 5}}); err == nil {
		t.Fatal("flag overrides were accepted although Sources cannot represent them")
	}
	broken := t.TempDir()
	if err := os.MkdirAll(filepath.Join(broken, ".slb", "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSources(LoadOptions{ProjectDir: broken}); err == nil {
		t.Fatal("a directory at the project config path was read as absent")
	}
}

func TestLoadSourcesParsesTheSnapshotItWasGiven(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SLB_MIN_APPROVALS", "")
	project := t.TempDir()
	projectConfig := filepath.Join(project, ".slb", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[general]\nmin_approvals = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := LoadOptions{ProjectDir: project}
	sources, err := ReadSources(opts)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, snapshot) {
		t.Fatalf("LoadSources disagrees with Load:\n%+v\n%+v", snapshot, direct)
	}

	// Later edits and environment changes do not leak into the snapshot.
	if err := os.WriteFile(projectConfig, []byte("[general]\nmin_approvals = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLB_MIN_APPROVALS", "4")
	again, err := LoadSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	if again.General.MinApprovals != 3 {
		t.Fatalf("snapshot reloaded live inputs: min_approvals = %d", again.General.MinApprovals)
	}
	if live, err := Load(opts); err != nil || live.General.MinApprovals != 4 {
		t.Fatalf("live load = %d, %v; want the environment override 4", live.General.MinApprovals, err)
	}
	if _, err := LoadSources(Sources{}); err == nil {
		t.Fatal("an empty Sources value was loaded as if it had been read")
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

type notificationSettingFixture struct {
	key, env, raw string
	want          any
}

func notificationSettingFixtures() []notificationSettingFixture {
	return []notificationSettingFixture{
		{"notifications.blocked.enabled", "SLB_NOTIFICATIONS_BLOCKED_ENABLED", "true", true},
		{"notifications.blocked.window_seconds", "SLB_NOTIFICATIONS_BLOCKED_WINDOW_SECONDS", "180", 180},
		{"notifications.blocked.repeat_threshold", "SLB_NOTIFICATIONS_BLOCKED_REPEAT_THRESHOLD", "4", 4},
		{"notifications.blocked.cooldown_seconds", "SLB_NOTIFICATIONS_BLOCKED_COOLDOWN_SECONDS", "45", 45},
		{"notifications.blocked.max_per_minute", "SLB_NOTIFICATIONS_BLOCKED_MAX_PER_MINUTE", "7", 7},
		{"notifications.blocked.agent_mail_url", "SLB_NOTIFICATIONS_BLOCKED_AGENT_MAIL_URL", "https://blocked.example.invalid/mcp/", "https://blocked.example.invalid/mcp/"},
		{"notifications.blocked.agent_mail_sender", "SLB_NOTIFICATIONS_BLOCKED_AGENT_MAIL_SENDER", "BlueLake", "BlueLake"},
		{"notifications.blocked.agent_mail_recipients", "SLB_NOTIFICATIONS_BLOCKED_AGENT_MAIL_RECIPIENTS", " GreenCastle, , RedStone, ", []string{"GreenCastle", "RedStone"}},
		{"notifications.blocked.human_recipient", "SLB_NOTIFICATIONS_BLOCKED_HUMAN_RECIPIENT", "GoldenOwl", "GoldenOwl"},
		{"notifications.blocked.blocked_template", "SLB_NOTIFICATIONS_BLOCKED_BLOCKED_TEMPLATE", "~/templates/blocked.md", "~/templates/blocked.md"},
		{"notifications.blocked.repeated_template", "SLB_NOTIFICATIONS_BLOCKED_REPEATED_TEMPLATE", "~/templates/repeated.md", "~/templates/repeated.md"},
		{"notifications.requests.enabled", "SLB_NOTIFICATIONS_REQUESTS_ENABLED", "true", true},
		{"notifications.requests.lookback_seconds", "SLB_NOTIFICATIONS_REQUESTS_LOOKBACK_SECONDS", "3600", 3600},
		{"notifications.requests.max_per_minute", "SLB_NOTIFICATIONS_REQUESTS_MAX_PER_MINUTE", "31", 31},
		{"notifications.requests.agent_mail_url", "SLB_NOTIFICATIONS_REQUESTS_AGENT_MAIL_URL", "https://requests.example.invalid/mcp/", "https://requests.example.invalid/mcp/"},
		{"notifications.requests.agent_mail_sender", "SLB_NOTIFICATIONS_REQUESTS_AGENT_MAIL_SENDER", "SilverFox", "SilverFox"},
		{"notifications.requests.agent_mail_recipients", "SLB_NOTIFICATIONS_REQUESTS_AGENT_MAIL_RECIPIENTS", " AmberWolf, , VioletHill, ", []string{"AmberWolf", "VioletHill"}},
		{"notifications.requests.webhook_url", "SLB_NOTIFICATIONS_REQUESTS_WEBHOOK_URL", "https://webhook.example.invalid/slb", "https://webhook.example.invalid/slb"},
		{"notifications.requests.template", "SLB_NOTIFICATIONS_REQUESTS_TEMPLATE", "~/templates/request.md", "~/templates/request.md"},
	}
}

func notificationConfigEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, binding := range envBindings {
		t.Setenv(binding.Env, "")
	}
}

func TestNotificationConfigKeyRoundTrip(t *testing.T) {
	notificationConfigEnvironment(t)
	for _, tc := range notificationSettingFixtures() {
		t.Run(tc.key, func(t *testing.T) {
			value, err := ParseValue(tc.key, tc.raw)
			if err != nil || !reflect.DeepEqual(value, tc.want) {
				t.Fatalf("ParseValue = %#v, %v; want %#v", value, err, tc.want)
			}
			project := t.TempDir()
			if err := WriteValue(filepath.Join(project, ".slb", "config.toml"), tc.key, value); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(LoadOptions{ProjectDir: project})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := GetValue(cfg, tc.key); !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stored setting = %#v, found=%t; want %#v", got, ok, tc.want)
			}
		})
	}
}

func TestNotificationConfigEnvironmentPrecedenceAndSnapshots(t *testing.T) {
	notificationConfigEnvironment(t)
	for _, tc := range notificationSettingFixtures() {
		t.Run(tc.key, func(t *testing.T) {
			// A distinct same-type value makes file/env/flag precedence visible.
			var alternative any
			switch value := tc.want.(type) {
			case bool:
				alternative = !value
			case int:
				alternative = value + 1
			case string:
				alternative = "file-value"
			case []string:
				alternative = []string{"FileRecipient"}
			default:
				t.Fatalf("unsupported fixture type %T", value)
			}
			project := t.TempDir()
			if err := WriteValue(filepath.Join(project, ".slb", "config.toml"), tc.key, alternative); err != nil {
				t.Fatal(err)
			}
			opts := LoadOptions{ProjectDir: project}
			before, err := ReadSources(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.env, tc.raw)
			after, err := ReadSources(opts)
			if err != nil || before.Equal(after) {
				t.Fatalf("environment change was not captured: %v", err)
			}
			cfg, err := Load(opts)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := GetValue(cfg, tc.key); !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("environment did not override the file: %#v, found=%t", got, ok)
			}
			opts.FlagOverrides = map[string]any{tc.key: alternative}
			cfg, err = Load(opts)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := GetValue(cfg, tc.key); !ok || !reflect.DeepEqual(got, alternative) {
				t.Fatalf("flag did not override environment: %#v, found=%t", got, ok)
			}
			t.Setenv(tc.env, "")
			for _, snapshot := range []struct {
				sources Sources
				want    any
			}{{before, alternative}, {after, tc.want}} {
				cfg, err := LoadSources(snapshot.sources)
				if err != nil {
					t.Fatal(err)
				}
				if got, ok := GetValue(cfg, tc.key); !ok || !reflect.DeepEqual(got, snapshot.want) {
					t.Fatalf("snapshot read live environment: %#v, found=%t; want %#v", got, ok, snapshot.want)
				}
			}
		})
	}
}

func TestNotificationConfigDefaultsAndTypedErrors(t *testing.T) {
	notificationConfigEnvironment(t)
	project := t.TempDir()
	cfg, err := Load(LoadOptions{ProjectDir: project})
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultConfig()
	if cfg.Notifications.Blocked.Enabled || cfg.Notifications.Requests.Enabled ||
		!reflect.DeepEqual(cfg.Notifications, defaults.Notifications) {
		t.Fatalf("registering nested settings changed notification defaults: %+v", cfg.Notifications)
	}
	for _, tc := range notificationSettingFixtures() {
		switch tc.want.(type) {
		case bool, int:
			t.Run(tc.key, func(t *testing.T) {
				if _, err := ParseValue(tc.key, "invalid-typed-value"); err == nil {
					t.Fatal("invalid CLI setting was accepted")
				}
				t.Setenv(tc.env, "invalid-typed-value")
				if _, err := Load(LoadOptions{ProjectDir: project}); err == nil || !strings.Contains(err.Error(), tc.env) {
					t.Fatalf("invalid environment setting lost its typed diagnostic: %v", err)
				}
			})
		}
	}
}

func TestNotificationConfigRecipientsCanBeCleared(t *testing.T) {
	notificationConfigEnvironment(t)
	for _, key := range []string{"notifications.blocked.agent_mail_recipients", "notifications.requests.agent_mail_recipients"} {
		t.Run(key, func(t *testing.T) {
			project := t.TempDir()
			userPath, projectPath := ConfigPaths(project, "")
			if err := WriteValue(userPath, key, []string{"InheritedRecipient"}); err != nil {
				t.Fatal(err)
			}
			value, err := ParseValue(key, " , , ")
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteValue(projectPath, key, value); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(LoadOptions{ProjectDir: project})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := GetValue(cfg, key); !ok || !reflect.DeepEqual(got, []string{}) {
				t.Fatalf("empty project CSV inherited recipients: %#v, found=%t", got, ok)
			}
		})
	}
}

func TestGetValue(t *testing.T) {
	cfg := DefaultConfig()

	cases := []struct {
		key  string
		want any
	}{
		{"general.approval_policy", cfg.General.ApprovalPolicy},
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
		{"notifications.blocked", cfg.Notifications.Blocked},
		{"notifications.requests", cfg.Notifications.Requests},

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
		{"integrations.hook_caution_action", cfg.Integrations.HookCautionAction},
		{"integrations.hook_query_timeout_ms", cfg.Integrations.HookQueryTimeoutMS},

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
		"notifications.blocked.nope",
		"notifications.requests.nope",
		"notifications.blocked.enabled.extra",
		"notifications.requests.agent_mail_recipients.extra",
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
