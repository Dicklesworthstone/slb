package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestInitCommandSelectedDirectoryDoesNotDiscoverAncestor(t *testing.T) {
	for _, explicitFlag := range []bool{false, true} {
		name := "environment"
		if explicitFlag {
			name = "flag overrides environment"
		}
		t.Run(name, func(t *testing.T) {
			h := testutil.NewHarness(t)
			selected := filepath.Join(h.ProjectDir, "nested")
			if err := os.Mkdir(selected, 0700); err != nil {
				t.Fatal(err)
			}
			cwd, unselected := t.TempDir(), t.TempDir()
			t.Chdir(cwd)
			oldProject, oldForce, oldOutput, oldJSON := flagProject, flagInitForce, flagOutput, flagJSON
			t.Cleanup(func() { flagProject, flagInitForce, flagOutput, flagJSON = oldProject, oldForce, oldOutput, oldJSON })
			flagProject, flagInitForce, flagOutput, flagJSON = "", false, "json", false
			t.Setenv("SLB_PROJECT", selected)
			if explicitFlag {
				flagProject = selected
				t.Setenv("SLB_PROJECT", unselected)
			}
			if err := runInit(nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(selected, ".slb", "state.db")); err != nil {
				t.Fatalf("selected nested project was not initialized: %v", err)
			}
			for _, dir := range []string{cwd, unselected} {
				if _, err := os.Stat(filepath.Join(dir, ".slb")); !os.IsNotExist(err) {
					t.Fatalf("initialization wrote outside the selected directory: %s, %v", dir, err)
				}
			}
			if current, err := os.Getwd(); err != nil || current != cwd {
				t.Fatalf("environment project selection changed command cwd: %q, %v", current, err)
			}
		})
	}
}

func TestInitCommand_NewProject(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	// Reset flags
	flagInitForce = false
	flagOutput = "text"
	flagJSON = false

	err := runInit(nil, nil)
	if err != nil {
		t.Fatalf("runInit failed: %v", err)
	}

	// Verify directory structure
	dirs := []string{
		".slb",
		".slb/logs",
		".slb/pending",
		".slb/sessions",
		".slb/rollback",
		".slb/processed",
	}
	for _, dir := range dirs {
		path := filepath.Join(tmpDir, dir)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("directory %s not created: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	// Verify database exists
	dbPath := filepath.Join(tmpDir, ".slb", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database not created: %v", err)
	}

	// Verify config exists
	configPath := filepath.Join(tmpDir, ".slb", "config.toml")
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("config not created: %v", err)
	}

	// Verify .gitignore entry
	gitignorePath := filepath.Join(tmpDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); err != nil {
		t.Errorf(".gitignore not created: %v", err)
	} else {
		content, _ := os.ReadFile(gitignorePath)
		if !strings.Contains(string(content), ".slb/") {
			t.Error(".gitignore does not contain .slb/ entry")
		}
	}
}

func TestInitCommand_AlreadyInitialized(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	// Create .slb directory manually
	slbDir := filepath.Join(tmpDir, ".slb")
	if err := os.MkdirAll(slbDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	// Reset flags
	flagInitForce = false

	err := runInit(nil, nil)
	if err == nil {
		t.Fatal("expected error when already initialized")
	}
	if !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInitCommand_ForceReinitialize(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	// Create .slb directory manually
	slbDir := filepath.Join(tmpDir, ".slb")
	if err := os.MkdirAll(slbDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	// Use --force
	flagInitForce = true
	flagOutput = "text"
	flagJSON = false

	err := runInit(nil, nil)
	if err != nil {
		t.Fatalf("runInit with --force failed: %v", err)
	}

	// Verify database exists
	dbPath := filepath.Join(tmpDir, ".slb", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database not created: %v", err)
	}
}

func TestInitCommand_JSONOutput(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	// Reset flags
	flagInitForce = false
	flagOutput = "json"
	flagJSON = true

	err := runInit(nil, nil)
	if err != nil {
		t.Fatalf("runInit with JSON output failed: %v", err)
	}

	// Verify directory was created
	slbDir := filepath.Join(tmpDir, ".slb")
	if _, err := os.Stat(slbDir); err != nil {
		t.Errorf(".slb directory not created: %v", err)
	}
}

func TestWriteDefaultConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	err := writeDefaultConfig(configPath, false)
	if err != nil {
		t.Fatalf("writeDefaultConfig failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// Verify content is valid TOML
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config failed: %v", err)
	}

	// Should have header comment
	if !strings.Contains(string(content), "# SLB Configuration") {
		t.Error("config missing header comment")
	}

	// Should have [general] section
	if !strings.Contains(string(content), "[general]") {
		t.Error("config missing [general] section")
	}
}

func TestWriteDefaultConfig_NoOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Create existing config
	customContent := "# Custom config\n[general]\nmin_approvals = 5\n"
	if err := os.WriteFile(configPath, []byte(customContent), 0644); err != nil {
		t.Fatalf("writing custom config failed: %v", err)
	}

	// Write default config without force
	err := writeDefaultConfig(configPath, false)
	if err != nil {
		t.Fatalf("writeDefaultConfig failed: %v", err)
	}

	// Verify original content preserved
	content, _ := os.ReadFile(configPath)
	if !strings.Contains(string(content), "min_approvals = 5") {
		t.Error("existing config was overwritten")
	}
}

func TestWriteDefaultConfig_ForceOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Create existing config
	customContent := "# Custom config\n[general]\nmin_approvals = 5\n"
	if err := os.WriteFile(configPath, []byte(customContent), 0644); err != nil {
		t.Fatalf("writing custom config failed: %v", err)
	}

	// Write default config with force
	err := writeDefaultConfig(configPath, true)
	if err != nil {
		t.Fatalf("writeDefaultConfig with force failed: %v", err)
	}

	// Verify content was replaced
	content, _ := os.ReadFile(configPath)
	if strings.Contains(string(content), "min_approvals = 5") {
		t.Error("config not overwritten with force")
	}
	if !strings.Contains(string(content), "min_approvals = 2") {
		t.Error("default config not written")
	}
}

func TestAddToGitignore_NewFile(t *testing.T) {
	tmpDir := t.TempDir()
	gitignorePath := filepath.Join(tmpDir, ".gitignore")

	err := addToGitignore(gitignorePath)
	if err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading .gitignore failed: %v", err)
	}

	if !strings.Contains(string(content), ".slb/") {
		t.Error(".gitignore missing .slb/ entry")
	}
}

func TestAddToGitignore_ExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	gitignorePath := filepath.Join(tmpDir, ".gitignore")

	// Create existing .gitignore
	existing := "node_modules/\n*.log\n"
	if err := os.WriteFile(gitignorePath, []byte(existing), 0644); err != nil {
		t.Fatalf("writing .gitignore failed: %v", err)
	}

	err := addToGitignore(gitignorePath)
	if err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading .gitignore failed: %v", err)
	}

	// Should preserve existing content
	if !strings.Contains(string(content), "node_modules/") {
		t.Error("existing .gitignore content was lost")
	}

	// Should add .slb/
	if !strings.Contains(string(content), ".slb/") {
		t.Error(".gitignore missing .slb/ entry")
	}
}

func TestAddToGitignore_AlreadyPresent(t *testing.T) {
	tmpDir := t.TempDir()
	gitignorePath := filepath.Join(tmpDir, ".gitignore")

	// Create .gitignore with .slb/ already present
	existing := "node_modules/\n.slb/\n*.log\n"
	if err := os.WriteFile(gitignorePath, []byte(existing), 0644); err != nil {
		t.Fatalf("writing .gitignore failed: %v", err)
	}

	err := addToGitignore(gitignorePath)
	if err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading .gitignore failed: %v", err)
	}

	// Count occurrences of .slb/
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == ".slb/" || line == ".slb" {
			count++
		}
	}

	if count != 1 {
		t.Errorf("expected 1 .slb/ entry, got %d", count)
	}
}

func TestAddToGitignore_AlreadyPresentSlbWithoutSlash(t *testing.T) {
	tmpDir := t.TempDir()
	gitignorePath := filepath.Join(tmpDir, ".gitignore")

	// Create .gitignore with .slb (without trailing slash) already present
	existing := "node_modules/\n.slb\n*.log\n"
	if err := os.WriteFile(gitignorePath, []byte(existing), 0644); err != nil {
		t.Fatalf("writing .gitignore failed: %v", err)
	}

	err := addToGitignore(gitignorePath)
	if err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading .gitignore failed: %v", err)
	}

	// Count occurrences of .slb or .slb/
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == ".slb/" || line == ".slb" {
			count++
		}
	}

	if count != 1 {
		t.Errorf("expected 1 .slb entry, got %d", count)
	}
}

func TestAddToGitignore_ExistingFileNoNewline(t *testing.T) {
	tmpDir := t.TempDir()
	gitignorePath := filepath.Join(tmpDir, ".gitignore")

	// Create .gitignore without trailing newline
	existing := "node_modules/\n*.log" // No trailing newline
	if err := os.WriteFile(gitignorePath, []byte(existing), 0644); err != nil {
		t.Fatalf("writing .gitignore failed: %v", err)
	}

	err := addToGitignore(gitignorePath)
	if err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading .gitignore failed: %v", err)
	}

	// Should have added newline before .slb/
	if !strings.Contains(string(content), "*.log\n") {
		t.Error("expected newline to be added before .slb/ entry")
	}
	if !strings.Contains(string(content), ".slb/") {
		t.Error(".gitignore missing .slb/ entry")
	}
}

func TestInitCommand_YAMLOutput(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	// Reset flags
	flagInitForce = false
	flagOutput = "yaml"
	flagJSON = false

	err := runInit(nil, nil)
	if err != nil {
		t.Fatalf("runInit with YAML output failed: %v", err)
	}

	// Verify directory was created
	slbDir := filepath.Join(tmpDir, ".slb")
	if _, err := os.Stat(slbDir); err != nil {
		t.Errorf(".slb directory not created: %v", err)
	}
}

func TestInitCommand_UnsupportedOutputFormat(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	// Reset flags with unsupported format
	flagInitForce = false
	flagOutput = "unsupported_format"
	flagJSON = false

	err := runInit(nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported output format")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestAddToGitignore_EmptyExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	gitignorePath := filepath.Join(tmpDir, ".gitignore")

	// Create empty .gitignore file
	if err := os.WriteFile(gitignorePath, []byte(""), 0644); err != nil {
		t.Fatalf("writing empty .gitignore failed: %v", err)
	}

	err := addToGitignore(gitignorePath)
	if err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading .gitignore failed: %v", err)
	}

	if !strings.Contains(string(content), ".slb/") {
		t.Error(".gitignore missing .slb/ entry")
	}
}
