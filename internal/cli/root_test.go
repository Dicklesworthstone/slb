package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// executeCommand runs a cobra command with the given args and returns stdout, stderr, and error.
func executeCommand(root *cobra.Command, args ...string) (stdout string, stderr string, err error) {
	stdoutBuf := new(bytes.Buffer)
	stderrBuf := new(bytes.Buffer)

	root.SetOut(stdoutBuf)
	root.SetErr(stderrBuf)
	root.SetArgs(args)

	err = root.Execute()

	return stdoutBuf.String(), stderrBuf.String(), err
}

// newTestRootCmd creates a fresh root command for testing (avoids state pollution).
func newTestRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "slb",
		Short:         "Simultaneous Launch Button - Two-person rule for dangerous commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Add persistent flags
	cmd.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file path")
	cmd.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format: text, json, yaml")
	cmd.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "shorthand for --output=json")
	cmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")
	cmd.PersistentFlags().StringVar(&flagDB, "db", "", "database path")
	cmd.PersistentFlags().StringVar(&flagActor, "actor", "", "actor identifier")
	cmd.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")
	cmd.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Add version command
	versionCmdTest := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if flagJSON || flagOutput == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]string{
					"version": version,
					"commit":  commit,
				})
			}
			_, err := out.Write([]byte("slb " + version + "\n"))
			return err
		},
	}
	cmd.AddCommand(versionCmdTest)

	return cmd
}

func TestRootRelativeProjectFlagKeepsSelectedAbsolutePath(t *testing.T) {
	for _, selection := range []string{".", "subdir"} {
		t.Run(selection, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(base, "subdir"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(base) // Restores the caller's cwd, including on test failure.
			previous := flagProject
			t.Cleanup(func() { flagProject = previous })
			command := &cobra.Command{Use: "slb", PersistentPreRunE: rootCmd.PersistentPreRunE}
			command.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
			var selectedProject, selectedCWD string
			command.AddCommand(&cobra.Command{
				Use: "probe", Args: cobra.NoArgs,
				RunE: func(*cobra.Command, []string) error {
					var err error
					if selectedCWD, err = os.Getwd(); err != nil {
						return err
					}
					selectedProject, err = projectPath()
					return err
				},
			})
			if _, _, err := executeCommand(command, "-C", selection, "probe"); err != nil {
				t.Fatal(err)
			}
			expected := filepath.Join(base, selection)
			if selectedProject != expected || selectedCWD != expected || flagProject != expected || !filepath.IsAbs(selectedProject) {
				t.Fatalf("relative project was lost or resolved twice: project=%q cwd=%q flag=%q want=%q", selectedProject, selectedCWD, flagProject, expected)
			}
		})
	}
}

func TestRootCommand_ShowsHelp(t *testing.T) {
	cmd := newTestRootCmd()
	stdout, _, err := executeCommand(cmd, "--help")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !strings.Contains(stdout, "Simultaneous Launch Button") {
		t.Error("expected help to contain 'Simultaneous Launch Button'")
	}
	if !strings.Contains(stdout, "Available Commands") {
		t.Error("expected help to list available commands")
	}
}

func TestRootCommand_GlobalFlags(t *testing.T) {
	cmd := newTestRootCmd()

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"help flag short", []string{"-h"}, false},
		{"help flag long", []string{"--help"}, false},
		{"config flag", []string{"--config", "/tmp/test.toml", "--help"}, false},
		{"output flag json", []string{"--output", "json", "--help"}, false},
		{"output flag yaml", []string{"--output", "yaml", "--help"}, false},
		{"output flag text", []string{"--output", "text", "--help"}, false},
		{"json shorthand", []string{"-j", "--help"}, false},
		{"verbose flag", []string{"-v", "--help"}, false},
		{"db flag", []string{"--db", "/tmp/test.db", "--help"}, false},
		{"actor flag", []string{"--actor", "test-actor", "--help"}, false},
		{"session-id flag", []string{"-s", "sess-123", "--help"}, false},
		{"project flag", []string{"-C", "/tmp/project", "--help"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset flags before each test
			flagConfig = ""
			flagOutput = "text"
			flagJSON = false
			flagVerbose = false
			flagDB = ""
			flagActor = ""
			flagSessionID = ""
			flagProject = ""

			_, _, err := executeCommand(cmd, tt.args...)
			if (err != nil) != tt.wantErr {
				t.Errorf("executeCommand() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestProductionCommandTree_NoLocalFlagCollidesWithPersistent walks the REAL
// rootCmd tree (every command and subcommand actually wired into the binary)
// and asserts that no command-local flag reuses a shorthand — or silently
// re-purposes a long name — that an ancestor's persistent flag already owns.
//
// This is a regression guard for issue #7. Two distinct failure modes were
// shipped at once:
//
//  1. Hard shorthand collision: `slb execute` redefined -t for --timeout while
//     the root persistent --toon already owns -t. cobra/pflag refuses to merge
//     the duplicate shorthand and PANICS at runtime (`slb execute --help`
//     crashed). approve/reject/execute/watch did the same with -s
//     (--session-id), and hook/patterns with -o (--output).
//
//  2. Silent long-name shadowing with a DIFFERENT meaning: `hook generate` and
//     `patterns export` defined a local --output bound to a path, shadowing the
//     persistent --output that selects the text/json/yaml/toon FORMAT. The
//     persistent format flag became unreachable on those subcommands
//     (`slb hook generate -o json` wrote to a dir named "json" instead of
//     emitting JSON). Those locals are now --output-dir / --output-file.
//
// Walking the production tree (not a hand-rolled one) is essential: the bug
// only manifests on the wired-up commands, and a fresh test tree could drift.
func TestProductionCommandTree_NoLocalFlagCollidesWithPersistent(t *testing.T) {
	// A live shorthand collision makes cobra/pflag PANIC the first time the
	// offending command's flags are merged (which the LocalFlags() call below
	// triggers). Recover so the guard reports a clean, actionable failure
	// instead of an opaque stack trace.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("walking the production command tree panicked, which means a "+
				"local flag reuses a persistent flag's shorthand (issue #7 regression): %v", r)
		}
	}()

	// inheritedShorthand/inheritedLong map an inherited persistent flag's
	// shorthand/long-name to the "owner --flag" string for diagnostics.
	var walk func(cmd *cobra.Command, inheritedShorthand, inheritedLong map[string]string)
	walk = func(cmd *cobra.Command, inheritedShorthand, inheritedLong map[string]string) {
		// Check this command's LOCAL (non-inherited) flags against everything
		// an ancestor's persistent flags already claim.
		cmd.LocalFlags().VisitAll(func(flag *pflag.Flag) {
			if flag.Shorthand != "" {
				if owner, ok := inheritedShorthand[flag.Shorthand]; ok {
					t.Errorf("%s: local flag --%s reuses shorthand -%s already owned by persistent flag %s "+
						"(cobra/pflag panics or silently shadows on this command)",
						cmd.CommandPath(), flag.Name, flag.Shorthand, owner)
				}
			}
			if owner, ok := inheritedLong[flag.Name]; ok {
				// A local long flag with the SAME name as an inherited
				// persistent one shadows it entirely. That is only safe when
				// the two are intentionally the same knob (e.g. a per-command
				// --session-id that intentionally overrides the persistent one
				// of the same name). It is a BUG when the local flag means
				// something different — caught here by name: a local --output
				// (path) shadowing the persistent --output (format) must be
				// renamed (--output-dir/--output-file), which removes it from
				// this collision set.
				if flag.Name == "output" {
					t.Errorf("%s: local flag --%s shadows persistent flag %s which has a different meaning "+
						"(output FORMAT vs a path); rename the local flag (e.g. --output-dir/--output-file)",
						cmd.CommandPath(), flag.Name, owner)
				}
			}
		})

		// Extend the inherited sets with THIS command's persistent flags for
		// the descent into children.
		nextShorthand := make(map[string]string, len(inheritedShorthand))
		for k, v := range inheritedShorthand {
			nextShorthand[k] = v
		}
		nextLong := make(map[string]string, len(inheritedLong))
		for k, v := range inheritedLong {
			nextLong[k] = v
		}
		cmd.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
			owner := cmd.CommandPath() + " --" + flag.Name
			if flag.Shorthand != "" {
				nextShorthand[flag.Shorthand] = owner
			}
			nextLong[flag.Name] = owner
		})

		for _, child := range cmd.Commands() {
			walk(child, nextShorthand, nextLong)
		}
	}

	walk(rootCmd, map[string]string{}, map[string]string{})
}

func TestVersionCommand_TextOutput(t *testing.T) {
	// Reset flags
	flagJSON = false
	flagOutput = "text"

	cmd := newTestRootCmd()
	stdout, _, err := executeCommand(cmd, "version")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !strings.Contains(stdout, "slb") {
		t.Errorf("expected version output to contain 'slb', got %q", stdout)
	}
}

func TestVersionCommand_JSONOutput(t *testing.T) {
	// Reset flags
	flagJSON = false
	flagOutput = "text"

	cmd := newTestRootCmd()
	stdout, _, err := executeCommand(cmd, "version", "-j")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v", err)
	}

	if _, ok := result["version"]; !ok {
		t.Error("expected JSON output to contain 'version' key")
	}
}

func TestGetOutput(t *testing.T) {
	tests := []struct {
		name       string
		flagJSON   bool
		flagOutput string
		want       string
	}{
		{"json flag overrides", true, "text", "json"},
		{"output flag text", false, "text", "text"},
		{"output flag json", false, "json", "json"},
		{"output flag yaml", false, "yaml", "yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flagJSON = tt.flagJSON
			flagOutput = tt.flagOutput
			if got := GetOutput(); got != tt.want {
				t.Errorf("GetOutput() = %v, want %v", got, tt.want)
			}
		})
	}

	// Reset
	flagJSON = false
	flagOutput = "text"
}

func TestGetDB(t *testing.T) {
	// Save original values
	origDB := flagDB
	origProject := flagProject
	defer func() {
		flagDB = origDB
		flagProject = origProject
	}()

	t.Run("explicit db flag", func(t *testing.T) {
		flagDB = "/custom/path/db.sqlite"
		flagProject = ""
		got, err := GetDB()
		if err != nil {
			t.Fatal(err)
		}
		if got != "/custom/path/db.sqlite" {
			t.Errorf("GetDB() = %v, want /custom/path/db.sqlite", got)
		}
	})

	t.Run("falls back to project path", func(t *testing.T) {
		h := testutil.NewHarness(t)
		flagDB = ""
		flagProject = h.ProjectDir

		// Change to project dir temporarily
		origWd, _ := os.Getwd()
		_ = os.Chdir(h.ProjectDir)
		defer func() { _ = os.Chdir(origWd) }()

		got, err := GetDB()
		if err != nil {
			t.Fatal(err)
		}
		expected := filepath.Join(h.ProjectDir, ".slb", "state.db")
		if got != expected {
			t.Errorf("GetDB() = %v, want %v", got, expected)
		}
	})

	t.Run("falls back to cwd when project flag empty", func(t *testing.T) {
		flagDB = ""
		flagProject = ""

		// Change to temp directory - projectPath() will return cwd
		origWd, _ := os.Getwd()
		tmpDir := os.TempDir()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		got, err := GetDB()
		if err != nil {
			t.Fatal(err)
		}
		// When in a directory, GetDB returns path based on cwd
		// Resolve symlinks for comparison (macOS /var -> /private/var)
		tmpDirResolved, _ := filepath.EvalSymlinks(tmpDir)
		expected := filepath.Join(tmpDirResolved, ".slb", "state.db")
		if got != expected {
			t.Errorf("GetDB() = %v, want %v", got, expected)
		}
	})
}

func TestEnterProjectDir_RelativeFlagResolvesOnce(t *testing.T) {
	origDB := flagDB
	origProject := flagProject
	defer func() {
		flagDB = origDB
		flagProject = origProject
	}()

	parent := t.TempDir()
	project := filepath.Join(parent, "sub")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	flagDB = ""
	flagProject = "sub"

	if err := enterProjectDir(); err != nil {
		t.Fatalf("enterProjectDir: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	physicalProject, _ := filepath.EvalSymlinks(project)
	physicalWd, _ := filepath.EvalSymlinks(wd)
	if physicalWd != physicalProject {
		t.Fatalf("cwd = %s, want %s", wd, project)
	}
	if got, err := projectPath(); err != nil || got != project {
		t.Fatalf("projectPath() = %q, %v; want %q", got, err, project)
	}
	if got, err := GetDB(); err != nil || got != filepath.Join(project, ".slb", "state.db") {
		t.Fatalf("GetDB() = %s, %v; want project database", got, err)
	}
}

func TestGetActor(t *testing.T) {
	// Save original values
	origActor := flagActor
	origSLBActor := os.Getenv("SLB_ACTOR")
	origAgentName := os.Getenv("AGENT_NAME")
	origUser := os.Getenv("USER")
	defer func() {
		flagActor = origActor
		os.Setenv("SLB_ACTOR", origSLBActor)
		os.Setenv("AGENT_NAME", origAgentName)
		os.Setenv("USER", origUser)
	}()

	t.Run("explicit actor flag", func(t *testing.T) {
		flagActor = "test-actor"
		got := GetActor()
		if got != "test-actor" {
			t.Errorf("GetActor() = %v, want test-actor", got)
		}
	})

	t.Run("SLB_ACTOR env var", func(t *testing.T) {
		flagActor = ""
		os.Setenv("SLB_ACTOR", "env-actor")
		os.Setenv("AGENT_NAME", "")
		got := GetActor()
		if got != "env-actor" {
			t.Errorf("GetActor() = %v, want env-actor", got)
		}
	})

	t.Run("AGENT_NAME env var", func(t *testing.T) {
		flagActor = ""
		os.Setenv("SLB_ACTOR", "")
		os.Setenv("AGENT_NAME", "agent-name-from-env")
		got := GetActor()
		if got != "agent-name-from-env" {
			t.Errorf("GetActor() = %v, want agent-name-from-env", got)
		}
	})

	t.Run("fallback to user@hostname", func(t *testing.T) {
		flagActor = ""
		os.Setenv("SLB_ACTOR", "")
		os.Setenv("AGENT_NAME", "")
		got := GetActor()
		if !strings.Contains(got, "@") {
			t.Errorf("GetActor() = %v, expected user@hostname format", got)
		}
	})

	t.Run("fallback with empty USER", func(t *testing.T) {
		flagActor = ""
		os.Setenv("SLB_ACTOR", "")
		os.Setenv("AGENT_NAME", "")
		os.Setenv("USER", "")
		got := GetActor()
		// Should fallback to "unknown@hostname"
		if !strings.HasPrefix(got, "unknown@") {
			t.Errorf("GetActor() = %v, expected to start with 'unknown@'", got)
		}
	})
}

func TestGetDBResolutionErrorNeverSelectsHomeHistory(t *testing.T) {
	origDB := flagDB
	origProject := flagProject
	defer func() {
		flagDB = origDB
		flagProject = origProject
	}()

	flagDB = ""
	flagProject = filepath.Join(t.TempDir(), "missing-project")
	if result, err := GetDB(); err == nil || result != "" {
		t.Fatalf("invalid project silently selected another store: %q %v", result, err)
	}

	flagDB = filepath.Join(t.TempDir(), "explicit.db")
	if result, err := GetDB(); err != nil || result != flagDB {
		t.Fatalf("explicit --db must remain authoritative: %q %v", result, err)
	}
}

func TestProjectDiscoveryUsesAncestorWithoutChangingCommandCWD(t *testing.T) {
	origProject, origDB := flagProject, flagDB
	t.Cleanup(func() { flagProject, flagDB = origProject, origDB })
	h := testutil.NewHarness(t)
	other := testutil.NewHarness(t)
	child := filepath.Join(h.ProjectDir, "src", "deep")
	otherChild := filepath.Join(other.ProjectDir, "src")
	for _, dir := range []string{child, otherChild} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name, selectedFlag, selectedEnv, wantProject string
	}{
		{name: "cwd", wantProject: h.ProjectDir},
		{name: "project flag descendant", selectedFlag: child, wantProject: h.ProjectDir},
		{name: "environment descendant", selectedEnv: otherChild, wantProject: other.ProjectDir},
		{name: "flag overrides environment", selectedFlag: child, selectedEnv: otherChild, wantProject: h.ProjectDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(child)
			t.Setenv("SLB_PROJECT", tc.selectedEnv)
			flagProject, flagDB = tc.selectedFlag, ""
			before, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if project, err := projectPath(); err != nil || project != tc.wantProject {
				t.Fatalf("projectPath() = %q, %v; want %q", project, err, tc.wantProject)
			}
			wantDB := filepath.Join(tc.wantProject, ".slb", "state.db")
			if path, err := GetDB(); err != nil || path != wantDB {
				t.Fatalf("GetDB() = %q, %v; want %q", path, err, wantDB)
			}
			if after, err := os.Getwd(); err != nil || after != before {
				t.Fatalf("discovery changed the command cwd: before=%q after=%q error=%v", before, after, err)
			}
			if _, err := os.Lstat(filepath.Join(child, ".slb")); !os.IsNotExist(err) {
				t.Fatalf("discovery created a shadow project: %v", err)
			}
		})
	}
}

func TestProjectDiscoveryKeepsNestedProjectsIsolated(t *testing.T) {
	origProject, origDB := flagProject, flagDB
	t.Cleanup(func() { flagProject, flagDB = origProject, origDB })
	t.Setenv("SLB_PROJECT", "")
	h := testutil.NewHarness(t)
	nested := filepath.Join(h.ProjectDir, "nested")
	child := filepath.Join(nested, "src")
	if err := os.MkdirAll(filepath.Join(nested, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	wantDB := filepath.Join(nested, ".slb", "state.db")
	testutil.NewTestDBAtPath(t, wantDB)
	flagProject, flagDB = child, ""
	if project, err := projectPath(); err != nil || project != nested {
		t.Fatalf("nested project escaped to parent: %q %v", project, err)
	}
	if path, err := GetDB(); err != nil || path != wantDB {
		t.Fatalf("nested database escaped to parent: %q %v", path, err)
	}
}

func TestProjectDiscoveryPreservesOnlyEquivalentRootAliases(t *testing.T) {
	h := testutil.NewHarness(t)
	other := testutil.NewHarness(t)
	otherChild := filepath.Join(other.ProjectDir, "src", "deep")
	if err := os.MkdirAll(otherChild, 0700); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(h.ProjectDir, "other-root")
	descendantAlias := filepath.Join(h.ProjectDir, "other-descendant")
	if err := os.Symlink(other.ProjectDir, rootAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(otherChild), descendantAlias); err != nil {
		t.Fatal(err)
	}
	physicalOther, err := filepath.EvalSymlinks(other.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ start, want string }{
		{filepath.Join(rootAlias, "src", "deep"), rootAlias},
		{filepath.Join(descendantAlias, "deep"), physicalOther},
	} {
		if project, err := resolveProjectRoot(tc.start); err != nil || project != tc.want {
			t.Errorf("resolveProjectRoot(%q) = %q, %v; want %q", tc.start, project, err, tc.want)
		}
	}
}

func TestGetDBRefusesUnavailableNestedState(t *testing.T) {
	origProject, origDB := flagProject, flagDB
	t.Cleanup(func() { flagProject, flagDB = origProject, origDB })
	t.Setenv("SLB_PROJECT", "")
	for _, kind := range []string{"marker file", "dangling marker", "missing database", "database directory", "unreadable database", "inaccessible marker"} {
		t.Run(kind, func(t *testing.T) {
			h := testutil.NewHarness(t)
			nested := filepath.Join(h.ProjectDir, "nested")
			child := filepath.Join(nested, "src")
			if err := os.MkdirAll(child, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(nested, ".slb")
			state := filepath.Join(marker, "state.db")
			switch kind {
			case "marker file":
				if err := os.WriteFile(marker, []byte("invalid marker"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling marker":
				if err := os.Symlink(filepath.Join(nested, "missing-state"), marker); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			default:
				if err := os.Mkdir(marker, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "database directory" {
					if err := os.Mkdir(state, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "unreadable database" {
					if err := os.WriteFile(state, nil, 0000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(state, 0600) })
					if file, err := os.Open(state); err == nil {
						_ = file.Close()
						t.Skip("current user bypasses file permissions")
					}
				}
				if kind == "inaccessible marker" {
					if err := os.Chmod(marker, 0000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(marker, 0700) })
					if _, err := os.Stat(state); !os.IsPermission(err) {
						t.Skip("current user bypasses directory permissions")
					}
				}
			}
			flagProject, flagDB = child, ""
			if path, err := GetDB(); err == nil || path != "" {
				t.Fatalf("unavailable nested state selected or created another database: %q %v", path, err)
			}
			if _, err := os.Lstat(filepath.Join(child, ".slb")); !os.IsNotExist(err) {
				t.Fatalf("failed discovery created child state: %v", err)
			}
			if kind == "missing database" {
				if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Fatalf("failed discovery recreated missing state: %v", err)
				}
			}
		})
	}
}

// TestGetActor_HostnameFallback documents the hostname fallback behavior.
// Note: The localhost fallback (when os.Hostname() returns "") is a rare
// system-level condition that's difficult to trigger in unit tests.
func TestResolveTUIOptionsKeepsSelectedDatabaseAndProject(t *testing.T) {
	origProject, origDB, origConfig := flagProject, flagDB, flagConfig
	origSession, origKey, origTheme := flagTuiSessionID, flagTuiSessionKey, flagTuiTheme
	origRefresh, origMouse := flagTuiRefreshSeconds, flagTuiNoMouse
	t.Cleanup(func() {
		flagProject, flagDB, flagConfig = origProject, origDB, origConfig
		flagTuiSessionID, flagTuiSessionKey, flagTuiTheme = origSession, origKey, origTheme
		flagTuiRefreshSeconds, flagTuiNoMouse = origRefresh, origMouse
	})
	t.Setenv("SLB_PROJECT", "")
	h := testutil.NewHarness(t)
	child := filepath.Join(h.ProjectDir, "src")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(t.TempDir(), "selected store.db")
	testutil.NewTestDBAtPath(t, selected)
	relative, err := filepath.Rel(child, selected)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, override, want string }{
		{"discovered default", "", h.DBPath},
		{"absolute explicit", selected, selected},
		{"relative explicit", relative, selected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(child)
			flagProject, flagDB, flagConfig = "", tc.override, "selected-policy.toml"
			flagTuiSessionID, flagTuiSessionKey, flagTuiTheme = "reviewer-session", "reviewer-key", "frappe"
			flagTuiRefreshSeconds, flagTuiNoMouse = 3, true
			opts, err := resolveTUIOptions()
			if err != nil {
				t.Fatal(err)
			}
			if opts.ProjectPath != h.ProjectDir || opts.DatabasePath != tc.want || !filepath.IsAbs(opts.DatabasePath) {
				t.Fatalf("selected database or project lost: %+v; want database=%q project=%q", opts, tc.want, h.ProjectDir)
			}
			if opts.ConfigPath != filepath.Join(child, "selected-policy.toml") || opts.SessionID != "reviewer-session" ||
				opts.SessionKey != "reviewer-key" || opts.Theme != "frappe" || opts.RefreshInterval != 3 || !opts.DisableMouse {
				t.Fatal("CLI review policy, credentials or display options were not forwarded")
			}
			if cwd, err := os.Getwd(); err != nil || cwd != child {
				t.Fatalf("TUI resolution changed cwd: %q %v", cwd, err)
			}
			if _, err := os.Lstat(filepath.Join(child, ".slb")); !os.IsNotExist(err) {
				t.Fatalf("TUI resolution created descendant state: %v", err)
			}
		})
	}
}

func TestResolveTUIOptionsRefusesUnavailableSelection(t *testing.T) {
	origProject, origDB, origConfig, origRefresh := flagProject, flagDB, flagConfig, flagTuiRefreshSeconds
	t.Cleanup(func() {
		flagProject, flagDB, flagConfig, flagTuiRefreshSeconds = origProject, origDB, origConfig, origRefresh
	})
	t.Setenv("SLB_PROJECT", "")
	for _, kind := range []string{"missing", "directory", "corrupt", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			h := testutil.NewHarness(t)
			selected := filepath.Join(t.TempDir(), "selected.db")
			switch kind {
			case "directory":
				if err := os.Mkdir(selected, 0700); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(selected, []byte("this is not a SQLite database"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				testutil.NewTestDBAtPath(t, selected)
				if err := os.Chmod(selected, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(selected, 0600) })
				if file, err := os.Open(selected); err == nil {
					_ = file.Close()
					t.Skip("current user bypasses file permissions")
				}
			}
			flagProject, flagDB, flagConfig, flagTuiRefreshSeconds = h.ProjectDir, selected, "", 5
			if _, err := resolveTUIOptions(); err == nil || !strings.Contains(err.Error(), selected) {
				t.Fatalf("unavailable explicit selection fell back to valid default state: %v", err)
			}
			if kind == "missing" {
				if _, err := os.Stat(selected); !os.IsNotExist(err) {
					t.Fatalf("TUI initialized missing state: %v", err)
				}
			}
		})
	}
}

func TestGetActor_HostnameFallbackDocumented(t *testing.T) {
	// Save original values
	origActor := flagActor
	origSLBActor := os.Getenv("SLB_ACTOR")
	origAgentName := os.Getenv("AGENT_NAME")
	origUser := os.Getenv("USER")
	defer func() {
		flagActor = origActor
		os.Setenv("SLB_ACTOR", origSLBActor)
		os.Setenv("AGENT_NAME", origAgentName)
		os.Setenv("USER", origUser)
	}()

	// Test that fallback format is always user@host
	flagActor = ""
	os.Setenv("SLB_ACTOR", "")
	os.Setenv("AGENT_NAME", "")
	os.Setenv("USER", "testuser")

	result := GetActor()

	// Should be in format user@hostname
	parts := strings.Split(result, "@")
	if len(parts) != 2 {
		t.Errorf("GetActor() = %q, expected user@hostname format", result)
	}
	if parts[0] != "testuser" {
		t.Errorf("GetActor() user part = %q, expected 'testuser'", parts[0])
	}
	if parts[1] == "" {
		t.Error("GetActor() hostname part should not be empty")
	}

	// Note: Testing the "localhost" fallback (when os.Hostname() returns "")
	// would require mocking os.Hostname, which is not trivial in Go.
	// This is documented as an acceptable coverage gap.
}

// TestGetDB_PrecedenceOrder verifies the precedence order of DB path resolution.
func TestGetDB_PrecedenceOrder(t *testing.T) {
	origDB := flagDB
	origProject := flagProject
	defer func() {
		flagDB = origDB
		flagProject = origProject
	}()

	h := testutil.NewHarness(t)

	// 1. Explicit --db flag has highest precedence
	flagDB = "/explicit/db/path.db"
	flagProject = h.ProjectDir
	result, err := GetDB()
	if err != nil {
		t.Fatal(err)
	}
	if result != "/explicit/db/path.db" {
		t.Errorf("explicit --db flag should take precedence, got %s", result)
	}

	// 2. Project path fallback when no explicit flag
	flagDB = ""
	flagProject = h.ProjectDir
	result, err = GetDB()
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(h.ProjectDir, ".slb", "state.db")
	if result != expected {
		t.Errorf("project path fallback failed: got %s, want %s", result, expected)
	}
}

// TestGetActor_PrecedenceOrder verifies the precedence order of actor resolution.
func TestGetActor_PrecedenceOrder(t *testing.T) {
	origActor := flagActor
	origSLBActor := os.Getenv("SLB_ACTOR")
	origAgentName := os.Getenv("AGENT_NAME")
	defer func() {
		flagActor = origActor
		os.Setenv("SLB_ACTOR", origSLBActor)
		os.Setenv("AGENT_NAME", origAgentName)
	}()

	// 1. Explicit --actor flag has highest precedence
	flagActor = "explicit-actor"
	os.Setenv("SLB_ACTOR", "env-actor")
	os.Setenv("AGENT_NAME", "agent-actor")
	result := GetActor()
	if result != "explicit-actor" {
		t.Errorf("explicit --actor flag should take precedence, got %s", result)
	}

	// 2. SLB_ACTOR env var is second
	flagActor = ""
	result = GetActor()
	if result != "env-actor" {
		t.Errorf("SLB_ACTOR should be second precedence, got %s", result)
	}

	// 3. AGENT_NAME env var is third
	os.Setenv("SLB_ACTOR", "")
	result = GetActor()
	if result != "agent-actor" {
		t.Errorf("AGENT_NAME should be third precedence, got %s", result)
	}

	// 4. Fallback to user@hostname is last
	os.Setenv("AGENT_NAME", "")
	result = GetActor()
	if !strings.Contains(result, "@") {
		t.Errorf("fallback should be user@hostname format, got %s", result)
	}
}

// TestRootDoesNotSilenceErrors is a regression guard for issue #8.
//
// main() exits non-zero WITHOUT printing the returned error. cobra only prints
// a RunE error when neither the command nor any ancestor has SilenceErrors set.
// If the production root command silences errors globally, every failing
// subcommand (run/request/approve, all of which `return fmt.Errorf(...)` for a
// missing --session-id BEFORE reaching their own writeError) exits 1 with no
// output at all — making the tool look completely broken on a fresh install.
//
// Assert directly on the wired-up rootCmd: SilenceErrors must be false so cobra
// surfaces the message, while SilenceUsage stays true so a failure is one line
// rather than a full usage dump.
func TestRootDoesNotSilenceErrors(t *testing.T) {
	if rootCmd.SilenceErrors {
		t.Error("rootCmd.SilenceErrors must be false: main() does not print the error, " +
			"so silencing it here swallows every command failure into a bare exit 1 (issue #8)")
	}
	if !rootCmd.SilenceUsage {
		t.Error("rootCmd.SilenceUsage should stay true so a failure prints one error line, not a usage dump")
	}
}

// TestFailingCommandWritesErrorToStderr is the behavioral regression guard for
// issue #8: a subcommand whose RunE returns a plain error (the missing
// --session-id guard, which returns BEFORE any writeError) must surface a
// non-empty error message rather than failing silently. We run the production
// command tree with its real SilenceErrors setting and capture stderr.
func TestFailingCommandWritesErrorToStderr(t *testing.T) {
	// Build a throwaway command whose RunE mimics the run/request/approve
	// session-id guard: a bare `return fmt.Errorf(...)`. Wiring it under a root
	// configured exactly like the production rootCmd (SilenceUsage true,
	// SilenceErrors = whatever rootCmd uses) proves cobra emits the message.
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  rootCmd.SilenceUsage,
		SilenceErrors: rootCmd.SilenceErrors,
	}
	failing := &cobra.Command{
		Use: "boom",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("--session-id is required")
		},
	}
	root.AddCommand(failing)

	stdout, stderr, err := executeCommand(root, "boom")
	if err == nil {
		t.Fatal("expected the failing command to return an error")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Fatalf("a failing command must write a non-empty error message to stderr (issue #8); "+
			"got empty stderr (stdout=%q)", stdout)
	}
	if !strings.Contains(stderr, "--session-id is required") {
		t.Errorf("stderr should contain the returned error message, got %q", stderr)
	}
	// Exactly one occurrence — guard against accidental double printing if both
	// cobra and a future main()-side printer emit the same error.
	if n := strings.Count(stderr, "--session-id is required"); n != 1 {
		t.Errorf("error message should appear exactly once on stderr, appeared %d times: %q", n, stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	cmd := newTestRootCmd()
	_, _, err := executeCommand(cmd, "nonexistent-command")
	if err == nil {
		t.Error("expected error for unknown command")
	}
}

func TestUnknownFlag(t *testing.T) {
	cmd := newTestRootCmd()
	_, _, err := executeCommand(cmd, "--nonexistent-flag")
	if err == nil {
		t.Error("expected error for unknown flag")
	}
}
