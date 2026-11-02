package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// slb's per-user directory (~/.slb) holds daemon.log, the user config.toml,
// generated hooks and audit records. Without a state.db it marks no project:
// a home holding only those must select projects exactly as a home without
// ~/.slb does, as v0.5.2 did. Each case is compared with such a home (H0)
// beside it, so a .slb above the test's temporary directory, such as the
// runner's own ~/.slb, moves both sides alike.

// The user config only changes the caution action: a configured pattern list
// would replace that tier's defaults and move unrelated decisions.
const userDirCautionConfig = "[integrations]\nhook_caution_action = \"ask\"\n"

// userDirHome creates parent/name with a proj directory and ~/.slb in state:
// H0 none, H1 daemon.log only, H2 config.toml only, HA audit records only,
// H3 an initialized state.db.
func userDirHome(t *testing.T, parent, name, state string) string {
	t.Helper()
	home := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(home, "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".slb")
	write := func(file, content string) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	switch state {
	case "H0":
	case "H1":
		write("daemon.log", "")
	case "H2":
		write("config.toml", userDirCautionConfig)
	case "HA":
		if err := os.MkdirAll(filepath.Join(dir, "audit", "blocked"), 0o700); err != nil {
			t.Fatal(err)
		}
	case "H3":
		conn, err := db.OpenAndMigrate(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown home state %q", state)
	}
	return home
}

func setUserHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func physicalPath(t *testing.T, path string) string {
	t.Helper()
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return physical
}

// likeControl maps the H0 control's root onto home: a root inside control is
// the same place inside home; a root above control is shared by both.
func likeControl(controlRoot, control, home string) string {
	rel, err := filepath.Rel(control, controlRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return controlRoot
	}
	return filepath.Join(home, rel)
}

func TestUserDirWithoutStateIsNotAProjectMarker(t *testing.T) {
	parent := t.TempDir()
	control := userDirHome(t, parent, "control", "H0")
	for _, state := range []string{"H1", "H2", "HA"} {
		t.Run(state, func(t *testing.T) {
			home := userDirHome(t, parent, "home-"+state, state)
			setUserHome(t, home)
			for _, sub := range []string{"proj", "."} {
				controlRoot, err := PhysicalProjectRootForCWD(filepath.Join(control, sub))
				if err != nil {
					t.Fatal(err)
				}
				want := likeControl(controlRoot, physicalPath(t, control), physicalPath(t, home))
				if got, err := PhysicalProjectRootForCWD(filepath.Join(home, sub)); err != nil || got != want {
					t.Errorf("physical root from %s = %q, %v; want %q as without ~/.slb", sub, got, err, want)
				}
				want = likeControl(ProjectRootForCWD(filepath.Join(control, sub)), control, home)
				if got := ProjectRootForCWD(filepath.Join(home, sub)); got != want {
					t.Errorf("socket root from %s = %q; want %q as without ~/.slb", sub, got, want)
				}
			}
			// The home itself is then uninitialized, as with no ~/.slb at all.
			want := filepath.Join(home, ".slb", "state.db")
			if got, err := db.ProjectDatabasePath(home); err != nil || got != want {
				t.Errorf("home database = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestInitializedHomeStillOwnsItsDescendants(t *testing.T) {
	home := userDirHome(t, t.TempDir(), "home", "H3")
	setUserHome(t, home)
	if got, err := PhysicalProjectRootForCWD(filepath.Join(home, "proj")); err != nil || got != physicalPath(t, home) {
		t.Fatalf("physical root = %q, %v; want the initialized home", got, err)
	}
	if got := ProjectRootForCWD(filepath.Join(home, "proj")); got != home {
		t.Fatalf("socket root = %q; want the initialized home", got)
	}
}

func TestNestedProjectUnderUserDirKeepsItsMarker(t *testing.T) {
	home := userDirHome(t, t.TempDir(), "home", "H1")
	setUserHome(t, home)
	initialized := filepath.Join(home, "proj")
	conn, err := db.OpenAndMigrate(filepath.Join(initialized, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// A marker that is not slb's per-user directory keeps 98b7cf7's refusal.
	stateless := filepath.Join(home, "lost")
	if err := os.MkdirAll(filepath.Join(stateless, ".slb"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{initialized, stateless} {
		sub := filepath.Join(project, "src")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if got, err := PhysicalProjectRootForCWD(sub); err != nil || got != physicalPath(t, project) {
			t.Errorf("physical root from %q = %q, %v; want its project", sub, got, err)
		}
		if got := ProjectRootForCWD(sub); got != project {
			t.Errorf("socket root from %q = %q; want its project", sub, got)
		}
	}
	if _, err := db.ProjectDatabasePath(stateless); err == nil {
		t.Error("a project marker without state.db was not refused")
	}
}

func TestForeignStatelessMarkerIsStillAProject(t *testing.T) {
	for _, state := range []string{"H0", "H1"} {
		t.Run("user home "+state, func(t *testing.T) {
			parent := t.TempDir()
			setUserHome(t, userDirHome(t, parent, "home", state))
			// Shaped like a per-user directory, but not this user's.
			other := userDirHome(t, parent, "other", "H1")
			sub := filepath.Join(other, "proj")
			if got, err := PhysicalProjectRootForCWD(sub); err != nil || got != physicalPath(t, other) {
				t.Errorf("physical root = %q, %v; want the marked directory", got, err)
			}
			if got := ProjectRootForCWD(sub); got != other {
				t.Errorf("socket root = %q; want the marked directory", got)
			}
			if _, err := db.ProjectDatabasePath(other); err == nil {
				t.Error("a stateless marker that is not the user's directory was not refused")
			}
		})
	}
}

func TestUserDirMatchesThroughAliasedHome(t *testing.T) {
	parent := t.TempDir()
	control := userDirHome(t, parent, "control", "H0")
	real := userDirHome(t, parent, "real-home", "H1")
	link := filepath.Join(parent, "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	setUserHome(t, link)
	controlRoot, err := PhysicalProjectRootForCWD(filepath.Join(control, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	want := likeControl(controlRoot, physicalPath(t, control), physicalPath(t, real))
	for _, start := range []string{filepath.Join(real, "proj"), filepath.Join(link, "proj")} {
		if got, err := PhysicalProjectRootForCWD(start); err != nil || got != want {
			t.Errorf("physical root from %q = %q, %v; want %q", start, got, err, want)
		}
	}
	want = likeControl(ProjectRootForCWD(filepath.Join(control, "proj")), control, link)
	if got := ProjectRootForCWD(filepath.Join(link, "proj")); got != want {
		t.Errorf("socket root through the alias = %q; want %q", got, want)
	}
	want = likeControl(ProjectRootForCWD(filepath.Join(control, "proj")), control, real)
	if got := ProjectRootForCWD(filepath.Join(real, "proj")); got != want {
		t.Errorf("socket root from the physical home = %q; want %q", got, want)
	}
}

func TestDaemonHookPolicyIgnoresUserDirWithoutState(t *testing.T) {
	parent := t.TempDir()
	control := userDirHome(t, parent, "control", "H0")
	commands := []string{"npm cache clean", "ls -la", "npm uninstall left-pad", "chmod -R 777 ./x", "terraform destroy"}
	for _, state := range []string{"H1", "H2", "HA"} {
		t.Run(state, func(t *testing.T) {
			home := userDirHome(t, parent, "home-"+state, state)
			setUserHome(t, home)
			srv := &IPCServer{}
			for _, sub := range []string{"proj", "."} {
				for _, command := range commands {
					want := srv.classifyCommand(HookQueryParams{Command: command, CWD: filepath.Join(control, sub)})
					wantAction, wantTier := want.Action, want.Tier
					if state == "H2" && command == "npm uninstall left-pad" && want.MatchedPattern != "policy_load_error" {
						// ~/.slb/config.toml is the user's configuration, as in v0.5.2.
						wantAction = "ask"
					}
					got := srv.classifyCommand(HookQueryParams{Command: command, CWD: filepath.Join(home, sub)})
					if got.Action != wantAction || got.Tier != wantTier {
						t.Errorf("%s from %s: got %s/%s (%s), want %s/%s", command, sub,
							got.Action, got.Tier, got.MatchedPattern, wantAction, wantTier)
					}
				}
			}
		})
	}
}
