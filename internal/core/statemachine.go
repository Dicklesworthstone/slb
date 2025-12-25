// Package core implements the request lifecycle state machine.
package core

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

const (
	defaultApprovalTTL         = 30 * time.Minute
	defaultApprovalTTLCritical = 10 * time.Minute
)

// validTransitions defines all valid state transitions.
// Map key is the from state, value is a list of valid to states.
var validTransitions = map[db.RequestStatus][]db.RequestStatus{
	db.StatusPending: {
		db.StatusApproved,
		db.StatusRejected,
		db.StatusCancelled,
		db.StatusTimeout,
	},
	db.StatusApproved: {
		db.StatusExecuting,
		db.StatusCancelled,
	},
	db.StatusExecuting: {
		db.StatusExecuted,
		db.StatusExecutionFailed,
		db.StatusTimedOut,
		db.StatusApproved, // Allow revert if execution fails before command starts
	},
	db.StatusTimeout: {
		db.StatusEscalated,
	},
	db.StatusEscalated: {
		db.StatusApproved, // Human can approve after escalation
		db.StatusRejected, // Human can reject after escalation
	},
}

// TerminalStates are states from which no further transitions are allowed.
var TerminalStates = map[db.RequestStatus]bool{
	db.StatusExecuted:        true,
	db.StatusExecutionFailed: true,
	db.StatusTimedOut:        true,
	db.StatusCancelled:       true,
	db.StatusRejected:        true,
}

// TransitionError represents an invalid state transition.
type TransitionError struct {
	From    db.RequestStatus
	To      db.RequestStatus
	Message string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid transition from %s to %s: %s", e.From, e.To, e.Message)
}

// CanTransition returns true if the transition from one state to another is valid.
func CanTransition(from, to db.RequestStatus) bool {
	// Allow creation-time transition.
	if from == "" && to == db.StatusPending {
		return true
	}

	// Cannot transition from terminal states
	if TerminalStates[from] {
		return false
	}

	// Check if transition is in the valid list
	validTargets, exists := validTransitions[from]
	if !exists {
		return false
	}

	for _, target := range validTargets {
		if target == to {
			return true
		}
	}

	return false
}

// ValidateTransition validates a state transition and returns an error if invalid.
func ValidateTransition(from, to db.RequestStatus) error {
	if TerminalStates[from] {
		return &TransitionError{
			From:    from,
			To:      to,
			Message: fmt.Sprintf("%s is a terminal state", from),
		}
	}

	if !CanTransition(from, to) {
		return &TransitionError{
			From:    from,
			To:      to,
			Message: "transition not allowed",
		}
	}

	return nil
}

// Transition attempts to transition a request to a new state.
// Returns an error if the transition is invalid.
func Transition(req *db.Request, to db.RequestStatus) error {
	if err := ValidateTransition(req.Status, to); err != nil {
		return err
	}

	// Update the request
	now := time.Now().UTC()
	if req.Status == "" && to == db.StatusPending && req.CreatedAt.IsZero() {
		req.CreatedAt = now
	}
	req.Status = to

	if to == db.StatusApproved && req.ApprovalExpiresAt == nil {
		ttl := defaultApprovalTTL
		if req.RiskTier == db.RiskTierCritical {
			ttl = defaultApprovalTTLCritical
		}
		expiresAt := now.Add(ttl)
		req.ApprovalExpiresAt = &expiresAt
	}

	// Set resolved timestamp for terminal states
	if TerminalStates[to] {
		req.ResolvedAt = &now
	}

	return nil
}

// TransitionWithReason transitions a request and logs the reason.
// This is useful for audit logging.
func TransitionWithReason(req *db.Request, to db.RequestStatus, reason string) error {
	if err := Transition(req, to); err != nil {
		return err
	}
	// Reason could be logged to the request's audit trail
	// For now, just return success
	return nil
}

// GetValidTransitions returns all valid target states from the given state.
func GetValidTransitions(from db.RequestStatus) []db.RequestStatus {
	if from == "" {
		return []db.RequestStatus{db.StatusPending}
	}
	if TerminalStates[from] {
		return nil
	}
	return validTransitions[from]
}

// IsTerminal returns true if the status is a terminal state.
func IsTerminal(status db.RequestStatus) bool {
	return TerminalStates[status]
}

// IsPending returns true if the status indicates the request needs action.
func IsPending(status db.RequestStatus) bool {
	return status == db.StatusPending
}

// IsApproved returns true if the request has been approved.
func IsApproved(status db.RequestStatus) bool {
	return status == db.StatusApproved
}

// IsComplete returns true if the request has reached a terminal state.
func IsComplete(status db.RequestStatus) bool {
	return IsTerminal(status)
}

// RequiresApproval checks if a request still needs approvals.
func RequiresApproval(req *db.Request, currentApprovals int) bool {
	if req.Status != db.StatusPending {
		return false
	}
	return currentApprovals < req.MinApprovals
}

// CanApprove checks if a request can receive more approvals.
func CanApprove(status db.RequestStatus) bool {
	return status == db.StatusPending
}

// CanExecute checks if a request can be executed.
func CanExecute(status db.RequestStatus) bool {
	return status == db.StatusApproved
}

// CanCancel checks if a request can be cancelled.
func CanCancel(status db.RequestStatus) bool {
	return status == db.StatusPending || status == db.StatusApproved
}

// CheckExpiry checks if a pending request has expired.
// Returns the appropriate status transition if expired.
func CheckExpiry(req *db.Request) (db.RequestStatus, bool) {
	if req.Status != db.StatusPending {
		return "", false
	}

	if req.ExpiresAt == nil {
		return "", false
	}

	if time.Now().After(*req.ExpiresAt) {
		return db.StatusTimeout, true
	}

	return "", false
}

// CheckApprovalExpiry checks if an approved request's approval has become stale.
func CheckApprovalExpiry(req *db.Request) bool {
	if req.Status != db.StatusApproved {
		return false
	}

	if req.ApprovalExpiresAt == nil {
		return false
	}

	return time.Now().After(*req.ApprovalExpiresAt)
}

// StateMachine provides request state management.
type StateMachine struct{}

// NewStateMachine creates a new state machine.
func NewStateMachine() *StateMachine {
	return &StateMachine{}
}

// Transition transitions a request to a new state.
func (sm *StateMachine) Transition(req *db.Request, to db.RequestStatus) error {
	return Transition(req, to)
}

// CanTransition checks if a transition is valid.
func (sm *StateMachine) CanTransition(from, to db.RequestStatus) bool {
	return CanTransition(from, to)
}
