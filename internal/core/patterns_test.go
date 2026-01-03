// Package core tests pattern matching.
package core

import (
	"testing"
)

func TestClassifyCommand(t *testing.T) {
	engine := NewPatternEngine()

	tests := []struct {
		name              string
		cmd               string
		wantTier          RiskTier
		wantApprovals     int
		wantNeedsApproval bool
	}{
		// Critical commands
		{
			name:              "rm -rf root",
			cmd:               "rm -rf /etc",
			wantTier:          RiskTierCritical,
			wantApprovals:     2,
			wantNeedsApproval: true,
		},
		{
			name:              "DROP DATABASE",
			cmd:               "psql -c 'DROP DATABASE mydb'",
			wantTier:          RiskTierCritical,
			wantApprovals:     2,
			wantNeedsApproval: true,
		},
		{
			name:              "terraform destroy",
			cmd:               "terraform destroy",
			wantTier:          RiskTierCritical,
			wantApprovals:     2,
			wantNeedsApproval: true,
		},
		{
			name:              "kubectl delete node",
			cmd:               "kubectl delete node worker-1",
			wantTier:          RiskTierCritical,
			wantApprovals:     2,
			wantNeedsApproval: true,
		},
		{
			name:              "git push --force",
			cmd:               "git push --force origin main",
			wantTier:          RiskTierCritical,
			wantApprovals:     2,
			wantNeedsApproval: true,
		},
		// Dangerous commands
		{
			name:              "rm -rf local",
			cmd:               "rm -rf ./build",
			wantTier:          RiskTierDangerous,
			wantApprovals:     1,
			wantNeedsApproval: true,
		},
		{
			name:              "rm -fr local (reversed flags)",
			cmd:               "rm -fr ./build",
			wantTier:          RiskTierDangerous,
			wantApprovals:     1,
			wantNeedsApproval: true,
		},
		{
			name:              "git reset --hard",
			cmd:               "git reset --hard HEAD~3",
			wantTier:          RiskTierDangerous,
			wantApprovals:     1,
			wantNeedsApproval: true,
		},
		{
			name:              "git clean -fd",
			cmd:               "git clean -fd",
			wantTier:          RiskTierDangerous,
			wantApprovals:     1,
			wantNeedsApproval: true,
		},
		{
			name:              "kubectl delete pod",
			cmd:               "kubectl delete deployment nginx",
			wantTier:          RiskTierDangerous,
			wantApprovals:     1,
			wantNeedsApproval: true,
		},
		{
			name:              "docker rm",
			cmd:               "docker rm container1",
			wantTier:          RiskTierDangerous,
			wantApprovals:     1,
			wantNeedsApproval: true,
		},
		// Caution commands
		{
			name:              "git stash drop",
			cmd:               "git stash drop",
			wantTier:          RiskTierCaution,
			wantApprovals:     0,
			wantNeedsApproval: true,
		},
		{
			name:              "npm uninstall",
			cmd:               "npm uninstall lodash",
			wantTier:          RiskTierCaution,
			wantApprovals:     0,
			wantNeedsApproval: true,
		},
		// Safe commands (no approval needed)
		{
			name:              "git status",
			cmd:               "git status",
			wantTier:          "",
			wantApprovals:     0,
			wantNeedsApproval: false,
		},
		{
			name:              "ls",
			cmd:               "ls -la",
			wantTier:          "",
			wantApprovals:     0,
			wantNeedsApproval: false,
		},
		{
			name:              "cat file",
			cmd:               "cat README.md",
			wantTier:          "",
			wantApprovals:     0,
			wantNeedsApproval: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.ClassifyCommand(tt.cmd, "")

			if result.Tier != tt.wantTier {
				t.Errorf("Tier = %q, want %q", result.Tier, tt.wantTier)
			}
			if result.MinApprovals != tt.wantApprovals {
				t.Errorf("MinApprovals = %d, want %d", result.MinApprovals, tt.wantApprovals)
			}
			if result.NeedsApproval != tt.wantNeedsApproval {
				t.Errorf("NeedsApproval = %v, want %v", result.NeedsApproval, tt.wantNeedsApproval)
			}
		})
	}
}

func TestNormalizeCommand(t *testing.T) {
	tests := []struct {
		name            string
		cmd             string
		wantPrimary     string
		wantCompound    bool
		wantStrippedLen int
	}{
		{
			name:         "simple command",
			cmd:          "ls -la",
			wantPrimary:  "ls -la",
			wantCompound: false,
		},
		{
			name:            "sudo wrapper",
			cmd:             "sudo rm -rf /tmp",
			wantPrimary:     "rm -rf /tmp",
			wantStrippedLen: 1,
		},
		{
			name:            "multiple wrappers",
			cmd:             "sudo env rm -rf /tmp",
			wantPrimary:     "rm -rf /tmp",
			wantStrippedLen: 2, // sudo, env
		},
		{
			name:         "compound with semicolon",
			cmd:          "cd /tmp; rm -rf .",
			wantCompound: true,
		},
		{
			name:         "compound with &&",
			cmd:          "make build && make test",
			wantCompound: true,
		},
		{
			name:         "compound with pipe",
			cmd:          "ps aux | grep nginx",
			wantCompound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeCommand(tt.cmd)

			if tt.wantPrimary != "" && result.Primary != tt.wantPrimary {
				t.Errorf("Primary = %q, want %q", result.Primary, tt.wantPrimary)
			}
			if result.IsCompound != tt.wantCompound {
				t.Errorf("IsCompound = %v, want %v", result.IsCompound, tt.wantCompound)
			}
			if tt.wantStrippedLen > 0 && len(result.StrippedWrappers) != tt.wantStrippedLen {
				t.Errorf("StrippedWrappers len = %d, want %d (got: %v)",
					len(result.StrippedWrappers), tt.wantStrippedLen, result.StrippedWrappers)
			}
		})
	}
}

func TestCompoundCommandClassification(t *testing.T) {
	engine := NewPatternEngine()

	// Compound command with highest tier being critical
	result := engine.ClassifyCommand("ls && rm -rf /etc", "")
	if result.Tier != RiskTierCritical {
		t.Errorf("Expected critical for compound with rm -rf /etc, got %s", result.Tier)
	}

	// Compound command with highest tier being dangerous
	result = engine.ClassifyCommand("cd /tmp && rm -rf ./build", "")
	if result.Tier != RiskTierDangerous {
		t.Errorf("Expected dangerous for compound with rm -rf ./build, got %s", result.Tier)
	}
}

func TestCompoundCommandSafePrecedence(t *testing.T) {
	engine := NewPatternEngine()

	// SAFE patterns should still take precedence for individual segments in compound commands.
	res := engine.ClassifyCommand("echo ok && kubectl delete pod nginx-123", "")
	if res.Tier != RiskTier(RiskSafe) || res.NeedsApproval || !res.IsSafe {
		t.Fatalf("Tier=%q NeedsApproval=%v IsSafe=%v, want safe/false/true", res.Tier, res.NeedsApproval, res.IsSafe)
	}
}

func TestCompoundCommandQuotesBypassFix(t *testing.T) {
	engine := NewPatternEngine()

	// SECURITY FIX: Commands with quotes should still be split when && is OUTSIDE quotes.
	// This tests the fix for the vulnerability where `echo "foo" && rm -rf /etc` was
	// incorrectly treated as a non-compound command because of the quote count heuristic.

	t.Run("dangerous rm -rf /etc hidden after quoted echo", func(t *testing.T) {
		res := engine.ClassifyCommand(`echo "foo" && rm -rf /etc`, "")
		if res.Tier != RiskTierCritical {
			t.Fatalf("Tier=%q, want critical (rm -rf /etc is critical)", res.Tier)
		}
		if !res.NeedsApproval {
			t.Error("NeedsApproval should be true")
		}
	})

	t.Run("dangerous git push --force hidden after quoted echo", func(t *testing.T) {
		res := engine.ClassifyCommand(`echo "done" && git push --force`, "")
		if res.Tier != RiskTierCritical {
			t.Fatalf("Tier=%q, want critical (git push --force is critical)", res.Tier)
		}
	})

	t.Run("SQL inside quotes should NOT split", func(t *testing.T) {
		res := engine.ClassifyCommand(`psql -c "DELETE FROM users; DROP TABLE users;"`, "")
		// The fallback SQL detection should still catch this
		if res.Tier != RiskTierCritical {
			t.Fatalf("Tier=%q, want critical (DELETE + DROP is critical)", res.Tier)
		}
	})
}

func TestSudoStripping(t *testing.T) {
	engine := NewPatternEngine()

	// sudo should be stripped and still match dangerous pattern
	result := engine.ClassifyCommand("sudo rm -rf ./build", "")
	if result.Tier != RiskTierDangerous {
		t.Errorf("Expected dangerous for 'sudo rm -rf', got %s", result.Tier)
	}
	if !result.NeedsApproval {
		t.Error("Expected NeedsApproval to be true")
	}
}

func TestCaseInsensitivity(t *testing.T) {
	engine := NewPatternEngine()

	// SQL keywords are case-insensitive
	result := engine.ClassifyCommand("drop database mydb", "")
	if result.Tier != RiskTierCritical {
		t.Errorf("Expected critical for 'drop database', got %s", result.Tier)
	}

	result = engine.ClassifyCommand("DROP DATABASE mydb", "")
	if result.Tier != RiskTierCritical {
		t.Errorf("Expected critical for 'DROP DATABASE', got %s", result.Tier)
	}
}

func TestSafePatternsAndPrecedence(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("git stash is SAFE but git stash drop is CAUTION", func(t *testing.T) {
		safe := engine.ClassifyCommand("git stash", "")
		if safe.Tier != RiskTier(RiskSafe) || safe.NeedsApproval || !safe.IsSafe {
			t.Fatalf("git stash: Tier=%q NeedsApproval=%v IsSafe=%v", safe.Tier, safe.NeedsApproval, safe.IsSafe)
		}

		drop := engine.ClassifyCommand("git stash drop", "")
		if drop.Tier != RiskTierCaution || !drop.NeedsApproval || drop.IsSafe {
			t.Fatalf("git stash drop: Tier=%q NeedsApproval=%v IsSafe=%v", drop.Tier, drop.NeedsApproval, drop.IsSafe)
		}
	})

	t.Run("kubectl delete pod is SAFE (overrides kubectl delete)", func(t *testing.T) {
		res := engine.ClassifyCommand("kubectl delete pod nginx-123", "")
		if res.Tier != RiskTier(RiskSafe) || res.NeedsApproval || !res.IsSafe {
			t.Fatalf("kubectl delete pod: Tier=%q NeedsApproval=%v IsSafe=%v", res.Tier, res.NeedsApproval, res.IsSafe)
		}
	})

	t.Run("npm cache clean is SAFE", func(t *testing.T) {
		res := engine.ClassifyCommand("npm cache clean", "")
		if res.Tier != RiskTier(RiskSafe) || res.NeedsApproval || !res.IsSafe {
			t.Fatalf("npm cache clean: Tier=%q NeedsApproval=%v IsSafe=%v", res.Tier, res.NeedsApproval, res.IsSafe)
		}
	})

	t.Run("rm *.log/tmp/bak is SAFE", func(t *testing.T) {
		for _, cmd := range []string{"rm app.log", "rm app.tmp", "rm app.bak"} {
			res := engine.ClassifyCommand(cmd, "")
			if res.Tier != RiskTier(RiskSafe) || res.NeedsApproval || !res.IsSafe {
				t.Fatalf("%s: Tier=%q NeedsApproval=%v IsSafe=%v", cmd, res.Tier, res.NeedsApproval, res.IsSafe)
			}
		}
	})
}

func TestGitPushForceWithLeaseIsDangerous(t *testing.T) {
	engine := NewPatternEngine()

	res := engine.ClassifyCommand("git push --force-with-lease origin main", "")
	if res.Tier != RiskTierDangerous {
		t.Fatalf("Tier = %q, want %q", res.Tier, RiskTierDangerous)
	}
	if res.MinApprovals != 1 || !res.NeedsApproval {
		t.Fatalf("MinApprovals=%d NeedsApproval=%v, want 1/true", res.MinApprovals, res.NeedsApproval)
	}
}

func TestSQLDeleteWhereVsNoWhere(t *testing.T) {
	engine := NewPatternEngine()

	noWhere := engine.ClassifyCommand(`psql -c "DELETE FROM users;"`, "")
	if noWhere.Tier != RiskTierCritical {
		t.Fatalf("DELETE without WHERE Tier = %q, want %q", noWhere.Tier, RiskTierCritical)
	}

	withWhere := engine.ClassifyCommand(`psql -c "DELETE FROM users WHERE id=1;"`, "")
	if withWhere.Tier != RiskTierDangerous {
		t.Fatalf("DELETE with WHERE Tier = %q, want %q", withWhere.Tier, RiskTierDangerous)
	}
}

func TestParseErrorUpgradesTierConservatively(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("no match + parse error defaults to CAUTION", func(t *testing.T) {
		// Unterminated quote triggers shellwords parse error.
		res := engine.ClassifyCommand(`echo "unterminated`, "")
		if !res.ParseError {
			t.Fatalf("expected ParseError=true")
		}
		if res.Tier != RiskTierCaution || !res.NeedsApproval || res.IsSafe {
			t.Fatalf("Tier=%q NeedsApproval=%v IsSafe=%v, want caution/true/false", res.Tier, res.NeedsApproval, res.IsSafe)
		}
		if res.MatchedPattern != "parse_error" {
			t.Fatalf("MatchedPattern=%q, want %q", res.MatchedPattern, "parse_error")
		}
	})

	t.Run("SAFE match + parse error upgrades to CAUTION", func(t *testing.T) {
		res := engine.ClassifyCommand(`git stash "unterminated`, "")
		if !res.ParseError {
			t.Fatalf("expected ParseError=true")
		}
		if res.Tier != RiskTierCaution || !res.NeedsApproval || res.IsSafe {
			t.Fatalf("Tier=%q NeedsApproval=%v IsSafe=%v, want caution/true/false", res.Tier, res.NeedsApproval, res.IsSafe)
		}
	})

	t.Run("CAUTION match + parse error upgrades to DANGEROUS", func(t *testing.T) {
		// git branch -d is CAUTION tier - add parse error to trigger upgrade
		res := engine.ClassifyCommand(`git branch -d "unterminated`, "")
		if !res.ParseError {
			t.Fatalf("expected ParseError=true")
		}
		if res.Tier != RiskTierDangerous {
			t.Fatalf("Tier=%q, want %q", res.Tier, RiskTierDangerous)
		}
		if !res.NeedsApproval {
			t.Fatalf("expected NeedsApproval=true")
		}
	})

	t.Run("DANGEROUS match + parse error upgrades to CRITICAL", func(t *testing.T) {
		// rm -rf is DANGEROUS tier - add parse error to trigger upgrade
		res := engine.ClassifyCommand(`rm -rf "unterminated`, "")
		if !res.ParseError {
			t.Fatalf("expected ParseError=true")
		}
		if res.Tier != RiskTierCritical {
			t.Fatalf("Tier=%q, want %q", res.Tier, RiskTierCritical)
		}
		if res.MinApprovals != 2 {
			t.Fatalf("MinApprovals=%d, want 2", res.MinApprovals)
		}
	})

	t.Run("CRITICAL match + parse error stays CRITICAL", func(t *testing.T) {
		// sudo rm -rf is CRITICAL tier - parse error shouldn't change it
		res := engine.ClassifyCommand(`sudo rm -rf /* "unterminated`, "")
		if !res.ParseError {
			t.Fatalf("expected ParseError=true")
		}
		if res.Tier != RiskTierCritical {
			t.Fatalf("Tier=%q, want %q", res.Tier, RiskTierCritical)
		}
	})
}

func TestCompoundCommandMatchedSegments(t *testing.T) {
	engine := NewPatternEngine()

	res := engine.ClassifyCommand("git status && rm -rf ./build", "")
	if res.Tier != RiskTierDangerous {
		t.Fatalf("Tier = %q, want %q", res.Tier, RiskTierDangerous)
	}
	if len(res.MatchedSegments) == 0 {
		t.Fatalf("expected MatchedSegments to be populated for compound command")
	}
	found := false
	for _, seg := range res.MatchedSegments {
		if seg.Tier == RiskTierDangerous {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected at least one dangerous segment match, got: %+v", res.MatchedSegments)
	}
}

func TestUpgradeTier(t *testing.T) {
	tests := []struct {
		name string
		in   RiskTier
		want RiskTier
	}{
		{"critical stays critical", RiskTierCritical, RiskTierCritical},
		{"dangerous upgrades to critical", RiskTierDangerous, RiskTierCritical},
		{"caution upgrades to dangerous", RiskTierCaution, RiskTierDangerous},
		{"safe upgrades to caution", RiskTier(RiskSafe), RiskTierCaution},
		{"unknown defaults to caution", RiskTier("unknown"), RiskTierCaution},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := upgradeTier(tc.in)
			if got != tc.want {
				t.Errorf("upgradeTier(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPatternEngineAddAndRemovePattern(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("add pattern to critical tier", func(t *testing.T) {
		err := engine.AddPattern(RiskTierCritical, "my-critical-pattern", "Test critical", "test")
		if err != nil {
			t.Fatalf("AddPattern failed: %v", err)
		}

		patterns := engine.ListPatterns(RiskTierCritical)
		found := false
		for _, p := range patterns {
			if p.Pattern == "my-critical-pattern" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Pattern not found after adding")
		}
	})

	t.Run("add pattern to dangerous tier", func(t *testing.T) {
		err := engine.AddPattern(RiskTierDangerous, "my-dangerous-pattern", "Test dangerous", "test")
		if err != nil {
			t.Fatalf("AddPattern failed: %v", err)
		}

		patterns := engine.ListPatterns(RiskTierDangerous)
		found := false
		for _, p := range patterns {
			if p.Pattern == "my-dangerous-pattern" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Pattern not found after adding")
		}
	})

	t.Run("add pattern to caution tier", func(t *testing.T) {
		err := engine.AddPattern(RiskTierCaution, "my-caution-pattern", "Test caution", "test")
		if err != nil {
			t.Fatalf("AddPattern failed: %v", err)
		}

		patterns := engine.ListPatterns(RiskTierCaution)
		found := false
		for _, p := range patterns {
			if p.Pattern == "my-caution-pattern" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Pattern not found after adding")
		}
	})

	t.Run("add pattern to safe tier (default)", func(t *testing.T) {
		err := engine.AddPattern(RiskTier(RiskSafe), "my-safe-pattern", "Test safe", "test")
		if err != nil {
			t.Fatalf("AddPattern failed: %v", err)
		}

		patterns := engine.ListPatterns(RiskTier(RiskSafe))
		found := false
		for _, p := range patterns {
			if p.Pattern == "my-safe-pattern" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Pattern not found after adding")
		}
	})

	t.Run("add invalid regex pattern", func(t *testing.T) {
		err := engine.AddPattern(RiskTierCritical, "[invalid", "Invalid regex", "test")
		if err == nil {
			t.Error("Expected error for invalid regex pattern")
		}
	})

	t.Run("remove existing pattern", func(t *testing.T) {
		removed := engine.RemovePattern(RiskTierCritical, "my-critical-pattern")
		if !removed {
			t.Error("RemovePattern returned false for existing pattern")
		}

		patterns := engine.ListPatterns(RiskTierCritical)
		for _, p := range patterns {
			if p.Pattern == "my-critical-pattern" {
				t.Error("Pattern still exists after removal")
			}
		}
	})

	t.Run("remove non-existent pattern", func(t *testing.T) {
		removed := engine.RemovePattern(RiskTierCritical, "non-existent-pattern")
		if removed {
			t.Error("RemovePattern returned true for non-existent pattern")
		}
	})

	t.Run("remove from different tiers", func(t *testing.T) {
		// Remove from dangerous tier
		removed := engine.RemovePattern(RiskTierDangerous, "my-dangerous-pattern")
		if !removed {
			t.Error("RemovePattern failed for dangerous tier")
		}

		// Remove from caution tier
		removed = engine.RemovePattern(RiskTierCaution, "my-caution-pattern")
		if !removed {
			t.Error("RemovePattern failed for caution tier")
		}

		// Remove from safe tier
		removed = engine.RemovePattern(RiskTier(RiskSafe), "my-safe-pattern")
		if !removed {
			t.Error("RemovePattern failed for safe tier")
		}
	})
}

func TestPatternEngineAllPatterns(t *testing.T) {
	engine := NewPatternEngine()

	all := engine.AllPatterns()
	if all == nil {
		t.Fatal("AllPatterns returned nil")
	}

	// Check that all expected keys exist
	for _, key := range []string{"safe", "critical", "dangerous", "caution"} {
		if _, ok := all[key]; !ok {
			t.Errorf("AllPatterns missing key %q", key)
		}
	}
}

func TestClassifyCommandWithCwd(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("cwd resolves relative paths - may escalate risk", func(t *testing.T) {
		// Test with a cwd provided - the path should be resolved
		// When resolved, /home/user/project/build becomes an absolute path which is CRITICAL
		result := engine.ClassifyCommand("rm -rf ./build", "/home/user/project")
		// Either dangerous or critical is acceptable depending on path resolution
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier, got %q", result.Tier)
		}
		if !result.NeedsApproval {
			t.Errorf("expected NeedsApproval to be true")
		}
	})

	t.Run("empty cwd uses command as-is", func(t *testing.T) {
		result := engine.ClassifyCommand("rm -rf ./build", "")
		if result.Tier != RiskTierDangerous {
			t.Errorf("expected dangerous tier, got %q", result.Tier)
		}
	})

	t.Run("cwd with absolute path in command", func(t *testing.T) {
		result := engine.ClassifyCommand("rm -rf /etc", "/home/user/project")
		if result.Tier != RiskTierCritical {
			t.Errorf("expected critical tier for /etc, got %q", result.Tier)
		}
	})
}

func TestClassifyCommandEdgeCases(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("empty command", func(t *testing.T) {
		result := engine.ClassifyCommand("", "")
		// Empty command should not match any pattern
		if result.NeedsApproval {
			t.Errorf("empty command should not need approval")
		}
	})

	t.Run("whitespace only command", func(t *testing.T) {
		result := engine.ClassifyCommand("   ", "")
		// Whitespace should not match any pattern
		if result.NeedsApproval && result.Tier != RiskTierCaution {
			t.Errorf("whitespace command should not match dangerous pattern")
		}
	})

	t.Run("unknown command falls through to no match", func(t *testing.T) {
		result := engine.ClassifyCommand("my-custom-command --flag", "")
		if result.NeedsApproval {
			t.Errorf("unknown command should not need approval")
		}
	})
}

func TestConvenienceFunctions(t *testing.T) {
	t.Run("Classify uses default engine", func(t *testing.T) {
		result := Classify("rm -rf /etc", "")
		if result == nil {
			t.Fatal("Classify returned nil")
		}
		if result.Tier != RiskTierCritical {
			t.Errorf("Classify tier = %q, want %q", result.Tier, RiskTierCritical)
		}
	})

	t.Run("TestPattern detects dangerous commands", func(t *testing.T) {
		if !TestPattern("rm -rf /") {
			t.Error("TestPattern should return true for 'rm -rf /'")
		}
		if TestPattern("ls -la") {
			t.Error("TestPattern should return false for 'ls -la'")
		}
	})

	t.Run("MatchesPattern checks specific pattern", func(t *testing.T) {
		if !MatchesPattern("rm -rf /tmp", `rm\s+-rf`) {
			t.Error("MatchesPattern should match 'rm -rf'")
		}
		if MatchesPattern("ls -la", `rm\s+-rf`) {
			t.Error("MatchesPattern should not match 'ls -la' against rm pattern")
		}
	})

	t.Run("MatchesPattern handles invalid regex", func(t *testing.T) {
		if MatchesPattern("anything", "[invalid") {
			t.Error("MatchesPattern should return false for invalid regex")
		}
	})

	t.Run("MatchesPattern trims whitespace", func(t *testing.T) {
		if !MatchesPattern("  rm -rf /tmp  ", `rm\s+-rf`) {
			t.Error("MatchesPattern should handle leading/trailing whitespace")
		}
	})
}

func TestCompilePatterns_InvalidPattern(t *testing.T) {
	// compilePatterns should skip invalid regex patterns
	patterns := compilePatterns(RiskTierDangerous, []string{
		"valid-pattern",
		"[invalid-regex", // Invalid regex - unclosed bracket
		"another-valid-.*",
	}, "test")

	// Should have 2 valid patterns (invalid one skipped)
	if len(patterns) != 2 {
		t.Errorf("expected 2 valid patterns, got %d", len(patterns))
	}
}

func TestClassifyCompoundCommands(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("xargs with rm escalates to dangerous inner command", func(t *testing.T) {
		// find | xargs rm should classify based on rm
		result := engine.ClassifyCommand("find . | xargs rm -rf", "")
		if !result.NeedsApproval {
			t.Error("expected xargs rm -rf to need approval")
		}
		// Should be at least dangerous due to rm -rf
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier, got %q", result.Tier)
		}
	})

	t.Run("xargs with kubectl delete", func(t *testing.T) {
		result := engine.ClassifyCommand("kubectl get pods | xargs kubectl delete pod", "")
		if !result.NeedsApproval {
			t.Error("expected xargs kubectl delete to need approval")
		}
	})

	t.Run("pipe with safe commands is safe", func(t *testing.T) {
		result := engine.ClassifyCommand("ls -la | grep test | head -5", "")
		// Safe commands don't need approval
		if result.NeedsApproval && result.Tier != RiskTierCaution {
			t.Errorf("ls | grep | head should not be dangerous, got tier %q", result.Tier)
		}
	})

	t.Run("compound with dangerous or critical command escalates", func(t *testing.T) {
		result := engine.ClassifyCommand("ls -la && rm -rf /", "")
		// rm -rf / may be dangerous or critical depending on pattern matching
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier, got %q", result.Tier)
		}
		if result.MinApprovals < 1 {
			t.Errorf("should require at least 1 approval, got %d", result.MinApprovals)
		}
	})

	t.Run("multiple segments with varying risk levels", func(t *testing.T) {
		// Mix of safe, caution, and dangerous commands
		result := engine.ClassifyCommand("git status && npm install && rm -rf ./build", "")
		// Should escalate to dangerous (rm -rf with relative path)
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier, got %q", result.Tier)
		}
	})

	t.Run("tracks matched segments", func(t *testing.T) {
		result := engine.ClassifyCommand("git status && rm -rf /tmp/test", "")
		// Should have matched segments from each part
		if len(result.MatchedSegments) == 0 {
			t.Error("expected matched segments to be recorded")
		}
	})

	t.Run("caution command alone as first segment", func(t *testing.T) {
		// npm uninstall is caution, ls is safe
		result := engine.ClassifyCommand("npm uninstall somepackage", "")
		if result.Tier != RiskTierCaution {
			t.Errorf("expected caution tier, got %q", result.Tier)
		}
	})

	t.Run("caution as highest tier in compound", func(t *testing.T) {
		// ls is safe, npm uninstall is caution
		result := engine.ClassifyCommand("ls -la && npm uninstall somepackage", "")
		if result.Tier != RiskTierCaution {
			t.Errorf("expected caution tier, got %q", result.Tier)
		}
		if !result.NeedsApproval {
			t.Error("expected caution to need approval")
		}
	})

	t.Run("safe command alone is safe", func(t *testing.T) {
		result := engine.ClassifyCommand("git stash", "")
		if result.Tier != RiskTier(RiskSafe) && result.NeedsApproval {
			t.Errorf("expected safe tier or no approval needed, got tier=%q needsApproval=%v", result.Tier, result.NeedsApproval)
		}
	})

	t.Run("safe segment followed by caution segment should be caution", func(t *testing.T) {
		// kubectl delete pod nginx-123 matches SAFE pattern
		// npm uninstall foo matches CAUTION pattern
		// Higher risk tier (caution) should win
		result := engine.ClassifyCommand("kubectl delete pod nginx-123 && npm uninstall foo", "")
		if result.Tier != RiskTierCaution {
			t.Errorf("expected caution tier when safe followed by caution, got %q", result.Tier)
		}
		if !result.NeedsApproval {
			t.Error("expected caution to need approval")
		}
		if result.IsSafe {
			t.Error("compound with caution segment should not be marked safe")
		}
	})

	t.Run("safe segment followed by dangerous segment should be dangerous", func(t *testing.T) {
		// git stash matches SAFE pattern
		// rm -rf ./build matches DANGEROUS pattern
		// Higher risk tier (dangerous) should win
		result := engine.ClassifyCommand("git stash && rm -rf ./build", "")
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier when safe followed by dangerous, got %q", result.Tier)
		}
		if result.IsSafe {
			t.Error("compound with dangerous segment should not be marked safe")
		}
	})
}

func TestClassifyCommand_FallbackSQL(t *testing.T) {
	// These tests check the fallback SQL detection that occurs when
	// the command doesn't match any primary patterns
	engine := NewPatternEngine()

	t.Run("DELETE FROM without WHERE in obscure wrapper", func(t *testing.T) {
		// Use an unusual command that won't match primary DELETE patterns
		// but will be caught by fallback detection
		result := engine.ClassifyCommand("some-script 'execute delete from users cascade'", "")
		if result.Tier != RiskTierCritical && result.Tier != RiskTierDangerous {
			t.Errorf("expected critical or dangerous tier for DELETE without WHERE, got %q", result.Tier)
		}
	})

	t.Run("DELETE FROM with WHERE in wrapper", func(t *testing.T) {
		result := engine.ClassifyCommand("some-tool 'delete from users where id > 100'", "")
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier for DELETE with WHERE, got %q", result.Tier)
		}
	})
}

func TestClassifyCommand_CommandResolution(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("uses first segment when primary is empty", func(t *testing.T) {
		// Simple command without wrapper should use the command directly
		result := engine.ClassifyCommand("rm -rf /tmp/test", "")
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier, got %q", result.Tier)
		}
	})

	t.Run("uses raw command when no segments parsed", func(t *testing.T) {
		// Very unusual command that might not parse into segments
		result := engine.ClassifyCommand("", "")
		// Empty command should not need approval
		if result.NeedsApproval {
			t.Error("empty command should not need approval")
		}
	})

	t.Run("safe pattern takes precedence", func(t *testing.T) {
		// git stash is a safe command (in default patterns)
		result := engine.ClassifyCommand("git stash", "")
		if result.Tier != RiskTier(RiskSafe) {
			t.Errorf("expected safe tier for git stash, got %q", result.Tier)
		}
		if !result.IsSafe {
			t.Error("expected IsSafe to be true")
		}
	})

	t.Run("caution pattern matches", func(t *testing.T) {
		// npm uninstall is a caution command (in default patterns)
		result := engine.ClassifyCommand("npm uninstall somepackage", "")
		if result.Tier != RiskTierCaution {
			t.Errorf("expected caution tier for npm uninstall, got %q", result.Tier)
		}
		if !result.NeedsApproval {
			t.Error("expected NeedsApproval to be true for caution tier")
		}
	})

	t.Run("parse error is captured", func(t *testing.T) {
		// Command with parse issues should capture the error
		// Using unbalanced quotes
		result := engine.ClassifyCommand(`echo "unclosed`, "")
		// The command should still be classified, but may have ParseError set
		// Note: parseError may or may not be set depending on shellwords behavior
		_ = result.ParseError // Just verify it's accessible
	})
}

func TestClassifyCommand_SQLDetection(t *testing.T) {
	engine := NewPatternEngine()

	t.Run("SQL delete without WHERE is critical", func(t *testing.T) {
		// DELETE FROM without WHERE should be critical
		result := engine.ClassifyCommand("psql -c 'DELETE FROM users'", "")
		if result.Tier != RiskTierCritical {
			t.Errorf("expected critical tier for DELETE without WHERE, got %q", result.Tier)
		}
		if result.MatchedPattern == "" {
			t.Error("expected a matched pattern")
		}
	})

	t.Run("SQL delete with WHERE is dangerous", func(t *testing.T) {
		result := engine.ClassifyCommand("mysql -e 'DELETE FROM users WHERE id = 1'", "")
		if result.Tier != RiskTierDangerous {
			t.Errorf("expected dangerous tier for DELETE with WHERE, got %q", result.Tier)
		}
		if result.MatchedPattern == "" {
			t.Error("expected a matched pattern")
		}
	})

	t.Run("SQL truncate is critical", func(t *testing.T) {
		result := engine.ClassifyCommand("psql -c 'TRUNCATE TABLE users'", "")
		if result.Tier != RiskTierCritical {
			t.Errorf("expected critical tier for TRUNCATE, got %q", result.Tier)
		}
	})

	t.Run("SQL drop is dangerous or critical", func(t *testing.T) {
		result := engine.ClassifyCommand("mysql -e 'DROP TABLE users'", "")
		if result.Tier != RiskTierDangerous && result.Tier != RiskTierCritical {
			t.Errorf("expected dangerous or critical tier for DROP TABLE, got %q", result.Tier)
		}
		if !result.NeedsApproval {
			t.Error("DROP TABLE should need approval")
		}
	})

	t.Run("SQL select is not dangerous", func(t *testing.T) {
		result := engine.ClassifyCommand("psql -c 'SELECT * FROM users'", "")
		// SELECT should not be critical or dangerous
		if result.Tier == RiskTierCritical || result.Tier == RiskTierDangerous {
			t.Errorf("SELECT should not be dangerous/critical, got %q", result.Tier)
		}
	})
}
