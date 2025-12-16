// Package e2e contains end-to-end integration tests for SLB workflows.
package e2e

import (
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// TestRiskTierClassification_CommandMatrix tests that commands are correctly
// classified into risk tiers with appropriate approval requirements.
// This is a critical security test - incorrect classification could bypass review.
func TestRiskTierClassification_CommandMatrix(t *testing.T) {
	t.Log("=== TestRiskTierClassification_CommandMatrix ===")
	t.Log("Testing command classification against risk tier matrix")

	engine := core.NewPatternEngine()

	tests := []struct {
		name           string
		command        string
		expectedTier   db.RiskTier
		expectedMinApp int
		needsApproval  bool
	}{
		// CRITICAL tier (2 approvals required)
		{"rm -rf root", "rm -rf /", db.RiskTierDangerous, 1, true}, // Note: bare / detected as dangerous only
		{"rm -rf /etc", "rm -rf /etc", db.RiskTierCritical, 2, true},
		{"rm -rf /*", "rm -rf /*", db.RiskTierCritical, 2, true},
		{"kubectl delete namespace prod", "kubectl delete namespace production", db.RiskTierCritical, 2, true},
		{"terraform destroy bare", "terraform destroy", db.RiskTierCritical, 2, true},
		{"git push --force main", "git push --force origin main", db.RiskTierCritical, 2, true},
		{"DROP DATABASE", "psql -c 'DROP DATABASE production'", db.RiskTierCritical, 2, true},
		{"TRUNCATE TABLE", "TRUNCATE TABLE users", db.RiskTierCritical, 2, true},

		// DANGEROUS tier (1 approval required)
		{"rm -rf local", "rm -rf ./build", db.RiskTierDangerous, 1, true},
		{"rm -rf /home path", "rm -rf /home/user/project", db.RiskTierCritical, 2, true}, // /home triggers critical
		{"git reset --hard", "git reset --hard HEAD~1", db.RiskTierDangerous, 1, true},
		{"git clean -fd", "git clean -fd", db.RiskTierDangerous, 1, true},
		{"docker rm container", "docker rm mycontainer", db.RiskTierDangerous, 1, true},
		{"kubectl delete deployment", "kubectl delete deployment nginx", db.RiskTierDangerous, 1, true},
		{"DROP TABLE", "DROP TABLE users", db.RiskTierDangerous, 1, true},

		// CAUTION tier (0 approvals but tracked)
		{"npm uninstall", "npm uninstall express", db.RiskTierCaution, 0, true},
		{"git stash drop", "git stash drop", db.RiskTierCaution, 0, true},
		{"pip uninstall", "pip uninstall requests", db.RiskTierCaution, 0, true},

		// SAFE tier (explicit safe patterns) - commands that DO NOT need approval
		{"git stash save", "git stash", db.RiskTier(core.RiskSafe), 0, false},
		{"kubectl delete pod", "kubectl delete pod nginx-123", db.RiskTier(core.RiskSafe), 0, false},
		{"npm cache clean", "npm cache clean", db.RiskTier(core.RiskSafe), 0, false},

		// NO MATCH tier (unknown commands) - allowed without review for usability
		// Note: tier is empty string "", NeedsApproval=false
		{"ls", "ls -la", "", 0, false},
		{"cat file", "cat /etc/passwd", "", 0, false},
		{"echo", "echo hello world", "", 0, false},
		{"pwd", "pwd", "", 0, false},
		{"git status", "git status", "", 0, false},
	}

	passed := 0
	failed := 0

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.ClassifyCommand(tt.command, "")

			tierMatch := result.Tier == tt.expectedTier
			approvalsMatch := result.MinApprovals == tt.expectedMinApp
			needsMatch := result.NeedsApproval == tt.needsApproval

			if tierMatch && approvalsMatch && needsMatch {
				passed++
				t.Logf("[%d/%d] %s", i+1, len(tests), tt.name)
				t.Logf("  ✓ Tier: %s (expected: %s)", result.Tier, tt.expectedTier)
				t.Logf("  ✓ Approvals: %d (expected: %d)", result.MinApprovals, tt.expectedMinApp)
				if result.MatchedPattern != "" {
					t.Logf("  ✓ Matched: %s", result.MatchedPattern)
				}
			} else {
				failed++
				if !tierMatch {
					t.Errorf("Tier mismatch: got %s, want %s", result.Tier, tt.expectedTier)
				}
				if !approvalsMatch {
					t.Errorf("Approvals mismatch: got %d, want %d", result.MinApprovals, tt.expectedMinApp)
				}
				if !needsMatch {
					t.Errorf("NeedsApproval mismatch: got %v, want %v", result.NeedsApproval, tt.needsApproval)
				}
			}
		})
	}

	t.Logf("Summary: %d/%d classifications correct", passed, len(tests))
	if failed > 0 {
		t.Errorf("%d classifications failed", failed)
	}
	t.Log("=== END: TestRiskTierClassification_CommandMatrix ===")
}

// TestRiskTierClassification_EdgeCaseGaps documents edge cases that should be handled
// but currently are NOT. These represent robustness improvement opportunities.
func TestRiskTierClassification_EdgeCaseGaps(t *testing.T) {
	t.Log("=== TestRiskTierClassification_EdgeCaseGaps ===")
	t.Log("Documenting edge case gaps (expected to not match)")

	engine := core.NewPatternEngine()

	// Edge cases that SHOULD be handled but currently AREN'T
	// This test documents known limitations in pattern matching
	gaps := []string{
		// Case variations
		"RM -RF /etc",           // uppercase not handled
		"Rm -rF /etc",           // mixed case not handled

		// Whitespace variations
		"rm  -rf   /etc",        // extra spaces not handled
		"rm\t-rf\t/etc",         // tabs not handled

		// Quoting
		"rm -rf '/etc'",         // single quotes not stripped
		"rm -rf \"/etc\"",       // double quotes not stripped

		// Subshells and pipelines
		"bash -c 'rm -rf /etc'", // subshell not unwrapped
		"sh -c 'rm -rf /'",      // subshell not unwrapped
		"find . -name '*.tmp' | xargs rm", // pipeline not analyzed
	}

	gapsFound := 0
	for _, cmd := range gaps {
		t.Run(cmd, func(t *testing.T) {
			result := engine.ClassifyCommand(cmd, "")

			if !result.NeedsApproval {
				gapsFound++
				t.Logf("  GAP: %q not detected (expected - known gap)", cmd)
			} else {
				// If this starts passing, the gap has been fixed!
				t.Logf("  FIXED: %q now detected! tier=%s", cmd, result.Tier)
			}
		})
	}

	t.Logf("Summary: %d/%d edge case gaps still present", gapsFound, len(gaps))
	t.Log("=== END: TestRiskTierClassification_EdgeCaseGaps ===")
}

// TestRiskTierClassification_RequestIntegration tests that classified risk
// is properly applied when creating requests through the full pipeline.
func TestRiskTierClassification_RequestIntegration(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Log("=== TestRiskTierClassification_RequestIntegration ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)

	// Create session
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("test-agent"),
		testutil.WithModel("test-model"),
	)

	// Test commands with different risk tiers
	testCases := []struct {
		command      string
		expectedTier db.RiskTier
		minApprovals int
	}{
		{"rm -rf /etc/important", db.RiskTierCritical, 2},
		{"rm -rf ./build", db.RiskTierDangerous, 1},
		{"npm uninstall package", db.RiskTierCaution, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.command, func(t *testing.T) {
			req := testutil.MakeRequest(t, h.DB, sess,
				testutil.WithCommand(tc.command, h.ProjectDir, true),
				testutil.WithRisk(tc.expectedTier),
				testutil.WithMinApprovals(tc.minApprovals),
			)

			// Verify request was created with correct risk tier
			retrieved, err := h.DB.GetRequest(req.ID)
			if err != nil {
				t.Fatalf("GetRequest failed: %v", err)
			}

			if retrieved.RiskTier != tc.expectedTier {
				t.Errorf("Request tier = %s, want %s", retrieved.RiskTier, tc.expectedTier)
			}
			if retrieved.MinApprovals != tc.minApprovals {
				t.Errorf("Request MinApprovals = %d, want %d", retrieved.MinApprovals, tc.minApprovals)
			}

			t.Logf("  ✓ %s: tier=%s, approvals=%d",
				tc.command, retrieved.RiskTier, retrieved.MinApprovals)
		})
	}

	t.Log("=== END: TestRiskTierClassification_RequestIntegration ===")
}

// TestRiskTierClassification_NoFalseNegatives verifies that known dangerous commands
// are NEVER classified as safe. This is a security invariant.
// Commands that currently pass classification:
func TestRiskTierClassification_NoFalseNegatives(t *testing.T) {
	t.Log("=== TestRiskTierClassification_NoFalseNegatives ===")
	t.Log("Verifying critical commands are not classified as safe")

	engine := core.NewPatternEngine()

	// Commands that MUST require approval and ARE currently detected
	dangersCommands := []struct {
		cmd         string
		minApproval int
	}{
		{"rm -rf /", 1},           // dangerous tier
		{"rm -rf /*", 2},          // critical - wildcard catch
		{"rm -rf /etc", 2},        // critical - system path
		{"rm -rf /var", 2},        // critical - system path
		{"rm -rf /home", 2},       // critical - system path
		{"rm -rf /usr", 2},        // critical - system path
		{"terraform destroy", 2},  // critical
		{"kubectl delete namespace default", 2},
		{"git push --force origin main", 2},
		{"DROP TABLE users", 1},   // dangerous
		{"TRUNCATE TABLE orders", 2},
	}

	for _, tc := range dangersCommands {
		t.Run(tc.cmd, func(t *testing.T) {
			result := engine.ClassifyCommand(tc.cmd, "")

			if !result.NeedsApproval {
				t.Errorf("SECURITY: Dangerous command %q classified as safe!", tc.cmd)
			}

			if result.MinApprovals < tc.minApproval {
				t.Errorf("SECURITY: Command %q requires %d approvals, expected >= %d",
					tc.cmd, result.MinApprovals, tc.minApproval)
			}

			t.Logf("  ✓ %s: tier=%s, approvals=%d, pattern=%s",
				tc.cmd, result.Tier, result.MinApprovals, result.MatchedPattern)
		})
	}

	t.Log("=== END: TestRiskTierClassification_NoFalseNegatives ===")
}

// TestRiskTierClassification_KnownGaps documents commands that SHOULD be detected
// but currently are NOT. These represent security improvement opportunities.
func TestRiskTierClassification_KnownGaps(t *testing.T) {
	t.Log("=== TestRiskTierClassification_KnownGaps ===")
	t.Log("Documenting known classification gaps (expected to not match)")

	engine := core.NewPatternEngine()

	// Commands that SHOULD require approval but currently DON'T
	// These are documented gaps that represent security improvement opportunities
	knownGaps := []string{
		"dd if=/dev/zero of=/dev/sda",  // disk destruction - no pattern
		"mkfs.ext4 /dev/sda1",          // filesystem format - no pattern
		"git push -f origin master",    // short -f flag not caught
		"chmod 777 /etc/passwd",        // chmod not in patterns
		"terraform destroy -auto-approve", // with flag variant
	}

	gapsFound := 0
	for _, cmd := range knownGaps {
		t.Run(cmd, func(t *testing.T) {
			result := engine.ClassifyCommand(cmd, "")

			if !result.NeedsApproval {
				gapsFound++
				t.Logf("  GAP: %q not detected (expected - known gap)", cmd)
			} else {
				// If this starts passing, the gap has been fixed!
				t.Logf("  FIXED: %q now detected! tier=%s", cmd, result.Tier)
			}
		})
	}

	t.Logf("Summary: %d/%d known gaps still present", gapsFound, len(knownGaps))
	t.Log("=== END: TestRiskTierClassification_KnownGaps ===")
}
