// Package core implements request creation logic for SLB.
package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	shellwords "github.com/mattn/go-shellwords"
)

// CreateRequestOptions holds the options for creating a new request.
type CreateRequestOptions struct {
	// SessionID is the requesting agent's session (required).
	SessionID string
	// Command is the raw command string to execute.
	Command string
	// Cwd is an absolute working directory. Empty captures the caller's
	// current directory at admission, before classification and hash binding.
	Cwd string
	// Shell indicates if the command should be run through a shell.
	Shell bool
	// Justification contains the reasoning for the request.
	Justification Justification
	// Attachments are optional context files.
	Attachments []db.Attachment
	// RedactPatterns are custom patterns to redact from display.
	RedactPatterns []string
	// ProjectPath defaults to the session's project. An explicit value must
	// match that exact scope; it cannot transfer a session to another project.
	ProjectPath string
	// RequireDryRun refuses admission when a preview cannot complete successfully.
	RequireDryRun bool
	// DryRunTimeout optionally shortens the default 30-second preview budget.
	DryRunTimeout time.Duration
}

// CreateRequestResult holds the result of creating a request.
type CreateRequestResult struct {
	// Request is the created request (nil if skipped).
	Request *Request
	// Skipped indicates the command was classified as safe.
	Skipped bool
	// SkipReason explains why the request was skipped.
	SkipReason string
	// Classification is the risk classification result.
	Classification *MatchResult
	// RateLimit is the committed admission result, including warn-only overruns.
	RateLimit *RateLimitResult
	// Queued means this call waited for capacity before it completed.
	Queued bool
	// QueueWait is elapsed admission time when Queued is true.
	QueueWait time.Duration
	// Preflight describes evidence persisted before the request became visible.
	Preflight *PreflightReport
}

// Request creation errors.
var (
	// ErrSessionRequired is returned when no session ID is provided.
	ErrSessionRequired = errors.New("session ID is required")
	// ErrCommandRequired is returned when no command is provided.
	ErrCommandRequired = errors.New("command is required")
	// ErrSessionNotFound is returned when the session doesn't exist.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionInactive is returned when the session has ended.
	ErrSessionInactive = errors.New("session is no longer active")
	// ErrAgentBlocked is returned when the agent is blocked from creating requests.
	ErrAgentBlocked = errors.New("agent is blocked from creating requests")
)

// RequestCreator handles request creation with validation.
type RequestCreator struct {
	db            *db.DB
	rateLimiter   *RateLimiter
	patternEngine *PatternEngine
	config        *RequestCreatorConfig
	notifier      integrations.RequestNotifier
}

// RequestCreatorConfig holds configuration for request creation.
type RequestCreatorConfig struct {
	// ApprovalPolicy is fixed on each new request; changing configuration
	// cannot reinterpret existing review evidence under another policy.
	ApprovalPolicy db.ApprovalPolicy
	// EnableDryRun collects advisory preview evidence before admitting requests.
	EnableDryRun bool
	// BlockedAgents is a list of agent names that cannot create requests.
	BlockedAgents []string
	// DynamicQuorumEnabled enables dynamic quorum adjustment.
	DynamicQuorumEnabled bool
	// DynamicQuorumFloor is the minimum approvals even with dynamic quorum.
	DynamicQuorumFloor int
	// RequestTimeout is the lifetime of a pending request. Keep the configured
	// precision: rounding seconds to minutes can delay expiry and stall queues.
	RequestTimeout time.Duration
	// ApprovalTTLMinutes is the default TTL for approvals (dangerous tier).
	ApprovalTTLMinutes int
	// ApprovalTTLCriticalMinutes is the TTL for critical tier approvals.
	ApprovalTTLCriticalMinutes int
	// AgentMailEnabled toggles Agent Mail notifications.
	AgentMailEnabled bool
	// RequestJournalEnabled leaves notification delivery to the durable journal
	// worker, suppressing legacy synchronous Agent Mail sends.
	RequestJournalEnabled bool
	// AgentMailThread is the thread to post notifications.
	AgentMailThread string
	// AgentMailSender optional sender name.
	AgentMailSender string
}

// DefaultRequestCreatorConfig returns the default configuration.
func DefaultRequestCreatorConfig() *RequestCreatorConfig {
	return &RequestCreatorConfig{
		ApprovalPolicy:             db.ApprovalPolicyIndependent,
		EnableDryRun:               true,
		BlockedAgents:              []string{},
		DynamicQuorumEnabled:       false,
		DynamicQuorumFloor:         1,
		RequestTimeout:             30 * time.Minute,
		ApprovalTTLMinutes:         30,
		ApprovalTTLCriticalMinutes: 10,
		AgentMailEnabled:           true,
		AgentMailThread:            "SLB-Reviews",
		AgentMailSender:            "SLB-System",
	}
}

// NewRequestCreator creates a new request creator.
func NewRequestCreator(database *db.DB, rateLimiter *RateLimiter, patternEngine *PatternEngine, config *RequestCreatorConfig) *RequestCreator {
	if config == nil {
		config = DefaultRequestCreatorConfig()
	}
	if rateLimiter == nil {
		rateLimiter = NewRateLimiter(database, DefaultRateLimitConfig())
	}
	if patternEngine == nil {
		patternEngine = GetDefaultEngine()
	}
	return &RequestCreator{
		db:            database,
		rateLimiter:   rateLimiter,
		patternEngine: patternEngine,
		config:        config,
		notifier:      integrations.NoopNotifier{},
	}
}

// createRequestOnce performs one fresh validation and atomic admission attempt.
func (rc *RequestCreator) createRequestOnce(ctx context.Context, opts CreateRequestOptions) (*CreateRequestResult, error) {
	// Validate required fields
	if opts.SessionID == "" {
		return nil, ErrSessionRequired
	}
	if opts.Command == "" {
		return nil, ErrCommandRequired
	}
	if opts.DryRunTimeout < 0 {
		return nil, errors.New("dry-run timeout must not be negative")
	}
	if opts.RequireDryRun && !rc.config.EnableDryRun {
		return nil, fmt.Errorf("%w: dry runs are disabled by configuration", ErrPreflightRequired)
	}

	// Step 1: Validate session exists and is active
	session, err := rc.db.GetSession(opts.SessionID)
	if err != nil {
		if errors.Is(err, db.ErrSessionNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("getting session: %w", err)
	}
	if session.EndedAt != nil {
		return nil, ErrSessionInactive
	}

	// Validate scope before classification (including the SAFE shortcut),
	// reviewer discovery, or external preflight tools. The database repeats
	// this identity check at admission, but that is too late to prevent a
	// rejected cross-project request from running its preview first.
	projectPath := opts.ProjectPath
	if projectPath == "" {
		projectPath = session.ProjectPath
	}
	if projectPath == "" || projectPath != session.ProjectPath || session.AgentName == "" {
		return nil, db.ErrRequestAdmissionIdentity
	}

	// Initialize notifier with project context if enabled.
	notifier := rc.notifier
	if rc.config != nil && rc.config.AgentMailEnabled {
		notifier = integrations.NewAgentMailClient(session.ProjectPath, rc.config.AgentMailThread, rc.config.AgentMailSender, rc.config.RequestJournalEnabled)
	}

	// Step 2: Check agent not blocked
	if rc.isAgentBlocked(session.AgentName) {
		return nil, fmt.Errorf("%w: %s", ErrAgentBlocked, session.AgentName)
	}

	// Quota is checked atomically with persistence below, not with a separate
	// precheck that concurrent clients could all pass. SAFE skips consume none.

	// Build the executable specification before classification or a SAFE
	// shortcut. A direct request must not silently execute only the prefix
	// that the tokenizer could parse, or fall back to different argv on error.
	argv, err := parseRequestArgv(opts.Command, opts.Shell)
	if err != nil {
		return nil, fmt.Errorf("parsing requested command: %w", err)
	}
	cwd, err := bindCommandCWD(opts.Cwd)
	if err != nil {
		return nil, err
	}
	cmdSpec := db.CommandSpec{
		Raw: opts.Command, Argv: argv, Cwd: cwd, Shell: opts.Shell,
	}

	// Step 4: Classify the same invocation used by preflight and execution.
	classification := rc.patternEngine.ClassifyCommandSpec(cmdSpec)

	// Step 5: Skip only when the SAFE verdict covers the entire command.
	// An unknown segment cannot borrow another segment's allowlist entry.
	if classification.IsSafe && !classification.HasUnmatchedSegment {
		return &CreateRequestResult{
			Request:        nil,
			Skipped:        true,
			SkipReason:     "Command is classified as safe and does not require approval",
			Classification: classification,
		}, nil
	}

	// Expired requests still occupy a pending slot until their policy makes a
	// durable decision. Resolve this requester's expired work even without a
	// daemon; the atomic admission below still decides who gets the free slot.
	if err := rc.advanceExpiredCapacity(ctx, session.ID, projectPath); err != nil {
		return nil, err
	}

	// Commands deliberately submitted to SLB default to dangerous when the
	// normalizer cannot recognize the operation (GH #9). Use the configured
	// dangerous quorum here too; opaque wrappers must not get a weaker quorum.
	// Opaque execution (a computed command word, or a program read from an
	// unknown feed) is at least CAUTION for hooks; an explicit request keeps
	// the unmatched-command escalation so it never gets a weaker quorum.
	if !classification.NeedsApproval || (classification.Opaque && classification.Tier == RiskTierCaution &&
		classification.MatchedPattern == "unresolved_execution") {
		classification.Tier = RiskTierDangerous
		classification.MinApprovals = rc.patternEngine.RequiredApprovals(RiskTierDangerous, -1)
		classification.NeedsApproval = true
		classification.IsSafe = false
		if classification.MatchedPattern == "" || classification.HasUnmatchedSegment {
			classification.MatchedPattern = "unmatched_default_escalation"
		}
	}

	// Step 8: Apply redaction
	cmdSpec.DisplayRedacted = ApplyRedaction(opts.Command, opts.RedactPatterns)
	cmdSpec.ContainsSensitive = cmdSpec.DisplayRedacted != opts.Command

	// Scope was validated before any classification or preflight. Distinct
	// active agents in that project exclude the requester identity.
	available, err := CountPolicyReviewers(rc.db, projectPath, session.ID, session.AgentName)
	if err != nil {
		return nil, fmt.Errorf("counting available reviewers: %w", err)
	}
	minApprovals := rc.patternEngine.RequiredApprovals(classification.Tier, available)
	if rc.config.DynamicQuorumEnabled {
		// Explicit programmatic overrides still cannot lower the configured
		// policy below what the execution gate will accept.
		minApprovals = max(minApprovals, rc.checkDynamicQuorum(classification.Tier, minApprovals, projectPath))
	}

	// Step 10: Set expiry times
	now := time.Now().UTC()
	requestExpiry := now.Add(rc.requestLifetime())

	// Step 11: Create request in DB
	request := &db.Request{
		ApprovalPolicy:        rc.config.ApprovalPolicy.Normalized(),
		ProjectPath:           projectPath,
		Command:               cmdSpec,
		RiskTier:              classification.Tier,
		RequestorSessionID:    opts.SessionID,
		RequestorAgent:        session.AgentName,
		RequestorModel:        session.Model,
		Justification:         opts.Justification,
		Attachments:           opts.Attachments,
		Status:                db.StatusPending,
		MinApprovals:          minApprovals,
		RequireDifferentModel: rc.patternEngine.RequiresDifferentModel(),
		ExpiresAt:             &requestExpiry,
	}

	preflight, err := rc.prepareRequestPreflight(ctx, request, opts)
	if err != nil {
		return nil, err
	}
	// Start the review lifetime after preflight, not before a slow tool runs.
	requestExpiry = time.Now().UTC().Add(rc.requestLifetime())
	admission, err := rc.rateLimiter.AdmitRequest(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("admitting request: %w", err)
	}

	// Step 12: Notify via Agent Mail (best effort; errors ignored)
	_ = notifier.NotifyNewRequest(request)

	// Step 12: (TODO) Materialize JSON file in .slb/pending/
	// This will be implemented when file materialization is needed

	return &CreateRequestResult{
		Request:        request,
		Skipped:        false,
		Classification: classification,
		RateLimit:      admission,
		Preflight:      preflight,
	}, nil
}

// isAgentBlocked checks if an agent is in the blocked list.
func (rc *RequestCreator) isAgentBlocked(agentName string) bool {
	for _, blocked := range rc.config.BlockedAgents {
		if strings.EqualFold(blocked, agentName) {
			return true
		}
	}
	return false
}

// checkDynamicQuorum adjusts min approvals based on active sessions.
func (rc *RequestCreator) checkDynamicQuorum(tier RiskTier, minApprovals int, projectPath string) int {
	sessions, err := rc.db.ListActiveSessions(projectPath)
	if err != nil {
		return minApprovals
	}
	activeSessions := len(sessions)
	if activeSessions == 0 {
		return minApprovals
	}
	availableReviewers := activeSessions - 1
	if availableReviewers < minApprovals {
		adjusted := availableReviewers
		if adjusted < rc.config.DynamicQuorumFloor {
			adjusted = rc.config.DynamicQuorumFloor
		}
		return adjusted
	}
	return minApprovals
}

// ParseCommandToArgv parses one complete direct invocation without expanding
// environment variables or running substitutions. Shell syntax that stops the
// tokenizer is an error, never permission to run a truncated prefix.
func ParseCommandToArgv(cmd string) ([]string, error) {
	if strings.IndexByte(cmd, 0) >= 0 {
		return nil, errors.New("command contains a NUL byte")
	}
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	argv, err := parser.Parse(cmd)
	if err != nil {
		return nil, err
	}
	if parser.Position >= 0 {
		return nil, errors.New("shell operators require shell mode")
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, ErrCommandRequired
	}
	return argv, nil
}

// Shell requests execute Raw, not Argv. Retain a parsed argv only when it
// represents the complete script; leave it empty otherwise rather than
// recording a misleading prefix as executable input. Malformed shell scripts
// still go through the normalizer's conservative parse-error classification.
func parseRequestArgv(raw string, shell bool) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, ErrCommandRequired
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return nil, errors.New("command contains a NUL byte")
	}
	argv, err := ParseCommandToArgv(raw)
	if shell && err != nil {
		return nil, nil
	}
	return argv, err
}

// Default redaction patterns for sensitive data.
var defaultRedactionPatterns = []string{
	`(?i)(api[_-]?key|apikey|token|secret|password|passwd|pwd)\s*[=:]\s*['"]?[^\s'"]+['"]?`,
	`(?i)aws[_-]?(access[_-]?key|secret[_-]?key|session[_-]?token)\s*[=:]\s*['"]?[^\s'"]+['"]?`,
	`(?i)export\s+(API_KEY|SECRET|TOKEN|PASSWORD|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|DATABASE_URL)\s*=\s*['"]?[^\s'"]+['"]?`,
	`(?i)(postgres|mysql|mongodb|redis)://[^@\s]+@`,
	`(?i)bearer\s+[a-zA-Z0-9._-]+`,
	`(?i)-----BEGIN\s+[A-Z]+\s+PRIVATE\s+KEY-----`,
}

// ApplyRedaction applies redaction patterns to a command string.
// Returns a display-safe version of the command with sensitive data masked.
func ApplyRedaction(cmd string, customPatterns []string) string {
	result := cmd
	for _, pattern := range defaultRedactionPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		result = re.ReplaceAllString(result, "[REDACTED]")
	}
	for _, pattern := range customPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		result = re.ReplaceAllString(result, "[REDACTED]")
	}
	return result
}

// DetectSensitiveContent checks if a command contains sensitive data.
func DetectSensitiveContent(cmd string) bool {
	for _, pattern := range defaultRedactionPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}
