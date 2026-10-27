package db

import (
	"crypto/sha256"
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
// Independent-review policies are refused, including historical local HMAC
// approvals: this backend has no external principal/model authenticator.
// Same-host requests use separately labeled, versioned evidence. Their local
// signatures catch mistakes and mutations, not malicious same-UID processes.
// Session keys and the database are local trust material, not tamper-proof
// storage or authentication of an agent's self-reported identity.
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
	if !request.ApprovalPolicy.Valid() {
		return deny("unknown approval policy %q", request.ApprovalPolicy)
	}
	switch request.RiskTier {
	case RiskTierCaution, RiskTierDangerous, RiskTierCritical:
	default:
		return deny("unknown request risk tier %q", request.RiskTier)
	}
	if request.MinApprovals < 0 || (request.MinApprovals == 0 && request.RiskTier != RiskTierCaution) {
		return deny("invalid approval quorum for %s", request.RiskTier)
	}
	if request.ApprovalPolicy.Normalized() == ApprovalPolicyIndependent &&
		(request.MinApprovals > 0 || request.RequireDifferentModel) && !independentIdentityAttested() {
		// Recheck at execution, including historical approvals created before
		// this fix or by direct database writes. A status bit and local HMACs
		// cannot attest independent reviewer identity.
		return fmt.Errorf("%w: %w", ErrInvalidApprovalProof, ErrIndependentIdentityUnavailable)
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
	proof, err := verifyReviewEvidenceTx(tx, request, now)
	if err != nil {
		return err
	}
	if proof.approvals < request.MinApprovals {
		return deny("insufficient authentic approvals: %d < %d required", proof.approvals, request.MinApprovals)
	}
	if request.RequireDifferentModel && !proof.differentModel {
		return deny("different model approval is required")
	}
	return nil
}

type reviewEvidenceCounts struct {
	approvals, rejections int
	differentModel        bool
}

// Verify every submitted vote before counting it. This also runs when a
// same-host review is added, so an older or relabeled row cannot silently help
// a new request reach quorum, even before its execution-time recheck.
func verifyReviewEvidenceTx(tx *sql.Tx, request *Request, now time.Time) (reviewEvidenceCounts, error) {
	var proof reviewEvidenceCounts
	deny := func(reason string, args ...any) (reviewEvidenceCounts, error) {
		return proof, fmt.Errorf("%w: %s", ErrInvalidApprovalProof, fmt.Sprintf(reason, args...))
	}
	rows, err := tx.Query(`
		SELECT r.reviewer_session_id, r.reviewer_agent, r.reviewer_model,
			r.decision, r.signature, r.signature_timestamp,
			s.session_key_digest, s.agent_name, s.model, s.project_path, s.program, s.ended_at,
			r.approval_policy, r.assurance, r.command_hash, r.request_binding, r.signature_version
		FROM reviews r LEFT JOIN sessions s ON s.id = r.reviewer_session_id
		WHERE r.request_id = ? ORDER BY r.rowid
	`, request.ID)
	if err != nil {
		return proof, fmt.Errorf("reading approval evidence: %w", err)
	}
	defer rows.Close()
	seenAgents := make(map[string]bool)
	for rows.Next() {
		var sessionID, agent, model, decision, signature, timestamp string
		var digest, sessionAgent, sessionModel, sessionProject, sessionProgram, sessionEnded sql.NullString
		review := Review{RequestID: request.ID}
		if err := rows.Scan(&sessionID, &agent, &model, &decision, &signature, &timestamp, &digest, &sessionAgent, &sessionModel, &sessionProject, &sessionProgram, &sessionEnded,
			&review.ApprovalPolicy, &review.Assurance, &review.CommandHash, &review.RequestBinding, &review.SignatureVersion); err != nil {
			return proof, fmt.Errorf("reading approval evidence: %w", err)
		}
		if !sessionAgent.Valid || agent == "" || sessionAgent.String != agent || sessionModel.String != model {
			return deny("reviewer identity is missing or changed for session %s", sessionID)
		}
		// Delegated reviews have an ended provenance session in the target
		// project. A foreign local session is never equivalent, even when a
		// shared database or older binary previously admitted its signature.
		if !sessionProject.Valid || sessionProject.String != request.ProjectPath {
			return deny("reviewer session %s belongs to another project", sessionID)
		}
		if _, err := ValidateReviewSessionProvenance(sessionID, sessionProgram.String, sessionEnded.Valid); err != nil {
			return deny("invalid cross-project provenance for reviewer session %s", sessionID)
		}
		if decoded, err := hex.DecodeString(digest.String); err != nil || len(decoded) != sha256.Size {
			return deny("invalid signing key for reviewer session %s", sessionID)
		}
		if decision != string(DecisionApprove) && decision != string(DecisionReject) {
			return deny("invalid review decision")
		}
		signedAt, err := time.Parse(time.RFC3339, timestamp)
		review.ReviewerSessionID, review.ReviewerAgent, review.ReviewerModel = sessionID, agent, model
		review.Decision, review.Signature, review.SignatureTimestamp = Decision(decision), signature, signedAt
		if !reviewMatchesRequest(&review, request) {
			return deny("review evidence policy, assurance or command binding mismatch for session %s", sessionID)
		}
		if err != nil || signedAt.IsZero() || signedAt.After(now.Add(time.Minute)) ||
			!verifyReviewEvidenceWithDigest(digest.String, &review) {
			return deny("invalid signature for reviewer session %s", sessionID)
		}
		if request.ApprovalPolicy == ApprovalPolicySameHost && (sessionID == request.RequestorSessionID || agent == request.RequestorAgent) {
			return deny("same-host self-review is not allowed")
		}
		if seenAgents[agent] {
			return deny("duplicate reviewer agent %s", agent)
		}
		seenAgents[agent] = true
		if decision == string(DecisionApprove) {
			differentModel := ModelsDiffer(request.RequestorModel, model)
			if request.ApprovalPolicy == ApprovalPolicySameHost && request.RequireDifferentModel && !differentModel {
				return deny("different model approval is required")
			}
			proof.approvals++
			proof.differentModel = proof.differentModel || differentModel
		} else {
			proof.rejections++
		}
	}
	if err := rows.Err(); err != nil {
		return proof, fmt.Errorf("reading approval evidence: %w", err)
	}
	return proof, nil
}
