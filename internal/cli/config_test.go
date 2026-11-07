package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestConfigCmd creates a fresh config command tree for testing.
func newTestConfigCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file")

	// Create fresh config commands
	cfgCmd := &cobra.Command{
		Use:   "config",
		Short: "Show or modify SLB configuration",
		RunE:  configCmd.RunE,
	}
	cfgCmd.PersistentFlags().BoolVar(&flagConfigGlobal, "global", false, "operate on user config")

	getCmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Get a specific configuration value",
		Args:  cobra.ExactArgs(1),
		RunE:  configGetCmd.RunE,
	}

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Args:  cobra.ExactArgs(2),
		RunE:  configSetCmd.RunE,
	}

	cfgCmd.AddCommand(getCmd, setCmd)
	root.AddCommand(cfgCmd)

	return root
}

func resetConfigFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagConfig = ""
	flagConfigGlobal = false
}

func TestConfigCommand_ShowsConfig(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "config", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should have general config section (may be "general" or "General" depending on struct tags)
	hasGeneral := false
	for key := range result {
		if strings.ToLower(key) == "general" {
			hasGeneral = true
			break
		}
	}
	if !hasGeneral {
		t.Errorf("expected config to have 'general' section, got keys: %v", result)
	}
}

func TestConfigGetCommand_RequiresKey(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "config", "get")

	if err == nil {
		t.Fatal("expected error when key is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigGetCommand_GetsValue(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "config", "get", "general.min_approvals", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["key"] != "general.min_approvals" {
		t.Errorf("expected key=general.min_approvals, got %v", result["key"])
	}
	if result["value"] == nil {
		t.Error("expected value to be set")
	}
}

func TestConfigGetCommand_UnknownKey(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "config", "get", "nonexistent.key", "-C", h.ProjectDir, "-j")

	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigSetCommand_RequiresKeyAndValue(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "config", "set", "some.key")

	if err == nil {
		t.Fatal("expected error when value is missing")
	}
	if !strings.Contains(err.Error(), "accepts 2 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigSetCommand_SetsValue(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "config", "set", "general.min_approvals", "3", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["key"] != "general.min_approvals" {
		t.Errorf("expected key=general.min_approvals, got %v", result["key"])
	}
	// Value should be set (could be string or number depending on implementation)
	if result["value"] == nil {
		t.Error("expected value to be set")
	}
}

func notificationConfigCLIEnvironment(t *testing.T) (string, string) {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(name, "SLB_NOTIFICATIONS_") {
			t.Setenv(name, "")
		}
	}
	t.Cleanup(resetConfigFlags)
	return home, project
}

func runNotificationConfigCLI(t *testing.T, project string, args ...string) map[string]any {
	t.Helper()
	resetConfigFlags()
	stdout, err := executeCommandCapture(t, newTestConfigCmd(""), append(args, "-C", project, "-j")...)
	if err != nil {
		t.Fatalf("config command failed: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid config JSON: %s, %v", stdout, err)
	}
	return result
}

func TestConfigNotificationSettingsScopes(t *testing.T) {
	for _, scope := range []string{"project", "global", "explicit", "global with explicit"} {
		t.Run(scope, func(t *testing.T) {
			home, project := notificationConfigCLIEnvironment(t)
			userPath := filepath.Join(home, ".slb", "config.toml")
			projectPath := filepath.Join(project, ".slb", "config.toml")
			explicitPath := filepath.Join(t.TempDir(), "selected.toml")
			paths := map[string]int{userPath: 3, projectPath: 4, explicitPath: 5}
			for path, approvals := range paths {
				if err := config.WriteValue(path, "general.min_approvals", approvals); err != nil {
					t.Fatal(err)
				}
			}
			target, options := projectPath, []string{}
			switch scope {
			case "global":
				target, options = userPath, []string{"--global"}
			case "explicit":
				target, options = explicitPath, []string{"--config", explicitPath}
			case "global with explicit":
				target, options = userPath, []string{"--global", "--config", explicitPath}
			}
			for _, tc := range []struct {
				key, raw string
				want     any
			}{
				{"notifications.blocked.enabled", "true", true},
				{"notifications.blocked.window_seconds", "180", 180},
				{"notifications.blocked.agent_mail_sender", "BlueLake", "BlueLake"},
				{"notifications.blocked.agent_mail_recipients", " GreenCastle, , RedStone, ", []string{"GreenCastle", "RedStone"}},
				{"notifications.requests.enabled", "true", true},
				{"notifications.requests.lookback_seconds", "3600", 3600},
				{"notifications.requests.agent_mail_sender", "SilverFox", "SilverFox"},
				{"notifications.requests.agent_mail_recipients", " AmberWolf, VioletHill ", []string{"AmberWolf", "VioletHill"}},
			} {
				before := make(map[string]string)
				for path := range paths {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					before[path] = string(data)
				}
				result := runNotificationConfigCLI(t, project, append([]string{"config", "set", tc.key, tc.raw}, options...)...)
				if result["path"] != target || result["key"] != tc.key {
					t.Fatalf("config set chose the wrong scope: %+v", result)
				}
				var stored config.Config
				if _, err := toml.DecodeFile(target, &stored); err != nil {
					t.Fatal(err)
				}
				if value, ok := config.GetValue(stored, tc.key); !ok || !reflect.DeepEqual(value, tc.want) || stored.General.MinApprovals != paths[target] {
					t.Fatalf("setting lost its TOML type or changed unrelated policy: value=%#v cfg=%+v", value, stored)
				}
				for path := range paths {
					if path == target {
						continue
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != before[path] {
						t.Fatalf("writing %s changed another config file %s: %v", target, path, err)
					}
				}
				result = runNotificationConfigCLI(t, project, append([]string{"config", "get", tc.key}, options...)...)
				wantJSON, err := json.Marshal(tc.want)
				if err != nil {
					t.Fatal(err)
				}
				gotJSON, err := json.Marshal(result["value"])
				if err != nil || string(gotJSON) != string(wantJSON) {
					t.Fatalf("effective getter lost the saved value: %+v, %v", result, err)
				}
			}
		})
	}
}

func TestConfigNotificationGetUsesEffectivePrecedence(t *testing.T) {
	home, project := notificationConfigCLIEnvironment(t)
	key := "notifications.requests.agent_mail_sender"
	explicitPath := filepath.Join(t.TempDir(), "selected.toml")
	for path, value := range map[string]string{
		filepath.Join(home, ".slb", "config.toml"):    "UserSender",
		filepath.Join(project, ".slb", "config.toml"): "ProjectSender",
		explicitPath: "ExplicitSender",
	} {
		if err := config.WriteValue(path, key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		options []string
		want    string
	}{
		{nil, "ProjectSender"},
		{[]string{"--global"}, "ProjectSender"},
		{[]string{"--config", explicitPath}, "ExplicitSender"},
		{[]string{"--global", "--config", explicitPath}, "ExplicitSender"},
	} {
		result := runNotificationConfigCLI(t, project, append([]string{"config", "get", key}, tc.options...)...)
		if result["value"] != tc.want {
			t.Fatalf("effective get precedence changed for %v: %+v", tc.options, result)
		}
	}
	t.Setenv("SLB_NOTIFICATIONS_REQUESTS_AGENT_MAIL_SENDER", "EnvironmentSender")
	result := runNotificationConfigCLI(t, project, "config", "get", key, "--global", "--config", explicitPath)
	if result["value"] != "EnvironmentSender" {
		t.Fatalf("effective get ignored environment: %+v", result)
	}
}

func TestConfigNotificationInvalidTypedValuesDoNotWrite(t *testing.T) {
	_, project := notificationConfigCLIEnvironment(t)
	path := filepath.Join(project, ".slb", "config.toml")
	if err := config.WriteValue(path, "general.min_approvals", 3); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"notifications.blocked.enabled", "notifications.blocked.repeat_threshold",
		"notifications.requests.enabled", "notifications.requests.lookback_seconds",
		"notifications.blocked.unknown", "notifications.requests.agent_mail_token",
	} {
		resetConfigFlags()
		if _, err := executeCommandCapture(t, newTestConfigCmd(""), "config", "set", key, "invalid-value", "-C", project, "-j"); err == nil {
			t.Fatalf("invalid or unsupported setting %q was accepted", key)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("invalid setting %q changed configuration: %v", key, err)
		}
	}
}

func TestConfigCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetConfigFlags()

	cmd := newTestConfigCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "config", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "config") {
		t.Error("expected help to mention 'config'")
	}
	if !strings.Contains(stdout, "get") {
		t.Error("expected help to mention 'get' subcommand")
	}
	if !strings.Contains(stdout, "set") {
		t.Error("expected help to mention 'set' subcommand")
	}
	if !strings.Contains(stdout, "--global") {
		t.Error("expected help to mention '--global' flag")
	}
}
