package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
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
		got := GetDB()
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

		got := GetDB()
		expected := filepath.Join(h.ProjectDir, ".slb", "state.db")
		if got != expected {
			t.Errorf("GetDB() = %v, want %v", got, expected)
		}
	})
}

func TestGetActor(t *testing.T) {
	// Save original values
	origActor := flagActor
	origSLBActor := os.Getenv("SLB_ACTOR")
	origAgentName := os.Getenv("AGENT_NAME")
	defer func() {
		flagActor = origActor
		os.Setenv("SLB_ACTOR", origSLBActor)
		os.Setenv("AGENT_NAME", origAgentName)
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
