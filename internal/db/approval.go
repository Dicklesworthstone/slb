package db

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidApprovalProof means the persisted decision cannot authorize an
// execution. Callers must obtain a new review rather than trust the status bit.
var ErrInvalidApprovalProof = errors.New("invalid approval proof")

// VerifyRequestApproval checks a consistent snapshot of the request and its
// supporting reviews. This is advisory, not an execution permit: the actual
// executor must repeat verification in ClaimRequestExecution's transaction.
func (db *DB) VerifyRequestApproval(id string) (*Request, error) {
	var request *Request
	err := db.Transaction(func(tx *sql.Tx) error {
		var err error
		request, err = db.GetRequestTx(tx, id)
		if err != nil {
			return err
		}
		return verifyApprovalTx(tx, request, time.Now().UTC())
	})
	if err != nil {
		return nil, err
	}
	return request, nil
}

// verifyApprovalTx verifies persisted evidence, not incoming-review freshness.
// A valid review can be older than the five-minute submission window and can
// survive its reviewer's session ending. Rotating the stored signing key, on
// the other hand, invalidates signatures made with the old key.
//
// Request creation/decision policy owns self-review exceptions and conflict
// resolution. In particular, first_wins may intentionally approve despite a
// later rejection. Do not reinterpret those decisions here; require the stored
// quorum of distinct, authentic approvals and any different-model constraint.
// Session keys and the database are local trust material, not tamper-proof
// storage or authentication of an agent's self-reported model identity.
func verifyApprovalTx(tx *sql.Tx, request *Request, now time.Time) error {
	deny := func(reason string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidApprovalProof, fmt.Sprintf(reason, args...))
	}
	if request.Status != StatusApproved {
		return deny("request status is %s, expected approved", request.Status)
	}
	if request.Command.Hash == "" || ComputeCommandHash(request.Command) != request.Command.Hash {
		return deny("command hash mismatch")
	}
	switch request.RiskTier {
	case RiskTierCaution, RiskTierDangerous, RiskTierCritical:
	default:
		return deny("unknown request risk tier %q", request.RiskTier)
	}
	if request.MinApprovals < 0 || (request.MinApprovals == 0 && request.RiskTier != RiskTierCaution) {
		return deny("invalid approval quorum for %s", request.RiskTier)
	}
	if request.ApprovalExpiresAt == nil {
		// Zero-review CAUTION requests are the explicit auto-approval path.
		// Older such requests have no TTL; human/peer approvals must have one.
		if request.MinApprovals > 0 || request.RequireDifferentModel {
			return deny("approval_expires_at is not set; submit a new request for review")
		}
	} else if !now.Before(*request.ApprovalExpiresAt) {
		return deny("approval has expired")
	}

	rows, err := tx.Query(`
		SELECT r.reviewer_session_id, r.reviewer_agent, r.reviewer_model,
			r.decision, r.signature, r.signature_timestamp,
			s.session_key, s.agent_name, s.model
		FROM reviews r LEFT JOIN sessions s ON s.id = r.reviewer_session_id
		WHERE r.request_id = ? ORDER BY r.rowid
	`, request.ID)
	if err != nil {
		return fmt.Errorf("reading approval evidence: %w", err)
	}
	defer rows.Close()
	seenAgents := make(map[string]bool)
	approvals, differentModel := 0, false
	for rows.Next() {
		var sessionID, agent, model, decision, signature, timestamp string
		var key, sessionAgent, sessionModel sql.NullString
		if err := rows.Scan(&sessionID, &agent, &model, &decision, &signature, &timestamp, &key, &sessionAgent, &sessionModel); err != nil {
			return fmt.Errorf("reading approval evidence: %w", err)
		}
		if !key.Valid || !sessionAgent.Valid || agent == "" || sessionAgent.String != agent || sessionModel.String != model {
			return deny("reviewer identity is missing or changed for session %s", sessionID)
		}
		decodedKey, err := hex.DecodeString(key.String)
		if err != nil || len(decodedKey) != 32 {
			return deny("invalid signing key for reviewer session %s", sessionID)
		}
		if decision != string(DecisionApprove) && decision != string(DecisionReject) {
			return deny("invalid review decision")
		}
		signedAt, err := time.Parse(time.RFC3339, timestamp)
		if err != nil || signedAt.IsZero() || signedAt.After(now.Add(time.Minute)) ||
			!VerifyReviewSignature(key.String, request.ID, Decision(decision), signedAt, signature) {
			return deny("invalid signature for reviewer session %s", sessionID)
		}
		if seenAgents[agent] {
			return deny("duplicate reviewer agent %s", agent)
		}
		seenAgents[agent] = true
		if decision == string(DecisionApprove) {
			approvals++
			differentModel = differentModel || model != request.RequestorModel
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading approval evidence: %w", err)
	}
	if approvals < request.MinApprovals {
		return deny("insufficient authentic approvals: %d < %d required", approvals, request.MinApprovals)
	}
	if request.RequireDifferentModel && !differentModel {
		return deny("different model approval is required")
	}
	return nil
}
