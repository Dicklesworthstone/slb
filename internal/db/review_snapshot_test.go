package db

import (
	"context"
	"errors"
	"testing"
)

func TestReviewSnapshotConsistentWithConcurrentDecision(t *testing.T) {
	database, author := admissionFixture(t)
	reviewer := &Session{AgentName: "snapshot-reviewer", Model: "review-model", ProjectPath: author.ProjectPath}
	if err := database.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	target := admissionRequest(author)
	if _, err := database.AdmitRequest(context.Background(), target, RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := database.ApplyReview(&Review{RequestID: target.ID, ReviewerSessionID: reviewer.ID, Decision: DecisionApprove}, reviewer.SessionKey, ReviewPolicy{})
		done <- err
	}()
	for i := 0; i < 40; i++ {
		snapshot, err := database.ReadRequestReviewSnapshot(context.Background(), author.ProjectPath, target.ID, reviewer.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch snapshot.Request.Status {
		case StatusPending:
			if len(snapshot.Reviews) != 0 || snapshot.Request.ApprovalExpiresAt != nil {
				t.Fatal("pending request mixed with post-commit reviews")
			}
		case StatusApproved:
			if len(snapshot.Reviews) != 1 || snapshot.Request.ApprovalExpiresAt == nil {
				t.Fatal("approved request mixed with pre-commit reviews")
			}
		default:
			t.Fatalf("unexpected status: %s", snapshot.Request.Status)
		}
		if snapshot.Session == nil || snapshot.Session.ID != reviewer.ID || snapshot.Request.Command.Hash != target.Command.Hash {
			t.Fatal("snapshot lost identity")
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReviewSnapshotScopeAndReadFailures(t *testing.T) {
	database, author := admissionFixture(t)
	target := admissionRequest(author)
	if _, err := database.AdmitRequest(context.Background(), target, RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ project, id string }{{"", target.ID}, {author.ProjectPath, ""}, {"/other", target.ID}, {author.ProjectPath, "missing"}} {
		if s, err := database.ReadRequestReviewSnapshot(context.Background(), tc.project, tc.id, ""); err == nil || s != nil {
			t.Fatalf("invalid scoped snapshot: %+v %v", s, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, err := database.ReadRequestReviewSnapshot(ctx, author.ProjectPath, target.ID, ""); !errors.Is(err, context.Canceled) || s != nil {
		t.Fatalf("cancelled read returned data: %+v %v", s, err)
	}
	if err := database.EndSession(author.ID); err != nil {
		t.Fatal(err)
	}
	s, err := database.ReadRequestReviewSnapshot(context.Background(), author.ProjectPath, target.ID, author.ID)
	if err != nil || s.Session == nil || s.Session.IsActive() {
		t.Fatalf("ended reviewer looked active: %+v %v", s, err)
	}
	s, err = database.ReadRequestReviewSnapshot(context.Background(), author.ProjectPath, target.ID, "missing")
	if err != nil || s.Session != nil {
		t.Fatalf("missing optional reviewer hid history: %+v %v", s, err)
	}
	// Corrupt session timestamps must not be hidden by a partially filled view.
	if _, err := database.Exec(`UPDATE sessions SET started_at = ? WHERE id = ?`, "not-a-time", author.ID); err != nil {
		t.Fatal(err)
	}
	if s, err := database.ReadRequestReviewSnapshot(context.Background(), author.ProjectPath, target.ID, author.ID); err == nil || s != nil {
		t.Fatalf("corrupt snapshot returned partial data: %+v %v", s, err)
	}
}
