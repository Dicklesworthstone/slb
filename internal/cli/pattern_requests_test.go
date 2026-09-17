package cli

import (
	"encoding/json"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestSafePatternAddRequiresDurableHumanReview(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()
	t.Cleanup(func() { resetPatternsFlags(); core.GetDefaultEngine().LoadDefaultPatterns() })
	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "add", `^reviewed-allow-command$`, "-T", "safe", "-r", "allow after review", "-j")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Status    string `json:"status"`
		RequestID int64  `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "pending" || response.RequestID <= 0 {
		t.Fatalf("allow rule was not queued: %s", stdout)
	}
	proposal, err := h.DB.GetPatternChange(response.RequestID)
	if err != nil || proposal.Status != db.PatternChangeStatusPending || proposal.Tier != "safe" {
		t.Fatalf("proposal not durable: %+v %v", proposal, err)
	}
	if count, err := h.DB.CountCustomPatterns(); err != nil || count != 0 {
		t.Fatalf("unreviewed allow rule was activated: count=%d err=%v", count, err)
	}
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatal(err)
	}
	if core.Classify("reviewed-allow-command", "").IsSafe {
		t.Fatal("unreviewed allow rule bypasses classification")
	}
	if err := h.DB.ApprovePatternChange(proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatal(err)
	}
	if !core.Classify("reviewed-allow-command", "").IsSafe {
		t.Fatal("human-approved rule was not activated on reload")
	}
}

func TestSuggestedPatternAndRemovalPersistUntilReview(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()
	t.Cleanup(func() { resetPatternsFlags(); core.GetDefaultEngine().LoadDefaultPatterns() })
	cmd := newTestPatternsCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd, "patterns", "suggest", `^governed-command$`, "-T", "critical", "-j"); err != nil {
		t.Fatal(err)
	}
	pending, err := h.DB.ListPendingPatternChanges()
	if err != nil || len(pending) != 1 || pending[0].ChangeType != db.PatternChangeTypeSuggest {
		t.Fatalf("suggestion not persisted: %+v %v", pending, err)
	}
	if err := h.DB.ApprovePatternChange(pending[0].ID); err != nil {
		t.Fatal(err)
	}
	resetPatternsFlags()
	cmd = newTestPatternsCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd, "patterns", "request-removal", `^governed-command$`, "-r", "retired command", "-j"); err != nil {
		t.Fatal(err)
	}
	pending, err = h.DB.ListPendingPatternChanges()
	if err != nil || len(pending) != 1 || pending[0].Tier != "critical" || pending[0].ChangeType != db.PatternChangeTypeRemove {
		t.Fatalf("removal not bound to persisted tier: %+v %v", pending, err)
	}
	if count, _ := h.DB.CountCustomPatterns(); count != 1 {
		t.Fatal("unapproved removal changed active policy")
	}
	if err := h.DB.ApprovePatternChange(pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if count, _ := h.DB.CountCustomPatterns(); count != 0 {
		t.Fatal("approved removal was not persisted")
	}
}

func TestPatternSuggestionsValidateBeforePersistence(t *testing.T) {
	for _, args := range [][]string{
		{"patterns", "suggest", `[`, "-T", "safe", "-j"},
		{"patterns", "suggest", `.*`, "-T", "typo", "-j"},
	} {
		h := testutil.NewHarness(t)
		resetPatternsFlags()
		cmd := newTestPatternsCmd(h.DBPath)
		if _, err := executeCommandCapture(t, cmd, args...); err == nil {
			t.Fatalf("invalid proposal accepted: %v", args)
		}
		if count, err := h.DB.CountPendingPatternChanges(); err != nil || count != 0 {
			t.Fatalf("invalid proposal was persisted: count=%d err=%v", count, err)
		}
	}
	t.Cleanup(resetPatternsFlags)
}
