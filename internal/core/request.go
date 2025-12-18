// Package core implements request creation logic for SLB.
package core

import (
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
	// Cwd is the working directory for the command.
	Cwd string
	// Shell indicates if the command should be run through a shell.
	Shell bool
	// Justification contains the reasoning for the request.
	Justification Justification
	// Attachments are optional context files.
	Attachments []db.Attachment
	// RedactPatterns are custom patterns to redact from display.
	RedactPatterns []string
	// ProjectPath overrides the project path (defaults to session's project).
	ProjectPath string
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
	// BlockedAgents is a list of agent names that cannot create requests.
	BlockedAgents []string
	// DynamicQuorumEnabled enables dynamic quorum adjustment.
	DynamicQuorumEnabled bool
	// DynamicQuorumFloor is the minimum approvals even with dynamic quorum.
	DynamicQuorumFloor int
	// RequestTimeoutMinutes is the default timeout for pending requests.
	RequestTimeoutMinutes int
	// ApprovalTTLMinutes is the default TTL for approvals (dangerous tier).
	ApprovalTTLMinutes int
	// ApprovalTTLCriticalMinutes is the TTL for critical tier approvals.
	ApprovalTTLCriticalMinutes int
	// AgentMailEnabled toggles Agent Mail notifications.
	AgentMailEnabled bool
	// AgentMailThread is the thread to post notifications to.
	AgentMailThread string
	// AgentMailSender optional sender name.
	AgentMailSender string
}

// DefaultRequestCreatorConfig returns the default configuration.
func DefaultRequestCreatorConfig() *RequestCreatorConfig {
	return &RequestCreatorConfig{
		BlockedAgents:              []string{},
		DynamicQuorumEnabled:       false,
		DynamicQuorumFloor:         1,
		RequestTimeoutMinutes:      30,
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

// CreateRequest creates a new command approval request with full validation.
func (rc *RequestCreator) CreateRequest(opts CreateRequestOptions) (*CreateRequestResult, error) {
	// Validate required fields
	if opts.SessionID == "" {
		return nil, ErrSessionRequired
	}
	if opts.Command == "" {
		return nil, ErrCommandRequired
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

	// Initialize notifier with project context if enabled.
	notifier := rc.notifier
	if rc.config != nil && rc.config.AgentMailEnabled {
		notifier = integrations.NewAgentMailClient(session.ProjectPath, rc.config.AgentMailThread, rc.config.AgentMailSender)
	}

	// Step 2: Check agent not blocked
	if rc.isAgentBlocked(session.AgentName) {
		return nil, fmt.Errorf("%w: %s", ErrAgentBlocked, session.AgentName)
	}

	// Step 3: Check rate limits
	// CheckRateLimit returns an error when Action=reject and limits are exceeded
	limitResult, err := rc.rateLimiter.CheckRateLimit(opts.SessionID)
	if err != nil {
		return nil, err
	}
	if !limitResult.Allowed {
		// Enforce block for actions that return Allowed=false (like queue, if not handled)
		return nil, fmt.Errorf("rate limit exceeded (action=%s): %s", limitResult.Action, limitResult.Message)
	}

	// Step 4: Classify command
	classification := rc.patternEngine.ClassifyCommand(opts.Command, opts.Cwd)

	// Step 5: If SAFE, skip
	if classification.IsSafe {
		return &CreateRequestResult{
			Request:        nil,
			Skipped:        true,
			SkipReason:     "Command is classified as safe and does not require approval",
			Classification: classification,
		}, nil
	}

	// If no approval needed (no pattern match), also skip
	if !classification.NeedsApproval {
		return &CreateRequestResult{
			Request:        nil,
			Skipped:        true,
			SkipReason:     "Command does not match any dangerous patterns",
			Classification: classification,
		}, nil
	}

	// Step 6: Parse command to argv
	argv, _ := ParseCommandToArgv(opts.Command)

	// Step 7: Build command spec (hash computed by db.CreateRequest)
	cmdSpec := db.CommandSpec{
		Raw:   opts.Command,
		Argv:  argv,
		Cwd:   opts.Cwd,
		Shell: opts.Shell,
	}

	// Step 8: Apply redaction
	cmdSpec.DisplayRedacted = ApplyRedaction(opts.Command, opts.RedactPatterns)
	cmdSpec.ContainsSensitive = cmdSpec.DisplayRedacted != opts.Command

	// Step 9: Get min approvals (with dynamic quorum check)
	minApprovals := classification.MinApprovals
	if rc.config.DynamicQuorumEnabled {
		minApprovals = rc.checkDynamicQuorum(classification.Tier, minApprovals, opts.ProjectPath)
	}

	// Step 10: Set expiry times
	now := time.Now().UTC()
	requestExpiry := now.Add(time.Duration(rc.config.RequestTimeoutMinutes) * time.Minute)

	// Determine project path
	projectPath := opts.ProjectPath
	if projectPath == "" {
		projectPath = session.ProjectPath
	}

	// Step 11: Create request in DB
	request := &db.Request{
		ProjectPath:        projectPath,
		Command:            cmdSpec,
		RiskTier:           classification.Tier,
		RequestorSessionID: opts.SessionID,
		RequestorAgent:     session.AgentName,
		RequestorModel:     session.Model,
		Justification:      opts.Justification,
		Attachments:        opts.Attachments,
		Status:             db.StatusPending,
		MinApprovals:       minApprovals,
		ExpiresAt:          &requestExpiry,
	}

	// Set require_different_model based on tier
	if classification.Tier == RiskTierCritical {
		request.RequireDifferentModel = true
	}

	if err := rc.db.CreateRequest(request); err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Step 12: Notify via Agent Mail (best effort; errors ignored)
	_ = notifier.NotifyNewRequest(request)

	// Step 12: (TODO) Materialize JSON file in .slb/pending/
	// This will be implemented when file materialization is needed

	return &CreateRequestResult{
		Request:        request,
		Skipped:        false,
		Classification: classification,
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
	// Count active sessions in the project
	sessions, err := rc.db.ListActiveSessions(projectPath)
	if err != nil {
		// On error, use default min approvals
		return minApprovals
	}

	activeSessions := len(sessions)
	if activeSessions == 0 {
		return minApprovals
	}

	// Dynamic quorum: at most (active_sessions - 1), but never below floor
	availableReviewers := activeSessions - 1 // Exclude requestor
	if availableReviewers < minApprovals {
		adjusted := availableReviewers
		if adjusted < rc.config.DynamicQuorumFloor {
			adjusted = rc.config.DynamicQuorumFloor
		}
		return adjusted
	}

	return minApprovals
}

// ParseCommandToArgv parses a command string into argv.
func ParseCommandToArgv(cmd string) ([]string, error) {
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	return parser.Parse(cmd)
}

// Default redaction patterns for sensitive data.
var defaultRedactionPatterns = []string{
	// API keys and tokens
	`(?i)(api[_-]?key|apikey|token|secret|password|passwd|pwd)\s*[=:]\s*['"]?[^\s'"]+['"]?`,
	// AWS credentials
	`(?i)aws[_-]?(access[_-]?key|secret[_-]?key|session[_-]?token)\s*[=:]\s*['"]?[^\s'"]+['"]?`,
	// Environment variable exports with sensitive names
	`(?i)export\s+(API_KEY|SECRET|TOKEN|PASSWORD|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|DATABASE_URL)\s*=\s*['"]?[^\s'"]+['"]?`,
	// Connection strings
	`(?i)(postgres|mysql|mongodb|redis)://[^@\s]+@`,
	// Bearer tokens
	`(?i)bearer\s+[a-zA-Z0-9._-]+`,
	// Private keys (just the header)
	`(?i)-----BEGIN\s+[A-Z]+\s+PRIVATE\s+KEY-----`,
}

// ApplyRedaction applies redaction patterns to a command string.
// Returns a display-safe version of the command with sensitive data masked.
func ApplyRedaction(cmd string, customPatterns []string) string {
	result := cmd

	// Apply default patterns
	for _, pattern := range defaultRedactionPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		result = re.ReplaceAllString(result, "[REDACTED]")
	}

	// Apply custom patterns
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
