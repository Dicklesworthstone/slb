package db

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrReviewExists indicates a duplicate review for the same request+reviewer.
var ErrReviewExists = errors.New("review already exists for this request and reviewer")

// ErrSelfReview indicates a session tried to review its own request.
var ErrSelfReview = errors.New("cannot review your own request")

// ErrInvalidSignature indicates the review signature is invalid.
var ErrInvalidSignature = errors.New("invalid review signature")

// CreateReviewTx inserts a review within a transaction.
func (db *DB) CreateReviewTx(tx *sql.Tx, r *Review) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.SignatureTimestamp.IsZero() {
		r.SignatureTimestamp = now
	}

	respJSON, _ := json.Marshal(r.Responses)

	_, err := tx.Exec(`
		INSERT INTO reviews (
			id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
			decision, signature, signature_timestamp,
			responses_json, comments, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		r.ID, r.RequestID, r.ReviewerSessionID, r.ReviewerAgent, r.ReviewerModel,
		string(r.Decision), r.Signature, r.SignatureTimestamp.Format(time.RFC3339),
		nullString(string(respJSON)), nullString(r.Comments), r.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return ErrReviewExists
		}
		return fmt.Errorf("creating review: %w", err)
	}
	return nil
}

// CreateReview inserts a review, generating ID and timestamps if missing.
func (db *DB) CreateReview(r *Review) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.SignatureTimestamp.IsZero() {
		r.SignatureTimestamp = now
	}

	// Enforce unique (request_id, reviewer_session_id)
	if exists, err := db.HasReviewerAlreadyReviewed(r.RequestID, r.ReviewerSessionID); err != nil {
		return err
	} else if exists {
		return ErrReviewExists
	}

	respJSON, _ := json.Marshal(r.Responses)

	_, err := db.Exec(`
		INSERT INTO reviews (
			id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
			decision, signature, signature_timestamp,
			responses_json, comments, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		r.ID, r.RequestID, r.ReviewerSessionID, r.ReviewerAgent, r.ReviewerModel,
		string(r.Decision), r.Signature, r.SignatureTimestamp.Format(time.RFC3339),
		nullString(string(respJSON)), nullString(r.Comments), r.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return ErrReviewExists
		}
		return fmt.Errorf("creating review: %w", err)
	}
	return nil
}

// GetReview retrieves a review by ID.
func (db *DB) GetReview(id string) (*Review, error) {
	row := db.QueryRow(`
		SELECT id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
		       decision, signature, signature_timestamp, responses_json, comments, created_at
		FROM reviews WHERE id = ?
	`, id)
	return scanReviewRow(row)
}

// ListReviewsForRequest returns all reviews for a request ordered by created_at.
func (db *DB) ListReviewsForRequest(requestID string) ([]*Review, error) {
	rows, err := db.Query(`
		SELECT id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
		       decision, signature, signature_timestamp, responses_json, comments, created_at
		FROM reviews WHERE request_id = ?
		ORDER BY created_at ASC
	`, requestID)
	if err != nil {
		return nil, fmt.Errorf("listing reviews: %w", err)
	}
	defer rows.Close()
	return scanReviewList(rows)
}

// CountReviewsByDecisionTx returns counts of approvals and rejections for a request within a transaction.
func (db *DB) CountReviewsByDecisionTx(tx *sql.Tx, requestID string) (int, int, error) {
	var approvals, rejections sql.NullInt64
	err := tx.QueryRow(`
		SELECT
		  SUM(CASE WHEN decision = 'approve' THEN 1 ELSE 0 END),
		  SUM(CASE WHEN decision = 'reject' THEN 1 ELSE 0 END)
		FROM reviews WHERE request_id = ?
	`, requestID).Scan(&approvals, &rejections)
	if err != nil {
		return 0, 0, fmt.Errorf("counting reviews: %w", err)
	}
	return int(approvals.Int64), int(rejections.Int64), nil
}

// CountReviewsByDecision returns counts of approvals and rejections for a request.
func (db *DB) CountReviewsByDecision(requestID string) (int, int, error) {
	var approvals, rejections sql.NullInt64
	err := db.QueryRow(`
		SELECT
		  SUM(CASE WHEN decision = 'approve' THEN 1 ELSE 0 END),
		  SUM(CASE WHEN decision = 'reject' THEN 1 ELSE 0 END)
		FROM reviews WHERE request_id = ?
	`, requestID).Scan(&approvals, &rejections)
	if err != nil {
		return 0, 0, fmt.Errorf("counting reviews: %w", err)
	}
	return int(approvals.Int64), int(rejections.Int64), nil
}

// HasReviewerAlreadyReviewedTx checks if the reviewer has already reviewed the request within a transaction.
func (db *DB) HasReviewerAlreadyReviewedTx(tx *sql.Tx, requestID, sessionID string) (bool, error) {
	var count int
	err := tx.QueryRow(`
		SELECT COUNT(*) FROM reviews WHERE request_id = ? AND reviewer_session_id = ?
	`, requestID, sessionID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking duplicate review: %w", err)
	}
	return count > 0, nil
}

// HasReviewerAlreadyReviewed checks if the reviewer has already reviewed the request.
func (db *DB) HasReviewerAlreadyReviewed(requestID, sessionID string) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM reviews WHERE request_id = ? AND reviewer_session_id = ?
	`, requestID, sessionID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking duplicate review: %w", err)
	}
	return count > 0, nil
}

// IsRequestorSameAsReviewer checks if the reviewer session is the same as requestor session.
func (db *DB) IsRequestorSameAsReviewer(requestID, reviewerSessionID string) (bool, error) {
	var reqSessionID string
	err := db.QueryRow(`SELECT requestor_session_id FROM requests WHERE id = ?`, requestID).Scan(&reqSessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrRequestNotFound
		}
		return false, fmt.Errorf("fetching request: %w", err)
	}
	return reqSessionID == reviewerSessionID, nil
}

func scanReviewRow(row *sql.Row) (*Review, error) {
	r := &Review{}
	var decision string
	var sigTs, created string
	var responsesJSON sql.NullString
	var comments sql.NullString

	err := row.Scan(&r.ID, &r.RequestID, &r.ReviewerSessionID, &r.ReviewerAgent, &r.ReviewerModel,
		&decision, &r.Signature, &sigTs, &responsesJSON, &comments, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReviewNotFound
		}
		return nil, fmt.Errorf("scanning review: %w", err)
	}

	r.Decision = Decision(decision)
	r.SignatureTimestamp, _ = time.Parse(time.RFC3339, sigTs)
	r.CreatedAt, _ = time.Parse(time.RFC3339, created)

	if responsesJSON.Valid {
		_ = json.Unmarshal([]byte(responsesJSON.String), &r.Responses)
	}
	if comments.Valid {
		r.Comments = comments.String
	}

	return r, nil
}

func scanReviewList(rows *sql.Rows) ([]*Review, error) {
	var list []*Review
	for rows.Next() {
		r := &Review{}
		var decision string
		var sigTs, created string
		var responsesJSON sql.NullString
		var comments sql.NullString

		if err := rows.Scan(&r.ID, &r.RequestID, &r.ReviewerSessionID, &r.ReviewerAgent, &r.ReviewerModel,
			&decision, &r.Signature, &sigTs, &responsesJSON, &comments, &created); err != nil {
			return nil, fmt.Errorf("scanning reviews: %w", err)
		}

		r.Decision = Decision(decision)
		r.SignatureTimestamp, _ = time.Parse(time.RFC3339, sigTs)
		r.CreatedAt, _ = time.Parse(time.RFC3339, created)
		if responsesJSON.Valid {
			_ = json.Unmarshal([]byte(responsesJSON.String), &r.Responses)
		}
		if comments.Valid {
			r.Comments = comments.String
		}

		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// ComputeReviewSignature computes an HMAC signature for a review.
// Signature = HMAC-SHA256(sessionKey, requestID + decision + timestamp)
func ComputeReviewSignature(sessionKey, requestID string, decision Decision, timestamp time.Time) string {
	data := requestID + string(decision) + timestamp.Format(time.RFC3339)
	key, _ := hex.DecodeString(sessionKey)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyReviewSignature verifies an HMAC signature for a review.
func VerifyReviewSignature(sessionKey, requestID string, decision Decision, timestamp time.Time, signature string) bool {
	expected := ComputeReviewSignature(sessionKey, requestID, decision, timestamp)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// HasDifferentModelApproval checks if there's an approval from a different model.
func (db *DB) HasDifferentModelApproval(requestID, excludeModel string) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM reviews
		WHERE request_id = ? AND decision = ? AND reviewer_model != ?
	`, requestID, string(DecisionApprove), excludeModel).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking different model approval: %w", err)
	}
	return count > 0, nil
}

// CheckRequestApprovalStatus checks if a request has met its approval requirements.
// Returns (approved, rejected, error).
func (db *DB) CheckRequestApprovalStatus(requestID string) (approved bool, rejected bool, err error) {
	req, err := db.GetRequest(requestID)
	if err != nil {
		return false, false, err
	}

	approvalCount, rejectionCount, err := db.CountReviewsByDecision(requestID)
	if err != nil {
		return false, false, err
	}

	if rejectionCount > 0 {
		return false, true, nil
	}

	if approvalCount >= req.MinApprovals {
		if req.RequireDifferentModel {
			hasDiffModel, err := db.HasDifferentModelApproval(requestID, req.RequestorModel)
			if err != nil {
				return false, false, err
			}
			return hasDiffModel, false, nil
		}
		return true, false, nil
	}

	return false, false, nil
}

// CreateReviewWithValidation verifies an externally signed review and commits
// its decision through the same atomic path used by the CLI review service.
// In particular, the supplied key must belong to the persisted reviewer session.
func (db *DB) CreateReviewWithValidation(r *Review, sessionKey string) error {
	if r == nil || r.Signature == "" || r.SignatureTimestamp.IsZero() {
		return ErrInvalidSignature
	}
	_, err := db.ApplyReview(r, sessionKey, ReviewPolicy{})
	return err
}
