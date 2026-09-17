package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func executorApprovalFixture(t *testing.T) (*db.DB, *db.Request, []*db.Session) {
	t.Helper()
	database, request, sessions := atomicReviewFixture(t, 2)
	service := NewReviewService(database, DefaultReviewConfig())
	for _, session := range sessions[1:3] {
		if _, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: session.ID,
			SessionKey: session.SessionKey, Decision: db.DecisionApprove}); err != nil {
			t.Fatal(err)
		}
	}
	request, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	return database, request, sessions
}

func TestExecutorRejectsInvalidProofBeforePreflight(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		want        error
	}{
		{"missing reviews", `DELETE FROM reviews`, db.ErrInvalidApprovalProof},
		{"forged review", `UPDATE reviews SET signature = 'forged'`, db.ErrInvalidApprovalProof},
		{"missing deadline", `UPDATE requests SET approval_expires_at = NULL`, db.ErrInvalidApprovalProof},
		{"ended executor", `UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z' WHERE id = 'atomic-review-0'`, ErrSessionInactive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request, sessions := executorApprovalFixture(t)
			if _, err := database.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, nil)
			if tc.want == db.ErrInvalidApprovalProof {
				if allowed, reason := executor.CanExecute(request.ID); allowed || reason == "" {
					t.Fatalf("advisory/hook check advertised unusable approval: %v %q", allowed, reason)
				}
			}
			logDir := filepath.Join(request.ProjectPath, "must-not-create-preflight")
			result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
				RequestID: request.ID, SessionID: sessions[0].ID, LogDir: logDir,
				CaptureRollback: true, SuppressOutput: true,
			})
			if result != nil || !errors.Is(err, tc.want) {
				t.Fatalf("invalid approval reached preflight/execution: %+v %v", result, err)
			}
			if _, err := os.Stat(logDir); !os.IsNotExist(err) {
				t.Fatalf("preflight touched files before proof validation: %v", err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != db.StatusApproved || stored.Execution != nil {
				t.Fatalf("failed preflight consumed approval: %+v", stored)
			}
		})
	}
}

func TestExecutorProofCheckDoesNotConsumeApproval(t *testing.T) {
	database, request, _ := executorApprovalFixture(t)
	executor := NewExecutor(database, nil)
	for i := 0; i < 2; i++ {
		if allowed, reason := executor.CanExecute(request.ID); !allowed {
			t.Fatalf("valid approval rejected: %q", reason)
		}
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != db.StatusApproved || stored.Execution != nil {
		t.Fatal("advisory check consumed approval")
	}
}

func TestExecutorProofCheckRejectsReplacedSnapshot(t *testing.T) {
	database, request, _ := executorApprovalFixture(t)
	if _, err := database.Exec(`UPDATE requests SET project_path = ? WHERE id = ?`, filepath.Join(request.ProjectPath, "other"), request.ID); err != nil {
		t.Fatal(err)
	}
	if err := NewExecutor(database, nil).verifyApprovalSnapshot(request); !errors.Is(err, db.ErrInvalidTransition) {
		t.Fatalf("preflight substituted a changed request snapshot: %v", err)
	}
}
