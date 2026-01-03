// Package daemon provides hook query handling for Claude Code integration.
package daemon

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// HookQueryParams are parameters for the hook_query method.
type HookQueryParams struct {
	Command   string `json:"command"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
}

// HookQueryResult is the result of a hook query.
type HookQueryResult struct {
	Action         string `json:"action"`          // "allow", "block", "ask"
	Message        string `json:"message"`         // Human-readable message
	Tier           string `json:"tier"`            // Risk tier
	MatchedPattern string `json:"matched_pattern"` // Pattern that matched
	MinApprovals   int    `json:"min_approvals"`   // Required approvals
	RequestID      string `json:"request_id,omitempty"` // If pending approval exists
}

// handleHookQuery processes a hook query request.
func (s *IPCServer) handleHookQuery(req RPCRequest) *RPCResponse {
	var params HookQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()},
			ID:    req.ID,
		}
	}

	if params.Command == "" {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "command is required"},
			ID:    req.ID,
		}
	}

	result := s.classifyCommand(params)

	return &RPCResponse{
		Result: result,
		ID:     req.ID,
	}
}

// classifyCommand classifies a command and checks for existing approvals.
func (s *IPCServer) classifyCommand(params HookQueryParams) *HookQueryResult {
	// Classify the command
	classification := core.Classify(params.Command, params.CWD)

	result := &HookQueryResult{
		Tier:           string(classification.Tier),
		MatchedPattern: classification.MatchedPattern,
		MinApprovals:   classification.MinApprovals,
	}

	// Determine action based on classification
	switch {
	case classification.IsSafe:
		result.Action = "allow"
		result.Message = "Safe command"
		return result

	case classification.Tier == core.RiskTierCritical:
		result.Action = "block"
		result.Message = "CRITICAL: Requires " + itoa(classification.MinApprovals) + " approvals"

	case classification.Tier == core.RiskTierDangerous:
		result.Action = "block"
		result.Message = "DANGEROUS: Requires approval"

	case classification.Tier == core.RiskTierCaution:
		result.Action = "ask"
		result.Message = "CAUTION: Proceed with care"

	default:
		// No matching pattern - allow by default
		result.Action = "allow"
		result.Message = "No matching pattern"
		return result
	}

	// Check for existing approval in database
	if params.SessionID != "" && classification.NeedsApproval {
		if approved, requestID := s.checkApproval(params.Command, params.SessionID, params.CWD); approved {
			result.Action = "allow"
			result.Message = "Pre-approved"
			result.RequestID = requestID
		}
	}

	return result
}

// checkApproval checks if a command has been pre-approved in the database.
func (s *IPCServer) checkApproval(command, sessionID, cwd string) (bool, string) {
	// Determine database path
	dbPath := filepath.Join(cwd, ".slb", "state.db")
	if cwd == "" {
		return false, ""
	}

	// Open database read-only
	opts := db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	}
	dbConn, err := db.OpenWithOptions(dbPath, opts)
	if err != nil {
		return false, ""
	}
	defer dbConn.Close()

	// Query for approved request matching this command by display_redacted field
	// We search for commands that match the display text (what the user sees)
	var requestID string
	var status string
	err = dbConn.QueryRow(`
		SELECT id, status FROM requests
		WHERE display_redacted = ?
		  AND session_id = ?
		  AND status IN ('approved', 'executed')
		  AND created_at > datetime('now', '-1 hour')
		ORDER BY created_at DESC
		LIMIT 1
	`, command, sessionID).Scan(&requestID, &status)

	if err != nil {
		return false, ""
	}

	return true, requestID
}

// HookHealthResult is the result of a hook health check.
type HookHealthResult struct {
	Status        string `json:"status"`
	Uptime        int64  `json:"uptime_seconds"`
	PatternHash   string `json:"pattern_hash"`
	PatternCount  int    `json:"pattern_count"`
	ServerTime    string `json:"server_time"`
}

// handleHookHealth responds to hook health checks.
func (s *IPCServer) handleHookHealth(req RPCRequest) *RPCResponse {
	engine := core.GetDefaultEngine()
	export := engine.Export()

	result := HookHealthResult{
		Status:       "ok",
		Uptime:       int64(time.Since(s.startTime).Seconds()),
		PatternHash:  export.SHA256,
		PatternCount: export.Metadata.PatternCount,
		ServerTime:   time.Now().UTC().Format(time.RFC3339),
	}

	return &RPCResponse{
		Result: result,
		ID:     req.ID,
	}
}

// Helper to convert int to string without fmt
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	if i == 1 {
		return "1"
	}
	if i == 2 {
		return "2"
	}
	// For larger numbers, use a simple approach
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

// DefaultHookSocketPath returns the default path for the hook query socket.
func DefaultHookSocketPath() string {
	// Use the same socket as the main daemon
	return DefaultSocketPath()
}
