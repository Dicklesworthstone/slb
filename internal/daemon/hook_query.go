// Package daemon provides hook query handling for Claude Code integration.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// slowHookQueryThreshold is well above the typical 10-30ms policy load.
var slowHookQueryThreshold = 100 * time.Millisecond

func (s *IPCServer) handleHookQuery(req RPCRequest) *RPCResponse {
	var params HookQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
	}
	if params.Command == "" {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "command is required"}, ID: req.ID}
	}
	started := time.Now()
	result := s.classifyCommand(params)
	// Native hook clients wait integrations.hook_query_timeout_ms (default
	// 250ms) before falling back to local policy. Make slow answers visible
	// in the daemon log so a client-side fallback can be correlated.
	if elapsed := time.Since(started); elapsed >= slowHookQueryThreshold && s.logger != nil {
		s.logger.Warn("slow hook query", "elapsed_ms", elapsed.Milliseconds(), "cwd", params.CWD,
			"action", result.Action)
	}
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

// hookPolicy is the effective, project-local policy for one hook query.
type hookPolicy struct {
	engine        *core.PatternEngine
	cautionAction string
	// statePath is the project's state database, or empty before `slb init`.
	statePath string
}

// hookPolicyCache keeps each project's compiled policy between hook queries
// (GH #24). Building it from scratch costs a config parse, a SQLite open and
// a regexp compilation per built-in and configured pattern, and the garbage
// from that work drove a GC cycle per query. Every query still revalidates every input the policy
// was built from, so an edit is never missed and one project's rules never
// leak into another's:
//   - the user and project config files, compared byte for byte and parsed
//     from the compared bytes, and the SLB_* overrides (config.Sources);
//   - the custom_patterns table, re-read only when the state database or
//     its WAL changed on disk, or when the last read raced a write.
//
// Entries are immutable once stored, so concurrent queries share an engine
// without holding the cache lock while they classify.
type hookPolicyCache struct {
	mu      sync.Mutex
	entries map[string]*cachedHookPolicy
}

// maxCachedHookPolicies bounds the cache for a daemon that answers hooks for
// many projects; past it an arbitrary entry is dropped and rebuilt on demand.
const maxCachedHookPolicies = 64

// hookStateRacyWindow covers coarse filesystem clocks (down to FAT's two
// seconds). A state file modified this close to the moment it was examined
// may be written again within the same timestamp tick, so the next query
// re-reads custom_patterns instead of trusting the unchanged stat (the rule
// Git applies to racily clean index entries).
const hookStateRacyWindow = 3 * time.Second

type cachedHookPolicy struct {
	sources  config.Sources
	cfg      config.Config
	hasState bool
	state    stateStamp
	custom   []core.Pattern
	policy   hookPolicy
}

// stateStamp records the state database and its WAL as last examined.
type stateStamp struct {
	db, wal fileStamp
	// racy means a timestamp fell inside hookStateRacyWindow of the moment
	// the stamp was taken, so equality with a later stamp proves nothing.
	racy bool
}

type fileStamp struct {
	info  os.FileInfo // nil when the file does not exist
	size  int64
	mtime time.Time
}

func statFileStamp(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{info: info, size: info.Size(), mtime: info.ModTime()}, nil
}

func (f fileStamp) equal(other fileStamp) bool {
	if f.info == nil || other.info == nil {
		return f.info == nil && other.info == nil
	}
	return os.SameFile(f.info, other.info) && f.size == other.size && f.mtime.Equal(other.mtime)
}

func statState(path string) (stateStamp, error) {
	// Take the reference time before the stats: any write after them gets a
	// timestamp at or after this instant, minus the clock's coarseness.
	examined := time.Now()
	dbStamp, err := statFileStamp(path)
	if err != nil {
		return stateStamp{}, err
	}
	walStamp, err := statFileStamp(path + "-wal")
	if err != nil {
		return stateStamp{}, err
	}
	horizon := examined.Add(-hookStateRacyWindow)
	racy := false
	for _, f := range []fileStamp{dbStamp, walStamp} {
		if f.info != nil && !f.mtime.Before(horizon) {
			racy = true
		}
	}
	return stateStamp{db: dbStamp, wal: walStamp, racy: racy}, nil
}

func (s stateStamp) unchangedSince(previous stateStamp) bool {
	return !previous.racy && s.db.equal(previous.db) && s.wal.equal(previous.wal)
}

func customPatternsEqual(a, b []core.Pattern) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Tier != b[i].Tier || a[i].Pattern != b[i].Pattern ||
			a[i].Description != b[i].Description || a[i].Source != b[i].Source {
			return false
		}
	}
	return true
}

func readHookCustomPatterns(path string) ([]core.Pattern, error) {
	conn, err := db.OpenWithOptions(path, db.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return core.ReadCustomPatterns(conn)
}

// policyFor returns the effective policy for a hook query's cwd, reusing the
// cached compilation when none of its inputs changed.
func (c *hookPolicyCache) policyFor(cwd string) (hookPolicy, error) {
	if cwd == "" {
		return hookPolicy{engine: core.NewPatternEngine(), cautionAction: "block"}, nil
	}
	if !filepath.IsAbs(cwd) {
		return hookPolicy{}, fmt.Errorf("hook cwd must be absolute")
	}
	root := ProjectRootForCWD(cwd)
	opts := config.LoadOptions{ProjectDir: root}
	hasState := true
	if _, err := os.Stat(filepath.Join(root, ".slb")); os.IsNotExist(err) {
		hasState = false
	} else if err != nil {
		return hookPolicy{}, err
	}
	statePath := ""
	if hasState {
		statePath = filepath.Join(root, ".slb", "state.db")
	}
	// The configuration is parsed from these very bytes (LoadSources), so a
	// cached policy always matches the snapshot it is compared by.
	sources, err := config.ReadSources(opts)
	if err != nil {
		c.forget(root)
		return hookPolicy{}, err
	}
	var state stateStamp
	if hasState {
		if state, err = statState(statePath); err != nil {
			c.forget(root)
			return hookPolicy{}, err
		}
	}

	cached := c.lookup(root)
	var entry *cachedHookPolicy
	switch {
	case cached != nil && cached.hasState == hasState && cached.sources.Equal(sources) &&
		(!hasState || state.unchangedSince(cached.state)):
		return cached.policy, nil
	case cached != nil && cached.hasState == hasState && cached.sources.Equal(sources):
		// Only the state database moved. Most writes there are requests and
		// reviews; recompile only if the custom rules themselves changed.
		custom, err := readHookCustomPatterns(statePath)
		if err != nil {
			c.forget(root)
			return hookPolicy{}, err
		}
		next := *cached
		next.state, next.custom = state, custom
		if !customPatternsEqual(custom, cached.custom) {
			engine, err := core.CompileCommandPolicy(cached.cfg, custom, "")
			if err != nil {
				c.forget(root)
				return hookPolicy{}, err
			}
			next.policy.engine = engine
		}
		entry = &next
	default:
		cfg, err := config.LoadSources(sources)
		if err != nil {
			c.forget(root)
			return hookPolicy{}, fmt.Errorf("loading command policy: %w", err)
		}
		var custom []core.Pattern
		if hasState {
			if custom, err = readHookCustomPatterns(statePath); err != nil {
				c.forget(root)
				return hookPolicy{}, err
			}
		}
		engine, err := core.CompileCommandPolicy(cfg, custom, "")
		if err != nil {
			c.forget(root)
			return hookPolicy{}, err
		}
		entry = &cachedHookPolicy{
			sources: sources, cfg: cfg, hasState: hasState, state: state, custom: custom,
			policy: hookPolicy{engine: engine, cautionAction: cfg.Integrations.HookCautionAction, statePath: statePath},
		}
	}
	c.store(root, entry)
	return entry.policy, nil
}

func (c *hookPolicyCache) lookup(root string) *cachedHookPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[root]
}

func (c *hookPolicyCache) store(root string, entry *cachedHookPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*cachedHookPolicy)
	}
	if _, ok := c.entries[root]; !ok && len(c.entries) >= maxCachedHookPolicies {
		for evict := range c.entries {
			delete(c.entries, evict)
			break
		}
	}
	c.entries[root] = entry
}

// forget drops a project's entry after a failed load so that a broken policy
// is rebuilt, and fails closed again, on every query until it is repaired.
func (c *hookPolicyCache) forget(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, root)
}

func (s *IPCServer) classifyCommand(params HookQueryParams) *HookQueryResult {
	policy, err := s.hookPolicies.policyFor(params.CWD)
	if err != nil {
		return &HookQueryResult{
			Action: "block", Tier: "unknown", MatchedPattern: "policy_load_error",
			Message: "SLB: project policy could not be loaded; command blocked until policy is repaired.",
		}
	}
	engine, cautionAction := policy.engine, policy.cautionAction
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

	if policy.statePath == "" || params.SessionID == "" {
		return result
	}
	// Only a command that needs approval reaches the database here.
	conn, err := db.OpenWithOptions(policy.statePath, db.OpenOptions{ReadOnly: true})
	if err != nil {
		return result
	}
	defer conn.Close()
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
			SessionID: params.SessionID, DatabasePath: policy.statePath,
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
	`, params.Command, params.CWD, ProjectRootForCWD(params.CWD), params.SessionID).Scan(&id)
	if err != nil {
		return nil
	}
	req, err := conn.GetRequest(id)
	if err != nil || req.Status != db.StatusApproved || req.ProjectPath != ProjectRootForCWD(params.CWD) || req.Command.Raw != params.Command ||
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
	Status            string `json:"status"`
	Uptime            int64  `json:"uptime_seconds"`
	PatternHash       string `json:"pattern_hash"`
	PatternCount      int    `json:"pattern_count"`
	HookCautionAction string `json:"hook_caution_action"`
	ServerTime        string `json:"server_time"`
	PolicyError       string `json:"policy_error,omitempty"`
}

func (s *IPCServer) handleHookHealth(req RPCRequest) *RPCResponse {
	now := time.Now().UTC()
	engine := core.GetDefaultEngine()
	cautionAction := config.DefaultConfig().Integrations.HookCautionAction
	if len(req.Params) != 0 {
		var params HookHealthParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
		}
		if params.CWD != "" {
			policy, err := s.hookPolicies.policyFor(params.CWD)
			if err != nil {
				return &RPCResponse{Result: HookHealthResult{
					Status: "degraded", Uptime: int64(time.Since(s.startTime).Seconds()),
					ServerTime: now.Format(time.RFC3339), PolicyError: err.Error(),
				}, ID: req.ID}
			}
			engine, cautionAction = policy.engine, policy.cautionAction
		}
	}
	export := engine.Export()
	result := HookHealthResult{
		Status: "ok", Uptime: int64(time.Since(s.startTime).Seconds()),
		PatternHash: export.SHA256, PatternCount: export.Metadata.PatternCount,
		HookCautionAction: cautionAction, ServerTime: now.Format(time.RFC3339),
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
