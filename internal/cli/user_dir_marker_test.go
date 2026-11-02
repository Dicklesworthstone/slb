package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slb's per-user directory (~/.slb) without a state.db marks no project. Its
// daemon.log, config.toml or audit records must neither block every hook
// decision beneath the home directory nor refuse its commands: hooks decide
// as v0.5.2 did and an uninitialized directory creates its first database.
// Each case is compared with a home without ~/.slb (H0) beside it, so a .slb
// above the test's temporary directory moves both sides alike.

// The user config only changes the caution action: a configured pattern list
// would replace that tier's defaults and move unrelated decisions.
const cliUserDirConfig = "[integrations]\nhook_caution_action = \"ask\"\n"

var userDirHookCommands = []string{"npm cache clean", "ls -la", "npm uninstall left-pad", "chmod -R 777 ./x", "terraform destroy"}

// cliUserDirHome creates parent/name with a proj directory and ~/.slb in
// state: H0 none, H1 daemon.log only, H2 config.toml only, HA audit records.
func cliUserDirHome(t *testing.T, parent, name, state string) string {
	t.Helper()
	home := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(home, "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".slb")
	switch state {
	case "H0":
		return home
	case "HA":
		if err := os.MkdirAll(filepath.Join(dir, "audit", "blocked"), 0o700); err != nil {
			t.Fatal(err)
		}
		return home
	}
	file, content := "daemon.log", ""
	switch state {
	case "H1":
	case "H2":
		file, content = "config.toml", cliUserDirConfig
	default:
		t.Fatalf("unknown home state %q", state)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestNativeHookFallbackIgnoresUserDirWithoutState(t *testing.T) {
	resetHookFlags()
	t.Cleanup(resetHookFlags)
	parent := t.TempDir()
	control := cliUserDirHome(t, parent, "control", "H0")
	for _, state := range []string{"H1", "H2", "HA"} {
		t.Run(state, func(t *testing.T) {
			home := cliUserDirHome(t, parent, "home-"+state, state)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			for _, sub := range []string{"proj", "."} {
				for _, command := range userDirHookCommands {
					want, wantErr := classifyNativeHookFallback(command, filepath.Join(control, sub))
					got, err := classifyNativeHookFallback(command, filepath.Join(home, sub))
					if wantErr != nil {
						if err == nil {
							t.Errorf("%s from %s: control failed (%v) but the case decided %+v", command, sub, wantErr, got)
						}
						continue
					}
					if err != nil {
						t.Errorf("%s from %s: classification failed: %v", command, sub, err)
						continue
					}
					action := want.Action
					if state == "H2" && command == "npm uninstall left-pad" {
						// ~/.slb/config.toml is the user's configuration, as in v0.5.2.
						action = "ask"
					}
					if got.Action != action || got.Tier != want.Tier {
						t.Errorf("%s from %s: got %s/%s, want %s/%s", command, sub, got.Action, got.Tier, action, want.Tier)
					}
				}
			}
		})
	}
}

func TestNativeHookGuardDecidesUnderUserDirWithoutState(t *testing.T) {
	resetHookFlags()
	t.Cleanup(resetHookFlags)
	t.Setenv("SLB_SESSION_ID", "")
	parent := t.TempDir()
	control := cliUserDirHome(t, parent, "control", "H0")
	decide := func(home, cwd, command string) (string, string) {
		permission, specific := nativeHookPermission(t, nativeHookOutput(t, map[string]any{
			"tool_name": "Bash", "tool_input": map[string]any{"command": command},
			"cwd": cwd, "session_id": "userdir-test",
		}, home))
		reason, _ := specific["permissionDecisionReason"].(string)
		return permission, reason
	}
	for _, state := range []string{"H1", "H2", "HA"} {
		t.Run(state, func(t *testing.T) {
			home := cliUserDirHome(t, parent, "home-"+state, state)
			t.Setenv("USERPROFILE", home)
			for _, sub := range []string{"proj", "."} {
				for _, command := range userDirHookCommands {
					want, wantReason := decide(home, filepath.Join(control, sub), command)
					got, reason := decide(home, filepath.Join(home, sub), command)
					if state == "H2" && command == "npm uninstall left-pad" && !strings.Contains(wantReason, "classification failed") {
						// ~/.slb/config.toml is the user's configuration, as in v0.5.2.
						want = "ask"
					}
					if got != want {
						t.Errorf("%s from %s: got %s (%s), want %s (%s)", command, sub, got, reason, want, wantReason)
					}
				}
			}
		})
	}
}

func TestCommandsAutoInitializeUnderUserDirWithoutState(t *testing.T) {
	resetPendingFlags()
	t.Cleanup(resetPendingFlags)
	t.Setenv("SLB_PROJECT", "")
	parent := t.TempDir()
	for _, state := range []string{"H1", "H2", "HA"} {
		t.Run(state, func(t *testing.T) {
			control := cliUserDirHome(t, parent, "control-"+state, "H0")
			home := cliUserDirHome(t, parent, "home-"+state, state)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			homeDB := filepath.Join(home, ".slb", "state.db")
			for _, sub := range []string{"proj", "."} {
				resetPendingFlags()
				_, wantErr := executeCommandCapture(t, newTestPendingCmd(""), "pending", "-C", filepath.Join(control, sub), "-j")
				resetPendingFlags()
				_, err := executeCommandCapture(t, newTestPendingCmd(""), "pending", "-C", filepath.Join(home, sub), "-j")
				if wantErr != nil {
					if err == nil {
						t.Errorf("from %s: control failed (%v) but the case succeeded", sub, wantErr)
					}
					continue
				}
				if err != nil {
					t.Errorf("from %s: %v", sub, err)
					continue
				}
				created := filepath.Join(home, sub, ".slb", "state.db")
				if info, err := os.Stat(created); err != nil || !info.Mode().IsRegular() {
					t.Errorf("from %s: %s was not created: %v", sub, created, err)
				}
				if sub == "proj" {
					if _, err := os.Lstat(homeDB); !os.IsNotExist(err) {
						t.Errorf("a command in proj created or found the home database: %v", err)
					}
				}
			}
		})
	}
}
