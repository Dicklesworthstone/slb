package db

import (
	"crypto/hmac"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrReviewNotPending         = errors.New("request is not pending")
	ErrReviewExpired            = errors.New("request has expired")
	ErrReviewSessionInactive    = errors.New("session is no longer active")
	ErrReviewSessionKeyMismatch = errors.New("session key does not match session")
	ErrReviewDifferentModel     = errors.New("different model required for approval")
	ErrReviewInvalidDecision    = errors.New("invalid decision (must be approve or reject)")
)

// ReviewPolicy is the policy applied while committing a review. Zero TTLs use
// finite defaults; negative durations and unknown conflict modes are rejected.
// The caller loads this policy from the target project's configuration.
type ReviewPolicy struct {
	ConflictResolution      string
	TrustedSelfApprove      []string
	TrustedSelfApproveDelay time.Duration
	ApprovalTTL             time.Duration
	CriticalApprovalTTL     time.Duration
}

// ReviewOutcome describes only committed state. Notifications must use Request,
// not a request read before the transaction acquired its write lock.
type ReviewOutcome struct {
	Request       *Request
	Review        *Review
	Approvals     int
	Rejections    int
	StatusChanged bool
}

// ApplyReview authenticates, validates, inserts and resolves a review in one
// transaction. An empty signature is signed using the authenticated session
// key; a supplied signature is verified against the persisted key. No caller
// supplied reviewer name or model is trusted.
func (db *DB) ApplyReview(review *Review, sessionKey string, policy ReviewPolicy) (*ReviewOutcome, error) {
	if review == nil || review.RequestID == "" || review.ReviewerSessionID == "" {
		return nil, fmt.Errorf("request_id and reviewer_session_id are required")
	}
	if sessionKey == "" {
		return nil, fmt.Errorf("session key required for signature")
	}
	if review.Decision != DecisionApprove && review.Decision != DecisionReject {
		return nil, ErrReviewInvalidDecision
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	// Do not change the caller's review object when the transaction rolls back.
	accepted := *review
	var outcome *ReviewOutcome
	err := db.Transaction(func(tx *sql.Tx) error {
		// Acquire SQLite's writer reservation BEFORE reading any eligibility
		// state. A deferred read-then-write transaction can otherwise fail to
		// upgrade its snapshot or apply checks from outside the transaction.
		locked, err := tx.Exec(`UPDATE requests SET id = id WHERE id = ?`, accepted.RequestID)
		if err != nil {
			return fmt.Errorf("locking review request: %w", err)
		}
		count, err := locked.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrRequestNotFound
		}
		request, err := db.GetRequestTx(tx, accepted.RequestID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if request.Status != StatusPending && request.Status != StatusEscalated {
			return fmt.Errorf("%w: status is %s", ErrReviewNotPending, request.Status)
		}
		// Escalation explicitly reopens human review after a pending timeout.
		if request.Status == StatusPending && request.ExpiresAt != nil && !now.Before(*request.ExpiresAt) {
			return ErrReviewExpired
		}
		if request.Command.Hash == "" || ComputeCommandHash(request.Command) != request.Command.Hash {
			return fmt.Errorf("%w: command changed since request creation", ErrInvalidSignature)
		}
		if request.MinApprovals < 0 {
			return fmt.Errorf("invalid negative approval quorum")
		}

		var agent, model, storedKey string
		var ended sql.NullString
		err = tx.QueryRow(`SELECT agent_name, COALESCE(model, ''), session_key, ended_at FROM sessions WHERE id = ?`,
			accepted.ReviewerSessionID).Scan(&agent, &model, &storedKey, &ended)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return fmt.Errorf("reading review session: %w", err)
		}
		if ended.Valid {
			return ErrReviewSessionInactive
		}
		if !hmac.Equal([]byte(sessionKey), []byte(storedKey)) {
			return ErrReviewSessionKeyMismatch
		}
		// Restarting a session does not create an independent second agent.
		self := accepted.ReviewerSessionID == request.RequestorSessionID || agent == request.RequestorAgent
		if self {
			trusted := false
			for _, name := range policy.TrustedSelfApprove {
				trusted = trusted || name == agent
			}
			if !trusted {
				return ErrSelfReview
			}
			if now.Sub(request.CreatedAt) < policy.TrustedSelfApproveDelay {
				return fmt.Errorf("trusted self-approve requires %v delay", policy.TrustedSelfApproveDelay)
			}
		}
		if accepted.Decision == DecisionApprove && request.RequireDifferentModel && model == request.RequestorModel {
			return ErrReviewDifferentModel
		}
		duplicate, err := db.HasReviewerAlreadyReviewedTx(tx, request.ID, accepted.ReviewerSessionID)
		if err != nil {
			return err
		}
		if duplicate {
			return ErrReviewExists
		}
		// A session restart cannot supply a second vote from the same agent.
		var sameAgent int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM reviews WHERE request_id = ? AND reviewer_agent = ?`,
			request.ID, agent).Scan(&sameAgent); err != nil {
			return err
		}
		if sameAgent > 0 {
			return ErrReviewExists
		}
		accepted.ReviewerAgent, accepted.ReviewerModel = agent, model
		if accepted.Signature == "" {
			accepted.SignatureTimestamp = now.Truncate(time.Second)
			accepted.Signature = ComputeReviewSignature(storedKey, request.ID, accepted.Decision, accepted.SignatureTimestamp)
		} else {
			if accepted.SignatureTimestamp.IsZero() || accepted.SignatureTimestamp.Before(now.Add(-5*time.Minute)) ||
				accepted.SignatureTimestamp.After(now.Add(time.Minute)) ||
				!VerifyReviewSignature(storedKey, request.ID, accepted.Decision, accepted.SignatureTimestamp, accepted.Signature) {
				return ErrInvalidSignature
			}
		}
		accepted.CreatedAt = now.Truncate(time.Second)
		if err := db.CreateReviewTx(tx, &accepted); err != nil {
			return err
		}
		approvals, rejections, err := db.CountReviewsByDecisionTx(tx, request.ID)
		if err != nil {
			return err
		}
		var first string
		if err := tx.QueryRow(`SELECT decision FROM reviews WHERE request_id = ? ORDER BY rowid LIMIT 1`, request.ID).Scan(&first); err != nil {
			return err
		}
		status := policy.resolvedStatus(request.MinApprovals, approvals, rejections, Decision(first))
		outcome = &ReviewOutcome{Request: request, Review: &accepted, Approvals: approvals, Rejections: rejections}
		if status != "" && status != request.Status {
			if err := db.ResolveReviewTx(tx, request, status, policy.approvalTTL(request.RiskTier), now); err != nil {
				return err
			}
			outcome.StatusChanged = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	*review = accepted
	return outcome, nil
}

func (p ReviewPolicy) validate() error {
	switch p.ConflictResolution {
	case "", "any_rejection_blocks", "first_wins", "human_breaks_tie":
	default:
		return fmt.Errorf("invalid review conflict resolution %q", p.ConflictResolution)
	}
	if p.ApprovalTTL < 0 || p.CriticalApprovalTTL < 0 || p.TrustedSelfApproveDelay < 0 {
		return fmt.Errorf("review policy durations must not be negative")
	}
	return nil
}

func (p ReviewPolicy) approvalTTL(tier RiskTier) time.Duration {
	if tier == RiskTierCritical {
		if p.CriticalApprovalTTL > 0 {
			return p.CriticalApprovalTTL
		}
		return 10 * time.Minute
	}
	if p.ApprovalTTL > 0 {
		return p.ApprovalTTL
	}
	return 30 * time.Minute
}

func (p ReviewPolicy) resolvedStatus(quorum, approvals, rejections int, first Decision) RequestStatus {
	switch p.ConflictResolution {
	case "first_wins":
		if first == DecisionReject {
			return StatusRejected
		}
		// The first decision chooses the conflict outcome, not the quorum.
		if approvals >= quorum {
			return StatusApproved
		}
	case "human_breaks_tie":
		if approvals > 0 && rejections > 0 {
			return StatusEscalated
		}
		if rejections > 0 {
			return StatusRejected
		}
		if approvals >= quorum {
			return StatusApproved
		}
	default:
		if rejections > 0 {
			return StatusRejected
		}
		if approvals >= quorum {
			return StatusApproved
		}
	}
	return ""
}

// ResolveReviewTx persists approval state and its expiry together. It must be
// called in the same transaction that validated and wrote the decisive review.
// It also supports pending -> escalated for human_breaks_tie, which the general
// execution state machine intentionally does not offer.
func (db *DB) ResolveReviewTx(tx *sql.Tx, request *Request, status RequestStatus, ttl time.Duration, now time.Time) error {
	if request == nil || (request.Status != StatusPending && request.Status != StatusEscalated) ||
		(status != StatusApproved && status != StatusRejected && status != StatusEscalated) {
		return ErrInvalidTransition
	}
	var expiry *time.Time
	if status == StatusApproved {
		if ttl <= 0 {
			return fmt.Errorf("approval TTL must be positive")
		}
		expires := now.Add(ttl).UTC()
		expiry = &expires
	}
	result, err := tx.Exec(`UPDATE requests SET status = ?, resolved_at = ?, approval_expires_at = ? WHERE id = ? AND status = ?`,
		string(status), now.UTC().Format(time.RFC3339Nano), formatTimePtr(expiry), request.ID, string(request.Status))
	if err != nil {
		return fmt.Errorf("resolving review: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrInvalidTransition
	}
	request.Status, request.ResolvedAt, request.ApprovalExpiresAt = status, &now, expiry
	return nil
}
