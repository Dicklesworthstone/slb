package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func TestPolicyReloadReplacesProjectAndRemovedRules(t *testing.T) {
	a := testutil.NewHarness(t)
	b := testutil.NewHarness(t)
	previousDB := flagDB
	t.Cleanup(func() {
		flagDB = previousDB
		core.GetDefaultEngine().LoadDefaultPatterns()
	})
	pattern := `^policy-snapshot-project-command$`
	if _, err := a.DB.InsertCustomPattern("safe", pattern, "only project A", "human"); err != nil {
		t.Fatal(err)
	}
	flagDB = a.DBPath
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatal(err)
	}
	if !core.Classify("policy-snapshot-project-command", "").IsSafe {
		t.Fatal("project A allow rule was not loaded")
	}
	flagDB = b.DBPath
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatal(err)
	}
	if core.Classify("policy-snapshot-project-command", "").IsSafe {
		t.Fatal("project A allow rule leaked into project B")
	}
	flagDB = a.DBPath
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(`DELETE FROM custom_patterns WHERE pattern = ?`, pattern); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatal(err)
	}
	if core.Classify("policy-snapshot-project-command", "").IsSafe {
		t.Fatal("removed allow rule survived reload")
	}
}

func TestPolicyLoadRejectsInvalidRows(t *testing.T) {
	for _, row := range []struct{ tier, pattern string }{
		{"typo", `.*`}, {"dangerous", `[`}, {"safe", `[`},
	} {
		t.Run(row.tier+row.pattern, func(t *testing.T) {
			h := testutil.NewHarness(t)
			previousDB := flagDB
			flagDB = h.DBPath
			t.Cleanup(func() { flagDB = previousDB })
			if _, err := h.DB.InsertCustomPattern(row.tier, row.pattern, "invalid", "human"); err != nil {
				t.Fatal(err)
			}
			before := core.GetDefaultEngine().ComputeHash()
			if _, err := loadCustomPatternsIntoDefaultEngine(); err == nil {
				t.Fatal("invalid persisted policy silently accepted")
			}
			if core.GetDefaultEngine().ComputeHash() != before {
				t.Fatal("failed load changed active policy")
			}
		})
	}
}

func TestPolicyConsumersFailClosedOnCorruptDatabase(t *testing.T) {
	previousDB := flagDB
	previousDir := flagHookOutputDir
	previousFile := flagPatternOutputFile
	t.Cleanup(func() {
		flagDB, flagHookOutputDir, flagPatternOutputFile = previousDB, previousDir, previousFile
	})
	flagDB = filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(flagDB, []byte("not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	flagHookOutputDir = t.TempDir()
	flagPatternOutputFile = filepath.Join(t.TempDir(), "patterns.json")
	script := filepath.Join(flagHookOutputDir, "slb_guard.py")
	for _, path := range []string{script, flagPatternOutputFile} {
		if err := os.WriteFile(path, []byte("existing protection"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	consumers := []struct {
		name string
		run  func(*cobra.Command, []string) error
	}{
		{"check", patternsTestCmd.RunE}, {"list", patternsListCmd.RunE},
		{"export", patternsExportCmd.RunE}, {"version", patternsVersionCmd.RunE},
		{"hook-test", runHookTest}, {"hook-generate", runHookGenerate},
	}
	for _, consumer := range consumers {
		t.Run(consumer.name, func(t *testing.T) {
			if err := consumer.run(&cobra.Command{}, []string{"git stash"}); err == nil {
				t.Fatal("consumer continued despite unreadable policy")
			}
		})
	}
	for _, path := range []string{script, flagPatternOutputFile} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "existing protection" {
			t.Fatalf("existing protection overwritten: %s (%v)", path, err)
		}
	}
}

func TestPolicyLoadDoesNotCreateExplicitMissingDatabase(t *testing.T) {
	previousDB := flagDB
	t.Cleanup(func() { flagDB = previousDB })
	flagDB = filepath.Join(t.TempDir(), "missing.db")
	if _, err := loadCustomPatternsIntoDefaultEngine(); err == nil {
		t.Fatal("explicit missing database was accepted")
	}
	if _, err := os.Stat(flagDB); !os.IsNotExist(err) {
		t.Fatalf("policy check created database: %v", err)
	}
}

func TestPatternFailedPersistenceDoesNotPublishAllowRule(t *testing.T) {
	previousDB, previousTier := flagDB, flagPatternTier
	t.Cleanup(func() { flagDB, flagPatternTier = previousDB, previousTier })
	flagDB = t.TempDir() // A directory cannot serve as a database.
	flagPatternTier = "safe"
	before := core.GetDefaultEngine().ComputeHash()
	err := patternsAddCmd.RunE(&cobra.Command{}, []string{`^unpersisted-allow-rule$`})
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("expected persistence error, got %v", err)
	}
	if core.GetDefaultEngine().ComputeHash() != before {
		t.Fatal("failed persistence exposed an uncommitted allow rule")
	}
}
