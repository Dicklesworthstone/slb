// Package daemon provides hook query handling for Claude Code integration.
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// HookQueryParams are parameters for the hook_query method.
type HookQueryParams struct {
	Command          string `json:"command"`
	SessionID        string `json:"session_id"`
	CWD              string `json:"cwd"`
	ExecutionHandoff bool   `json:"execution_handoff,omitempty"`
}

// HookExecutionHandoff is an instruction to invoke the atomic SLB executor,
// never permission to execute the raw command directly. Queries do not consume
// approval; the executor claims it exactly once immediately before execution.
type HookExecutionHandoff struct {
	RequestID    string `json:"request_id"`
	CommandHash  string `json:"command_hash"`
	SessionID    string `json:"session_id"`
	DatabasePath string `json:"database_path"`
}

// HookQueryResult is the result of a hook query.
type HookQueryResult struct {
	Action           string                `json:"action"` // "allow", "block", "ask", "execute"
	Message          string                `json:"message"`
	Tier             string                `json:"tier"`
	MatchedPattern   string                `json:"matched_pattern"`
	MinApprovals     int                   `json:"min_approvals"`
	RequestID        string                `json:"request_id,omitempty"`
	ExecutionHandoff *HookExecutionHandoff `json:"execution_handoff,omitempty"`
	AuditRecorded    bool                  `json:"audit_recorded,omitempty"`
	AuditError       string                `json:"audit_error,omitempty"`
}

func (s *IPCServer) handleHookQuery(req RPCRequest) *RPCResponse {
	var params HookQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
	}
	if params.Command == "" {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "command is required"}, ID: req.ID}
	}
	result := s.classifyCommand(params)
	if result.Action == "block" || result.Action == "ask" {
		directory, err := audit.DefaultDirectory()
		if err == nil {
			err = recordHookAudit(directory, params, result)
		}
		result.AuditRecorded = err == nil
		if err != nil {
			result.AuditError = err.Error()
		}
	}
	return &RPCResponse{Result: result, ID: req.ID}
}

func recordHookAudit(directory string, params HookQueryParams, result *HookQueryResult) error {
	return audit.Record(directory, audit.Event{
		CommandRedacted: core.ApplyRedaction(params.Command, nil),
		CommandHash:     audit.CommandHash(params.Command, params.CWD),
		CWD:             params.CWD,
		SessionID:       params.SessionID,
		Action:          result.Action,
		Tier:            result.Tier,
		MatchedPattern:  result.MatchedPattern,
		MinApprovals:    result.MinApprovals,
		Source:          "daemon",
	})
}

// loadHookPolicy loads a fresh, project-local effective policy. A global
// mutable engine would miss edits or leak another project's allowlist.
func loadHookPolicy(cwd string) (*core.PatternEngine, *db.DB, string, error) {
	if cwd == "" {
		return core.NewPatternEngine(), nil, "", nil
	}
	if !filepath.IsAbs(cwd) {
		return nil, nil, "", fmt.Errorf("hook cwd must be absolute")
	}
	root := projectRootForSocket(cwd)
	opts := config.LoadOptions{ProjectDir: root}
	if _, err := os.Stat(filepath.Join(root, ".slb")); os.IsNotExist(err) {
		engine, loadErr := core.LoadCommandPolicy(nil, opts)
		return engine, nil, "", loadErr
	} else if err != nil {
		return nil, nil, "", err
	}
	path := filepath.Join(root, ".slb", "state.db")
	conn, err := db.OpenWithOptions(path, db.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, "", err
	}
	engine, err := core.LoadCommandPolicy(conn, opts)
	if err != nil {
		conn.Close()
		return nil, nil, "", err
	}
	return engine, conn, path, nil
}

func (s *IPCServer) classifyCommand(params HookQueryParams) *HookQueryResult {
	engine, conn, dbPath, err := loadHookPolicy(params.CWD)
	if err != nil {
		return &HookQueryResult{
			Action: "block", Tier: "unknown", MatchedPattern: "policy_load_error",
			Message: "SLB: project policy could not be loaded; command blocked until policy is repaired.",
		}
	}
	cautionAction := "block"
	if params.CWD != "" {
		cfg, cfgErr := config.Load(config.LoadOptions{ProjectDir: projectRootForSocket(params.CWD)})
		if cfgErr != nil {
			if conn != nil {
				conn.Close()
			}
			return &HookQueryResult{
				Action: "block", Tier: "unknown", MatchedPattern: "policy_load_error",
				Message: "SLB: hook policy could not be loaded; command blocked until policy is repaired.",
			}
		}
		cautionAction = cfg.Integrations.HookCautionAction
	}
	if conn != nil {
		defer conn.Close()
	}
	classification := engine.ClassifyCommand(params.Command, params.CWD)
	result := &HookQueryResult{
		Tier: string(classification.Tier), MatchedPattern: classification.MatchedPattern,
		MinApprovals: classification.MinApprovals,
	}
	switch {
	case classification.IsSafe:
		result.Action, result.Message = "allow", "Safe command"
		return result
	case classification.Tier == core.RiskTierCritical:
		result.Action, result.Message = "block", "CRITICAL: Requires "+itoa(classification.MinApprovals)+" approvals"
	case classification.Tier == core.RiskTierDangerous:
		result.Action, result.Message = "block", "DANGEROUS: Requires approval"
	case classification.Tier == core.RiskTierCaution:
		if cautionAction == "ask" {
			result.Action, result.Message = "ask", "CAUTION: Proceed with care"
		} else {
			result.Action = "block"
			result.Message = "CAUTION: Submit with slb request; configured auto-approval policy applies after admission."
		}
	default:
		result.Action, result.Message = "allow", "No matching pattern"
		return result
	}

	if conn == nil || params.SessionID == "" {
		return result
	}
	approved := findHookApproval(conn, engine, params, classification.MinApprovals)
	if approved == nil {
		return result
	}
	result.RequestID = approved.ID
	result.MinApprovals = approved.MinApprovals
	result.Message = "Approval ready. Execute with slb execute " + approved.ID + " --session-id <SLB_SESSION_ID>."
	if params.ExecutionHandoff {
		result.Action = "execute"
		result.ExecutionHandoff = &HookExecutionHandoff{
			RequestID: approved.ID, CommandHash: approved.Command.Hash,
			SessionID: params.SessionID, DatabasePath: dbPath,
		}
	}
	// Old clients must never get a raw-shell permit from this lookup.
	return result
}

func findHookApproval(conn *db.DB, engine *core.PatternEngine, params HookQueryParams, minApprovals int) *db.Request {
	var id string
	err := conn.QueryRow(`
		SELECT r.id FROM requests r JOIN sessions s ON s.id = r.requestor_session_id
		WHERE r.command_raw = ? AND r.command_cwd = ? AND r.project_path = ?
			AND r.requestor_session_id = ? AND r.status = 'approved'
			AND s.ended_at IS NULL AND s.project_path = r.project_path
			AND (r.approval_expires_at IS NULL OR julianday(r.approval_expires_at) > julianday('now'))
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1
	`, params.Command, params.CWD, projectRootForSocket(params.CWD), params.SessionID).Scan(&id)
	if err != nil {
		return nil
	}
	req, err := conn.GetRequest(id)
	if err != nil || req.Status != db.StatusApproved || req.ProjectPath != projectRootForSocket(params.CWD) || req.Command.Raw != params.Command ||
		req.Command.Cwd != params.CWD || req.RequestorSessionID != params.SessionID {
		return nil
	}
	// CanExecute checks the current project-local dynamic quorum; comparing
	// against the classifier's advisory full quorum would reject valid claims.
	if ok, _ := core.NewExecutor(conn, engine).CanExecute(id); !ok {
		return nil
	}
	return req
}

// HookHealthParams scopes diagnostics to the same project policy as hook_query.
type HookHealthParams struct {
	CWD string `json:"cwd,omitempty"`
}

// HookHealthResult is the result of a hook health check.
type HookHealthResult struct {
	Status       string `json:"status"`
	Uptime       int64  `json:"uptime_seconds"`
	PatternHash  string `json:"pattern_hash"`
	PatternCount int    `json:"pattern_count"`
	ServerTime   string `json:"server_time"`
	PolicyError  string `json:"policy_error,omitempty"`
}

func (s *IPCServer) handleHookHealth(req RPCRequest) *RPCResponse {
	now := time.Now().UTC()
	engine := core.GetDefaultEngine()
	var policyConn *db.DB
	if len(req.Params) != 0 {
		var params HookHealthParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
		}
		if params.CWD != "" {
			fresh, conn, _, err := loadHookPolicy(params.CWD)
			if err != nil {
				return &RPCResponse{Result: HookHealthResult{
					Status: "degraded", Uptime: int64(time.Since(s.startTime).Seconds()),
					ServerTime: now.Format(time.RFC3339), PolicyError: err.Error(),
				}, ID: req.ID}
			}
			engine, policyConn = fresh, conn
		}
	}
	if policyConn != nil {
		defer policyConn.Close()
	}
	export := engine.Export()
	result := HookHealthResult{
		Status: "ok", Uptime: int64(time.Since(s.startTime).Seconds()),
		PatternHash: export.SHA256, PatternCount: export.Metadata.PatternCount,
		ServerTime: now.Format(time.RFC3339),
	}
	return &RPCResponse{Result: result, ID: req.ID}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func DefaultHookSocketPath() string { return DefaultSocketPath() }
