package db

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func patternProposal(t *testing.T, database *DB, tier, pattern, changeType string) *PatternChange {
	t.Helper()
	proposal := &PatternChange{Tier: tier, Pattern: pattern, ChangeType: changeType, Reason: "human-reviewed test policy"}
	if err := database.CreatePatternChange(proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}

func activePatternCount(t *testing.T, database *DB, tier, pattern string) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM custom_patterns WHERE tier = ? AND pattern = ?`, tier, pattern).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func insertActivePattern(t *testing.T, database *DB, tier, pattern string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO custom_patterns (tier, pattern, description, source, created_at) VALUES (?, ?, ?, ?, ?)`,
		tier, pattern, "existing rule", "human", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
}

func TestPatternResolutionPublishesApprovedRules(t *testing.T) {
	for _, changeType := range []string{PatternChangeTypeAdd, PatternChangeTypeSuggest} {
		t.Run(changeType, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()
			p := patternProposal(t, database, "SAFE", `^reviewed-command$`, changeType)
			if activePatternCount(t, database, "safe", p.Pattern) != 0 {
				t.Fatal("proposal was active before review")
			}
			if err := database.ApprovePatternChange(p.ID); err != nil {
				t.Fatal(err)
			}
			if activePatternCount(t, database, "safe", p.Pattern) != 1 {
				t.Fatal("approval did not persist the canonical rule")
			}
			var source, description string
			if err := database.QueryRow(`SELECT source, description FROM custom_patterns WHERE tier = ? AND pattern = ?`, "safe", p.Pattern).
				Scan(&source, &description); err != nil {
				t.Fatal(err)
			}
			if source != "human" || description != p.Reason {
				t.Fatalf("review provenance lost: %q %q", source, description)
			}
			got, err := database.GetPatternChange(p.ID)
			if err != nil || got.Status != PatternChangeStatusApproved {
				t.Fatalf("decision not persisted: %+v %v", got, err)
			}
			if err := database.ApprovePatternChange(p.ID); !errors.Is(err, ErrPatternChangeDecided) {
				t.Fatalf("approval replay accepted: %v", err)
			}
			if err := database.RejectPatternChange(p.ID); !errors.Is(err, ErrPatternChangeDecided) {
				t.Fatalf("terminal decision rewritten: %v", err)
			}
		})
	}
}

func TestPatternResolutionRemovalIsExactAndDurable(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	pattern := `^remove-reviewed-rule$`
	insertActivePattern(t, database, "dangerous", pattern)
	insertActivePattern(t, database, "critical", pattern)
	insertActivePattern(t, database, "dangerous", `^keep-reviewed-rule$`)
	p := patternProposal(t, database, "dangerous", pattern, PatternChangeTypeRemove)
	if err := database.ApprovePatternChange(p.ID); err != nil {
		t.Fatal(err)
	}
	if activePatternCount(t, database, "dangerous", pattern) != 0 ||
		activePatternCount(t, database, "critical", pattern) != 1 ||
		activePatternCount(t, database, "dangerous", `^keep-reviewed-rule$`) != 1 {
		t.Fatal("removal affected another tier or pattern")
	}
	// A replay must not remove a newly installed rule with the same identity.
	insertActivePattern(t, database, "dangerous", pattern)
	if err := database.ApprovePatternChange(p.ID); !errors.Is(err, ErrPatternChangeDecided) {
		t.Fatalf("removal replay: %v", err)
	}
	if activePatternCount(t, database, "dangerous", pattern) != 1 {
		t.Fatal("replayed approval removed a replacement rule")
	}
}

func TestPatternResolutionFailuresLeaveProposalPending(t *testing.T) {
	for _, tc := range []struct{ name, tier, pattern, changeType string }{
		{"unknown-tier", "typo", `.*`, PatternChangeTypeAdd},
		{"invalid-regex", "safe", `[`, PatternChangeTypeSuggest},
		{"unknown-change", "dangerous", `^x$`, "unknown"},
		{"missing-tier", "", `^x$`, PatternChangeTypeRemove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()
			p := patternProposal(t, database, tc.tier, tc.pattern, tc.changeType)
			if err := database.ApprovePatternChange(p.ID); err == nil {
				t.Fatal("invalid policy effect was approved")
			}
			got, err := database.GetPatternChange(p.ID)
			if err != nil || got.Status != PatternChangeStatusPending {
				t.Fatalf("failed application committed its decision: %+v %v", got, err)
			}
			if err := database.RejectPatternChange(p.ID); err != nil {
				t.Fatalf("malformed proposal cannot be rejected: %v", err)
			}
		})
	}
}

func TestPatternResolutionRollsBackStorageFailures(t *testing.T) {
	for _, op := range []string{"INSERT", "DELETE"} {
		t.Run(op, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()
			changeType := PatternChangeTypeAdd
			pattern := `^failed-write$`
			if op == "DELETE" {
				changeType = PatternChangeTypeRemove
				insertActivePattern(t, database, "safe", pattern)
			}
			p := patternProposal(t, database, "safe", pattern, changeType)
			if _, err := database.Exec(`CREATE TRIGGER fail_policy_write BEFORE ` + op + ` ON custom_patterns BEGIN SELECT RAISE(ABORT, 'injected storage failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := activePatternCount(t, database, "safe", pattern)
			if err := database.ApprovePatternChange(p.ID); err == nil {
				t.Fatal("injected storage failure was swallowed")
			}
			got, err := database.GetPatternChange(p.ID)
			if err != nil || got.Status != PatternChangeStatusPending {
				t.Fatalf("decision survived failed policy write: %+v %v", got, err)
			}
			if activePatternCount(t, database, "safe", pattern) != before {
				t.Fatal("failed decision partially changed active policy")
			}
		})
	}
}

func TestPatternResolutionConcurrentReviewers(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	other, err := Open(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	p := patternProposal(t, database, "dangerous", `^concurrent-reviewed-rule$`, PatternChangeTypeAdd)
	start := make(chan struct{})
	results := make(chan error, 2)
	var reviewers sync.WaitGroup
	for _, connection := range []*DB{database, other} {
		reviewers.Add(1)
		go func(d *DB) {
			defer reviewers.Done()
			<-start
			results <- d.ApprovePatternChange(p.ID)
		}(connection)
	}
	close(start)
	reviewers.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrPatternChangeDecided) {
			conflicts++
		} else {
			t.Fatalf("unexpected review error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 || activePatternCount(t, database, "dangerous", p.Pattern) != 1 {
		t.Fatalf("decision was not single-winner: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestPatternResolutionCanRemoveMalformedCustomRule(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	insertActivePattern(t, database, "typo", `[`) // Corrupt policy that fails closed.
	p := patternProposal(t, database, "typo", `[`, PatternChangeTypeRemove)
	if err := database.ApprovePatternChange(p.ID); err != nil {
		t.Fatal(err)
	}
	if activePatternCount(t, database, "typo", `[`) != 0 {
		t.Fatal("malformed rule could not be removed for recovery")
	}
}

func TestPatternResolutionRejectsAndHandlesMissingIDs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	p := patternProposal(t, database, "safe", `.*`, PatternChangeTypeAdd)
	if err := database.RejectPatternChange(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ApprovePatternChange(p.ID); !errors.Is(err, ErrPatternChangeDecided) {
		t.Fatalf("rejected rule was approved: %v", err)
	}
	if activePatternCount(t, database, "safe", p.Pattern) != 0 {
		t.Fatal("rejected proposal became active")
	}
	if err := database.ApprovePatternChange(-1); !errors.Is(err, ErrPatternChangeNotFound) {
		t.Fatalf("missing proposal error: %v", err)
	}
}

func TestPatternResolutionRemovalOfAbsentCustomRuleIsIdempotent(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	p := patternProposal(t, database, "dangerous", `^already-absent$`, PatternChangeTypeRemove)
	if err := database.ApprovePatternChange(p.ID); err != nil {
		t.Fatal(err)
	}
	if activePatternCount(t, database, "dangerous", p.Pattern) != 0 {
		t.Fatal("removal unexpectedly introduced an active rule")
	}
}
