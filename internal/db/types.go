// Package db provides database types and operations for SLB.
package db

import (
	"encoding/json"
	"time"
)

// Session represents an agent session with the SLB daemon.
type Session struct {
	// ID is the unique session identifier (UUID).
	ID string `json:"id"`
	// AgentName is the agent's identifier (e.g., "GreenLake").
	AgentName string `json:"agent_name"`
	// Program is the agent program (e.g., "claude-code", "codex-cli").
	Program string `json:"program"`
	// Model is the underlying model (e.g., "opus-4.5", "gpt-5.1-codex").
	Model string `json:"model"`
	// ProjectPath is the absolute path to the project.
	ProjectPath string `json:"project_path"`
	// SessionKey is the session's bearer credential. It is never stored
	// (only its digest is) and never serialized: CreateSession sets it for
	// the caller to hand out once, and AuthenticateSession echoes the key
	// that was just verified. Sessions read from the database leave it empty.
	SessionKey string `json:"-"`
	// StartedAt is when the session was started.
	StartedAt time.Time `json:"started_at"`
	// LastActiveAt is when the session was last active.
	LastActiveAt time.Time `json:"last_active_at"`
	// EndedAt is when the session ended (nil if still active).
	EndedAt *time.Time `json:"ended_at,omitempty"`
}

// IsActive returns true if the session is still active.
func (s *Session) IsActive() bool {
	return s.EndedAt == nil
}

// CommandSpec represents the command to be executed.
type CommandSpec struct {
	// Raw is exactly what the agent requested.
	Raw string `json:"raw"`
	// Argv is the parsed command (preferred for execution).
	Argv []string `json:"argv,omitempty"`
	// Cwd is the working directory at request time.
	Cwd string `json:"cwd"`
	// Shell indicates if shell parsing/execution is required.
	Shell bool `json:"shell"`
	// Hash is sha256(raw + "\n" + cwd + "\n" + argv_json + "\n" + shell).
	Hash string `json:"hash"`
	// DisplayRedacted is the redacted version for display (if contains sensitive data).
	DisplayRedacted string `json:"display_redacted,omitempty"`
	// ContainsSensitive indicates if the command contains sensitive data.
	ContainsSensitive bool `json:"contains_sensitive"`
}

// Justification provides the reasoning for a command request.
type Justification struct {
	// Reason explains why this command should be run (required).
	Reason string `json:"reason"`
	// ExpectedEffect describes what will happen (optional).
	ExpectedEffect string `json:"expected_effect,omitempty"`
	// Goal describes what we're trying to achieve (optional).
	Goal string `json:"goal,omitempty"`
	// SafetyArgument explains why this is safe/reversible (optional).
	SafetyArgument string `json:"safety_argument,omitempty"`
}

// Attachment represents additional context attached to a request.
type Attachment struct {
	// Type is the attachment type (file, git_diff, context, screenshot).
	Type AttachmentType `json:"type"`
	// Content is the attachment content.
	Content string `json:"content"`
	// Metadata contains additional metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// DryRunResult contains the results of a dry run.
type DryRunResult struct {
	// Command is the dry run command that was executed.
	Command string `json:"command"`
	// Output is the output from the dry run.
	Output string `json:"output"`
}

// Execution contains information about command execution.
type Execution struct {
	// ApprovalPolicy and Assurance are copied from verified evidence when the
	// execution is claimed; a later configuration change cannot relabel it.
	ApprovalPolicy ApprovalPolicy  `json:"approval_policy"`
	Assurance      ReviewAssurance `json:"assurance"`
	// ExecutedAt is when the command was executed.
	ExecutedAt *time.Time `json:"executed_at,omitempty"`
	// ExecutedBySessionID is the session that executed the command.
	ExecutedBySessionID string `json:"executed_by_session_id,omitempty"`
	// ExecutedByAgent is the agent that executed the command.
	ExecutedByAgent string `json:"executed_by_agent,omitempty"`
	// ExecutedByModel is the model that executed the command.
	ExecutedByModel string `json:"executed_by_model,omitempty"`
	// LogPath is the path to the execution log.
	LogPath string `json:"log_path,omitempty"`
	// ExitCode is the command's exit code.
	ExitCode *int `json:"exit_code,omitempty"`
	// DurationMs is the execution duration in milliseconds.
	DurationMs *int64 `json:"duration_ms,omitempty"`
}

// Rollback contains information about rollback state.
type Rollback struct {
	// Path is the path to the captured state.
	Path string `json:"path,omitempty"`
	// RolledBackAt is when the rollback was performed.
	RolledBackAt *time.Time `json:"rolled_back_at,omitempty"`
}

// Request represents a command request submitted for approval.
type Request struct {
	// ID is the unique request identifier (UUID).
	ID string `json:"id"`
	// ProjectPath is the absolute path to the project.
	ProjectPath string `json:"project_path"`
	// Command is the command specification.
	Command CommandSpec `json:"command"`
	// RiskTier is the risk classification.
	RiskTier RiskTier `json:"risk_tier"`

	// Requestor is the session ID that submitted the request.
	RequestorSessionID string `json:"requestor_session_id"`
	// RequestorAgent is the agent that submitted the request.
	RequestorAgent string `json:"requestor_agent"`
	// RequestorModel is the model that submitted the request.
	RequestorModel string `json:"requestor_model"`

	// Justification is the reasoning for the request.
	Justification Justification `json:"justification"`

	// DryRun contains the dry run results if applicable.
	DryRun *DryRunResult `json:"dry_run,omitempty"`

	// Attachments contains additional context.
	Attachments []Attachment `json:"attachments,omitempty"`

	// Status is the current request status.
	Status RequestStatus `json:"status"`
	// MinApprovals is the minimum approvals required.
	MinApprovals int `json:"min_approvals"`
	// RequireDifferentModel requires a different model for approval.
	RequireDifferentModel bool `json:"require_different_model"`
	// ApprovalPolicy is chosen when the request is created. Existing requests
	// are never upgraded or downgraded by changing the project's configuration.
	ApprovalPolicy ApprovalPolicy `json:"approval_policy"`

	// Execution contains execution information.
	Execution *Execution `json:"execution,omitempty"`
	// Rollback contains rollback information.
	Rollback *Rollback `json:"rollback,omitempty"`

	// CreatedAt is when the request was created.
	CreatedAt time.Time `json:"created_at"`
	// ResolvedAt is when the request was approved/rejected/etc.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// ExpiresAt is the auto-timeout deadline for pending.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// ApprovalExpiresAt is when approval becomes stale.
	ApprovalExpiresAt *time.Time `json:"approval_expires_at,omitempty"`
}

// IsExpired returns true if the request has expired.
func (r *Request) IsExpired() bool {
	if r.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*r.ExpiresAt)
}

// ApprovalCount returns the number of approvals for this request.
// This requires the reviews to be loaded separately.
func (r *Request) ApprovalCount(reviews []Review) int {
	count := 0
	for _, rev := range reviews {
		if rev.RequestID == r.ID && rev.Decision == DecisionApprove {
			count++
		}
	}
	return count
}

// ReviewResponse contains the reviewer's response to justification fields.
type ReviewResponse struct {
	// ReasonResponse is the response to the reason field.
	ReasonResponse string `json:"reason_response,omitempty"`
	// EffectResponse is the response to the expected_effect field.
	EffectResponse string `json:"effect_response,omitempty"`
	// GoalResponse is the response to the goal field.
	GoalResponse string `json:"goal_response,omitempty"`
	// SafetyResponse is the response to the safety_argument field.
	SafetyResponse string `json:"safety_response,omitempty"`
}

// Review represents an approval or rejection of a request.
type Review struct {
	// ID is the unique review identifier (UUID).
	ID string `json:"id"`
	// RequestID is the request being reviewed.
	RequestID string `json:"request_id"`

	// ReviewerSessionID is the session that submitted the review.
	ReviewerSessionID string `json:"reviewer_session_id"`
	// ReviewerAgent is the agent that submitted the review.
	ReviewerAgent string `json:"reviewer_agent"`
	// ReviewerModel is the model that submitted the review.
	ReviewerModel string `json:"reviewer_model"`

	// Decision is approve or reject.
	Decision Decision `json:"decision"`
	// ApprovalPolicy and Assurance distinguish same-host workflow evidence
	// from independent authorization and from historical unauthenticated rows.
	ApprovalPolicy ApprovalPolicy  `json:"approval_policy"`
	Assurance      ReviewAssurance `json:"assurance"`
	// CommandHash is the exact command binding for version 2 reviews.
	CommandHash string `json:"command_hash"`
	// RequestBinding captures the immutable request fields reviewed. It is
	// signed and recomputed against the request before execution.
	RequestBinding string `json:"request_binding"`
	// SignatureVersion 1 is the historical local signature. Version 2 binds
	// policy, assurance, command, requester, quorum, model rule and reviewer.
	SignatureVersion int `json:"signature_version"`
	// Signature is an HMAC using the stored session key digest, not proof of
	// an independently authenticated reviewer identity.
	Signature string `json:"signature"`
	// SignatureTimestamp is included in the signature to prevent replay.
	SignatureTimestamp time.Time `json:"signature_timestamp"`

	// Responses contains structured responses to justification.
	Responses ReviewResponse `json:"responses,omitempty"`
	// Comments contains additional comments.
	Comments string `json:"comments,omitempty"`

	// CreatedAt is when the review was created.
	CreatedAt time.Time `json:"created_at"`
}

// RequestJSON is the JSON serialization format for requests.
// Used for file-based materialized views in .slb/pending/ and .slb/processed/.
type RequestJSON struct {
	Request
	// Reviews contains all reviews for this request.
	Reviews []Review `json:"reviews,omitempty"`
}

// MarshalJSON implements custom JSON marshaling for time fields.
// Ensures consistent ISO 8601 format.
func (r *Request) MarshalJSON() ([]byte, error) {
	type Alias Request
	return json.Marshal(&struct {
		*Alias
		CreatedAt         string  `json:"created_at"`
		ResolvedAt        *string `json:"resolved_at,omitempty"`
		ExpiresAt         *string `json:"expires_at,omitempty"`
		ApprovalExpiresAt *string `json:"approval_expires_at,omitempty"`
	}{
		Alias:             (*Alias)(r),
		CreatedAt:         r.CreatedAt.Format(time.RFC3339),
		ResolvedAt:        formatTimePtr(r.ResolvedAt),
		ExpiresAt:         formatTimePtr(r.ExpiresAt),
		ApprovalExpiresAt: formatTimePtr(r.ApprovalExpiresAt),
	})
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
