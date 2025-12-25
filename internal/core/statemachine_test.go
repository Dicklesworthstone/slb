package core

import (
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from db.RequestStatus
		to   db.RequestStatus
		want bool
	}{
		{"new->pending", "", db.StatusPending, true},
		{"new->approved (invalid)", "", db.StatusApproved, false},

		{"pending->approved", db.StatusPending, db.StatusApproved, true},
		{"pending->rejected", db.StatusPending, db.StatusRejected, true},
		{"pending->cancelled", db.StatusPending, db.StatusCancelled, true},
		{"pending->timeout", db.StatusPending, db.StatusTimeout, true},
		{"pending->executing (invalid)", db.StatusPending, db.StatusExecuting, false},

		{"timeout->escalated", db.StatusTimeout, db.StatusEscalated, true},
		{"timeout->approved (invalid)", db.StatusTimeout, db.StatusApproved, false},

		{"escalated->approved", db.StatusEscalated, db.StatusApproved, true},
		{"escalated->rejected", db.StatusEscalated, db.StatusRejected, true},
		{"escalated->executing (invalid)", db.StatusEscalated, db.StatusExecuting, false},

		{"approved->executing", db.StatusApproved, db.StatusExecuting, true},
		{"approved->cancelled", db.StatusApproved, db.StatusCancelled, true},
		{"approved->executed (invalid)", db.StatusApproved, db.StatusExecuted, false},

		{"executing->executed", db.StatusExecuting, db.StatusExecuted, true},
		{"executing->execution_failed", db.StatusExecuting, db.StatusExecutionFailed, true},
		{"executing->timed_out", db.StatusExecuting, db.StatusTimedOut, true},

		{"executed->pending (invalid)", db.StatusExecuted, db.StatusPending, false},
		{"rejected->approved (invalid)", db.StatusRejected, db.StatusApproved, false},
		{"cancelled->approved (invalid)", db.StatusCancelled, db.StatusApproved, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestTransitionSetsResolvedAtForTerminalStates(t *testing.T) {
	t.Run("sets created_at for new->pending when missing", func(t *testing.T) {
		req := &db.Request{Status: ""}
		if err := Transition(req, db.StatusPending); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		if req.Status != db.StatusPending {
			t.Fatalf("Status = %q, want %q", req.Status, db.StatusPending)
		}
		if req.CreatedAt.IsZero() {
			t.Fatalf("CreatedAt is zero, want non-zero after creation transition")
		}
	})

	t.Run("sets resolved_at for rejected", func(t *testing.T) {
		req := &db.Request{Status: db.StatusPending}
		if err := Transition(req, db.StatusRejected); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		if req.Status != db.StatusRejected {
			t.Fatalf("Status = %q, want %q", req.Status, db.StatusRejected)
		}
		if req.ResolvedAt == nil {
			t.Fatalf("ResolvedAt is nil, want non-nil for terminal state")
		}
	})

	t.Run("does not set resolved_at for timeout", func(t *testing.T) {
		req := &db.Request{Status: db.StatusPending}
		if err := Transition(req, db.StatusTimeout); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		if req.Status != db.StatusTimeout {
			t.Fatalf("Status = %q, want %q", req.Status, db.StatusTimeout)
		}
		if req.ResolvedAt != nil {
			t.Fatalf("ResolvedAt = %v, want nil for non-terminal state", req.ResolvedAt)
		}
	})

	t.Run("sets resolved_at for executed", func(t *testing.T) {
		req := &db.Request{Status: db.StatusExecuting}
		if err := Transition(req, db.StatusExecuted); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		if req.Status != db.StatusExecuted {
			t.Fatalf("Status = %q, want %q", req.Status, db.StatusExecuted)
		}
		if req.ResolvedAt == nil {
			t.Fatalf("ResolvedAt is nil, want non-nil for terminal state")
		}
	})

	t.Run("sets approval_expires_at on approved transition (dangerous)", func(t *testing.T) {
		req := &db.Request{Status: db.StatusPending, RiskTier: db.RiskTierDangerous}

		before := time.Now().UTC()
		if err := Transition(req, db.StatusApproved); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		after := time.Now().UTC()

		if req.ApprovalExpiresAt == nil {
			t.Fatalf("ApprovalExpiresAt is nil, want non-nil after approved transition")
		}

		min := before.Add(defaultApprovalTTL)
		max := after.Add(defaultApprovalTTL)
		if req.ApprovalExpiresAt.Before(min) || req.ApprovalExpiresAt.After(max) {
			t.Fatalf("ApprovalExpiresAt = %s, want between [%s, %s]", req.ApprovalExpiresAt.Format(time.RFC3339), min.Format(time.RFC3339), max.Format(time.RFC3339))
		}
	})

	t.Run("sets shorter approval_expires_at for critical", func(t *testing.T) {
		req := &db.Request{Status: db.StatusPending, RiskTier: db.RiskTierCritical}

		before := time.Now().UTC()
		if err := Transition(req, db.StatusApproved); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		after := time.Now().UTC()

		if req.ApprovalExpiresAt == nil {
			t.Fatalf("ApprovalExpiresAt is nil, want non-nil after approved transition")
		}

		min := before.Add(defaultApprovalTTLCritical)
		max := after.Add(defaultApprovalTTLCritical)
		if req.ApprovalExpiresAt.Before(min) || req.ApprovalExpiresAt.After(max) {
			t.Fatalf("ApprovalExpiresAt = %s, want between [%s, %s]", req.ApprovalExpiresAt.Format(time.RFC3339), min.Format(time.RFC3339), max.Format(time.RFC3339))
		}
	})
}

func TestTransitionRejectsInvalidMoves(t *testing.T) {
	req := &db.Request{Status: db.StatusPending}
	if err := Transition(req, db.StatusExecuting); err == nil {
		t.Fatalf("expected error for invalid transition")
	}
	if req.Status != db.StatusPending {
		t.Fatalf("Status changed on invalid transition: %q", req.Status)
	}
	if req.ResolvedAt != nil {
		t.Fatalf("ResolvedAt changed on invalid transition: %v", req.ResolvedAt)
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusPending, false},
		{db.StatusApproved, false},
		{db.StatusRejected, true},
		{db.StatusExecuting, false},
		{db.StatusExecuted, true},
		{db.StatusExecutionFailed, true},
		{db.StatusCancelled, true},
		{db.StatusTimeout, false},
		{db.StatusTimedOut, true},
		{db.StatusEscalated, false},
	}

	for _, tt := range tests {
		if got := IsTerminal(tt.status); got != tt.want {
			t.Fatalf("IsTerminal(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestTransitionRejectsSameState(t *testing.T) {
	req := &db.Request{Status: db.StatusPending}
	if err := Transition(req, db.StatusPending); err == nil {
		t.Fatalf("expected error for same-state transition")
	}
}

func TestTransitionError_Error(t *testing.T) {
	err := &TransitionError{
		From:    db.StatusPending,
		To:      db.StatusExecuted,
		Message: "transition not allowed",
	}
	got := err.Error()
	want := "invalid transition from pending to executed: transition not allowed"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestValidateTransition_TerminalState(t *testing.T) {
	err := ValidateTransition(db.StatusExecuted, db.StatusPending)
	if err == nil {
		t.Fatal("expected error for transition from terminal state")
	}
	terr, ok := err.(*TransitionError)
	if !ok {
		t.Fatalf("expected TransitionError, got %T", err)
	}
	if terr.From != db.StatusExecuted || terr.To != db.StatusPending {
		t.Errorf("unexpected error fields: from=%q to=%q", terr.From, terr.To)
	}
}

func TestValidateTransition_InvalidTransition(t *testing.T) {
	err := ValidateTransition(db.StatusPending, db.StatusExecuting)
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
}

func TestValidateTransition_ValidTransition(t *testing.T) {
	err := ValidateTransition(db.StatusPending, db.StatusApproved)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTransitionWithReason(t *testing.T) {
	req := &db.Request{Status: db.StatusPending}
	err := TransitionWithReason(req, db.StatusApproved, "manually approved by admin")
	if err != nil {
		t.Fatalf("TransitionWithReason() error = %v", err)
	}
	if req.Status != db.StatusApproved {
		t.Errorf("Status = %q, want %q", req.Status, db.StatusApproved)
	}
}

func TestTransitionWithReason_Invalid(t *testing.T) {
	req := &db.Request{Status: db.StatusPending}
	err := TransitionWithReason(req, db.StatusExecuted, "invalid")
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
}

func TestGetValidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from db.RequestStatus
		want []db.RequestStatus
	}{
		{"empty->pending", "", []db.RequestStatus{db.StatusPending}},
		{"pending", db.StatusPending, []db.RequestStatus{db.StatusApproved, db.StatusRejected, db.StatusCancelled, db.StatusTimeout}},
		{"approved", db.StatusApproved, []db.RequestStatus{db.StatusExecuting, db.StatusCancelled}},
		{"executing", db.StatusExecuting, []db.RequestStatus{db.StatusExecuted, db.StatusExecutionFailed, db.StatusTimedOut, db.StatusApproved}},
		{"timeout", db.StatusTimeout, []db.RequestStatus{db.StatusEscalated}},
		{"escalated", db.StatusEscalated, []db.RequestStatus{db.StatusApproved, db.StatusRejected}},
		{"terminal (executed)", db.StatusExecuted, nil},
		{"terminal (rejected)", db.StatusRejected, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetValidTransitions(tt.from)
			if len(got) != len(tt.want) {
				t.Fatalf("GetValidTransitions(%q) = %v, want %v", tt.from, got, tt.want)
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Fatalf("GetValidTransitions(%q)[%d] = %q, want %q", tt.from, i, v, tt.want[i])
				}
			}
		})
	}
}

func TestIsPending(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusPending, true},
		{db.StatusApproved, false},
		{db.StatusExecuting, false},
		{db.StatusExecuted, false},
	}

	for _, tt := range tests {
		if got := IsPending(tt.status); got != tt.want {
			t.Errorf("IsPending(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestIsApproved(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusApproved, true},
		{db.StatusPending, false},
		{db.StatusExecuting, false},
		{db.StatusExecuted, false},
	}

	for _, tt := range tests {
		if got := IsApproved(tt.status); got != tt.want {
			t.Errorf("IsApproved(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestIsComplete(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusExecuted, true},
		{db.StatusExecutionFailed, true},
		{db.StatusRejected, true},
		{db.StatusCancelled, true},
		{db.StatusTimedOut, true},
		{db.StatusPending, false},
		{db.StatusApproved, false},
		{db.StatusExecuting, false},
	}

	for _, tt := range tests {
		if got := IsComplete(tt.status); got != tt.want {
			t.Errorf("IsComplete(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestRequiresApproval(t *testing.T) {
	tests := []struct {
		name             string
		status           db.RequestStatus
		minApprovals     int
		currentApprovals int
		want             bool
	}{
		{"pending needs more", db.StatusPending, 2, 1, true},
		{"pending has enough", db.StatusPending, 2, 2, false},
		{"pending has more than enough", db.StatusPending, 1, 3, false},
		{"not pending", db.StatusApproved, 2, 0, false},
		{"pending needs zero", db.StatusPending, 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &db.Request{Status: tt.status, MinApprovals: tt.minApprovals}
			if got := RequiresApproval(req, tt.currentApprovals); got != tt.want {
				t.Errorf("RequiresApproval() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanApprove(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusPending, true},
		{db.StatusApproved, false},
		{db.StatusExecuting, false},
		{db.StatusExecuted, false},
		{db.StatusRejected, false},
	}

	for _, tt := range tests {
		if got := CanApprove(tt.status); got != tt.want {
			t.Errorf("CanApprove(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestCanExecute(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusApproved, true},
		{db.StatusPending, false},
		{db.StatusExecuting, false},
		{db.StatusExecuted, false},
	}

	for _, tt := range tests {
		if got := CanExecute(tt.status); got != tt.want {
			t.Errorf("CanExecute(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestCanCancel(t *testing.T) {
	tests := []struct {
		status db.RequestStatus
		want   bool
	}{
		{db.StatusPending, true},
		{db.StatusApproved, true},
		{db.StatusExecuting, false},
		{db.StatusExecuted, false},
		{db.StatusRejected, false},
	}

	for _, tt := range tests {
		if got := CanCancel(tt.status); got != tt.want {
			t.Errorf("CanCancel(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestCheckExpiry(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	tests := []struct {
		name       string
		status     db.RequestStatus
		expiresAt  *time.Time
		wantStatus db.RequestStatus
		wantExpiry bool
	}{
		{"not pending", db.StatusApproved, &past, "", false},
		{"no expiry set", db.StatusPending, nil, "", false},
		{"expired", db.StatusPending, &past, db.StatusTimeout, true},
		{"not expired", db.StatusPending, &future, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &db.Request{Status: tt.status, ExpiresAt: tt.expiresAt}
			gotStatus, gotExpiry := CheckExpiry(req)
			if gotStatus != tt.wantStatus || gotExpiry != tt.wantExpiry {
				t.Errorf("CheckExpiry() = (%q, %v), want (%q, %v)", gotStatus, gotExpiry, tt.wantStatus, tt.wantExpiry)
			}
		})
	}
}

func TestCheckApprovalExpiry(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	tests := []struct {
		name              string
		status            db.RequestStatus
		approvalExpiresAt *time.Time
		want              bool
	}{
		{"not approved", db.StatusPending, &past, false},
		{"no approval expiry set", db.StatusApproved, nil, false},
		{"approval expired", db.StatusApproved, &past, true},
		{"approval not expired", db.StatusApproved, &future, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &db.Request{Status: tt.status, ApprovalExpiresAt: tt.approvalExpiresAt}
			if got := CheckApprovalExpiry(req); got != tt.want {
				t.Errorf("CheckApprovalExpiry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStateMachine(t *testing.T) {
	t.Run("NewStateMachine", func(t *testing.T) {
		sm := NewStateMachine()
		if sm == nil {
			t.Fatal("NewStateMachine() returned nil")
		}
	})

	t.Run("Transition", func(t *testing.T) {
		sm := NewStateMachine()
		req := &db.Request{Status: db.StatusPending}
		if err := sm.Transition(req, db.StatusApproved); err != nil {
			t.Fatalf("Transition() error = %v", err)
		}
		if req.Status != db.StatusApproved {
			t.Errorf("Status = %q, want %q", req.Status, db.StatusApproved)
		}
	})

	t.Run("CanTransition", func(t *testing.T) {
		sm := NewStateMachine()
		if !sm.CanTransition(db.StatusPending, db.StatusApproved) {
			t.Error("CanTransition(pending, approved) = false, want true")
		}
		if sm.CanTransition(db.StatusPending, db.StatusExecuted) {
			t.Error("CanTransition(pending, executed) = true, want false")
		}
	})
}
