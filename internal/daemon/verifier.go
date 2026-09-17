// Package daemon provides execution verification for the approval notary.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// VerificationResult describes either an advisory check or a committed claim.
// Only a committed claim has an ExecutionReceipt and may authorize execution.
type VerificationResult struct {
	Allowed                  bool        `json:"allowed"`
	Reason                   string      `json:"reason,omitempty"`
	Request                  *db.Request `json:"request,omitempty"`
	ApprovalRemainingSeconds int         `json:"approval_remaining_seconds"`
	ExecutionReceipt         string      `json:"execution_receipt,omitempty"`
}

// Verifier validates execution gates. It never starts a command on the daemon.
type Verifier struct {
	db *db.DB
}

func NewVerifier(database *db.DB) *Verifier { return &Verifier{db: database} }

// VerifyExecuteParams binds the caller to a session and the exact command it
// intends to execute. An ID-only request is not an execution authorization.
type VerifyExecuteParams struct {
	RequestID   string `json:"request_id"`
	SessionID   string `json:"session_id"`
	SessionKey  string `json:"session_key"`
	CommandHash string `json:"command_hash"`
}

func (p VerifyExecuteParams) validate() error {
	switch {
	case p.RequestID == "":
		return errors.New("request_id is required")
	case p.SessionID == "":
		return errors.New("session_id is required")
	case p.SessionKey == "":
		return errors.New("session_key is required")
	case p.CommandHash == "":
		return errors.New("command_hash is required")
	}
	return nil
}

// VerifyExecutionAllowed is advisory: it neither consumes approval nor grants
// permission to run a process. The claim repeats authentication and signed
// evidence verification inside the database's writer transaction.
func (v *Verifier) VerifyExecutionAllowed(p VerifyExecuteParams) (*VerificationResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	request, err := v.db.GetRequest(p.RequestID)
	if err != nil {
		return nil, fmt.Errorf("getting request: %w", err)
	}
	session, err := v.db.GetSession(p.SessionID)
	if err != nil || !session.IsActive() || session.ProjectPath != request.ProjectPath ||
		!db.ExecutionSessionKeyMatches(session.SessionKey, p.SessionKey) {
		return &VerificationResult{Reason: db.ErrExecutionAuthentication.Error()}, nil
	}

	// Use the shared evidence verifier, not a count of unsigned review rows.
	request, err = v.db.VerifyRequestApproval(p.RequestID)
	if err != nil {
		if errors.Is(err, db.ErrInvalidApprovalProof) {
			return &VerificationResult{Reason: err.Error()}, nil
		}
		return nil, fmt.Errorf("verifying approval: %w", err)
	}
	if request.ProjectPath != session.ProjectPath {
		return &VerificationResult{Reason: db.ErrExecutionAuthentication.Error()}, nil
	}
	if request.Command.Hash != p.CommandHash {
		return &VerificationResult{Reason: "command hash does not match the requested execution"}, nil
	}
	if err := v.checkExecutionPolicy(request); err != nil {
		return &VerificationResult{Reason: err.Error()}, nil
	}

	remaining := 0
	if request.ApprovalExpiresAt != nil {
		remaining = max(0, int(time.Until(*request.ApprovalExpiresAt).Seconds()))
	}
	return &VerificationResult{Allowed: true, Request: request, ApprovalRemainingSeconds: remaining}, nil
}

// checkExecutionPolicy uses the same effective configuration, persisted rules,
// project-local quorum and model requirement as the local executor. The final
// claim repeats the check under its writer reservation after advisory preflight.
func (v *Verifier) checkExecutionPolicy(request *db.Request) error {
	return core.CheckExecutionPolicy(v.db, request, "")
}

// VerifyAndMarkExecuting commits an authenticated, single-use execution claim.
// A lost reply is ambiguous: callers MUST NOT execute or retry the raw command
// without a successful response. No reset-to-approved path is exposed.
func (v *Verifier) VerifyAndMarkExecuting(p VerifyExecuteParams) (*VerificationResult, error) {
	result, err := v.VerifyExecutionAllowed(p)
	if err != nil || !result.Allowed {
		return result, err
	}
	session, err := v.db.GetSession(p.SessionID)
	if err != nil {
		return nil, fmt.Errorf("getting executor: %w", err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("creating execution receipt: %w", err)
	}
	// The database's log reference doubles as the execution fence. A notary
	// receipt is an opaque reference, NOT a filesystem path or a server log.
	receipt := "notary:" + hex.EncodeToString(nonce[:])
	now := time.Now().UTC()
	execution := &db.Execution{
		ExecutedAt: &now, ExecutedBySessionID: session.ID,
		ExecutedByAgent: session.AgentName, ExecutedByModel: session.Model,
		LogPath: receipt,
	}
	if err := v.db.ClaimRequestExecutionAuthenticated(result.Request, execution, p.SessionKey, core.ExecutionPolicyGuard("")); err != nil {
		if errors.Is(err, db.ErrExecutionAuthentication) || errors.Is(err, db.ErrInvalidTransition) ||
			errors.Is(err, core.ErrApprovalPolicyChanged) || errors.Is(err, core.ErrTierEscalated) {
			return &VerificationResult{Reason: err.Error()}, nil
		}
		return nil, fmt.Errorf("claiming execution: %w", err)
	}
	result.Request.Status = db.StatusExecuting
	result.Request.ResolvedAt = nil
	result.Request.Execution = execution
	result.ExecutionReceipt = receipt
	return result, nil
}

// CompleteExecuteParams reports one owned execution, including a failed start
// (nil exit_code). Completion is terminal and cannot reissue execution rights.
type CompleteExecuteParams struct {
	RequestID        string           `json:"request_id"`
	SessionID        string           `json:"session_id"`
	SessionKey       string           `json:"session_key"`
	ExecutionReceipt string           `json:"execution_receipt"`
	Status           db.RequestStatus `json:"status"`
	ExitCode         *int             `json:"exit_code"`
	DurationMs       *int64           `json:"duration_ms,omitempty"`
}

func (p CompleteExecuteParams) validate() error {
	if p.RequestID == "" || p.SessionID == "" || p.SessionKey == "" {
		return errors.New("request_id, session_id and session_key are required")
	}
	if !validNotaryReceipt(p.ExecutionReceipt) {
		return errors.New("a valid execution_receipt is required")
	}
	if p.Status != db.StatusExecuted && p.Status != db.StatusExecutionFailed && p.Status != db.StatusTimedOut {
		return errors.New("status must be executed, execution_failed or timed_out")
	}
	if p.Status == db.StatusExecuted && (p.ExitCode == nil || *p.ExitCode != 0) {
		return errors.New("executed requires exit_code zero")
	}
	if p.ExitCode != nil && *p.ExitCode < 0 {
		return errors.New("exit_code must be nonnegative, or null when unavailable")
	}
	if p.DurationMs != nil && *p.DurationMs < 0 {
		return errors.New("duration_ms must not be negative")
	}
	return nil
}

func validNotaryReceipt(receipt string) bool {
	if !strings.HasPrefix(receipt, "notary:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(receipt, "notary:"))
	return err == nil && len(decoded) == 32
}

// MarkExecutionComplete authenticates against the current key and fences the
// report with the receipt returned by the committed claim. Ended sessions may
// finish already-started work; rotated keys and stale receipts are rejected.
func (v *Verifier) MarkExecutionComplete(p CompleteExecuteParams) error {
	if err := p.validate(); err != nil {
		return err
	}
	return v.db.CompleteRequestExecutionAuthenticated(p.RequestID, p.Status, &db.Execution{
		ExecutedBySessionID: p.SessionID, LogPath: p.ExecutionReceipt,
		ExitCode: p.ExitCode, DurationMs: p.DurationMs,
	}, p.SessionKey)
}

// VerifyExecuteResponse supplies the complete reviewed command specification.
// Clients must preserve argv, cwd and shell mode, rather than reparse Command.
type VerifyExecuteResponse struct {
	Allowed                  bool            `json:"allowed"`
	Reason                   string          `json:"reason,omitempty"`
	ApprovalRemainingSeconds int             `json:"approval_remaining_seconds"`
	RequestID                string          `json:"request_id,omitempty"`
	Command                  string          `json:"command,omitempty"`
	CommandHash              string          `json:"command_hash,omitempty"`
	RiskTier                 string          `json:"risk_tier,omitempty"`
	CommandSpec              *db.CommandSpec `json:"command_spec,omitempty"`
	ExecutionReceipt         string          `json:"execution_receipt,omitempty"`
}

// ToIPCResponse never turns an advisory check into a raw-command permit.
func (r *VerificationResult) ToIPCResponse() *VerifyExecuteResponse {
	resp := &VerifyExecuteResponse{Reason: r.Reason}
	if !r.Allowed {
		return resp
	}
	if r.Request == nil || !validNotaryReceipt(r.ExecutionReceipt) {
		resp.Reason = "execution has not been claimed"
		return resp
	}
	resp.Allowed = true
	resp.ApprovalRemainingSeconds = r.ApprovalRemainingSeconds
	resp.RequestID = r.Request.ID
	resp.Command = r.Request.Command.Raw
	resp.CommandHash = r.Request.Command.Hash
	resp.RiskTier = string(r.Request.RiskTier)
	spec := r.Request.Command
	if spec.Argv != nil {
		spec.Argv = make([]string, len(r.Request.Command.Argv))
		copy(spec.Argv, r.Request.Command.Argv)
	}
	resp.CommandSpec = &spec
	resp.ExecutionReceipt = r.ExecutionReceipt
	return resp
}
