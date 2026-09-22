package integrations

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// Importance levels mapped from risk tiers.
const (
	ImportanceLow    = "low"
	ImportanceNormal = "normal"
	ImportanceUrgent = "urgent"
)

// AgentMailClient sends best-effort legacy CLI notifications only when durable
// request-journal delivery is disabled. Journal mode performs no synchronous
// transport: the committed mutation is the notification source of truth.
type AgentMailClient struct {
	projectKey      string
	threadID        string
	sender          string
	journalDelivery bool
	configErr       error
}

// NewAgentMailClient constructs a client.
func NewAgentMailClient(projectKey, threadID, sender string) *AgentMailClient {
	if threadID == "" {
		threadID = "SLB-Reviews"
	}
	if sender == "" {
		sender = "SLB-System"
	}
	cfg, err := config.Load(config.LoadOptions{ProjectDir: projectKey})
	return &AgentMailClient{
		projectKey:      projectKey,
		threadID:        threadID,
		sender:          sender,
		journalDelivery: cfg.Notifications.Requests.Enabled,
		configErr:       err,
	}
}

// NotifyNewRequest sends a notification when a request is created.
func (c *AgentMailClient) NotifyNewRequest(req *db.Request) error {
	subject := fmt.Sprintf("[SLB] %s: %s", strings.ToUpper(string(req.RiskTier)), truncate(safeDisplay(req), 60))
	body := fmt.Sprintf("## Command Approval Request\n\n**ID**: %s\n**Risk**: %s\n**Command**: `%s`\n\n### Justification\n- Reason: %s\n- Expected: %s\n- Goal: %s\n- Safety: %s\n\n---\nTo review: `slb review %s`\nTo approve: `slb approve %s --session-id <your-session> --session-key <key>`\nTo reject: `slb reject %s --session-id <your-session> --session-key <key>`\n",
		req.ID, req.RiskTier, safeDisplay(req),
		req.Justification.Reason,
		req.Justification.ExpectedEffect,
		req.Justification.Goal,
		req.Justification.SafetyArgument,
		req.ID, req.ID, req.ID,
	)
	return c.send(subject, body, importanceForTier(req.RiskTier))
}

// NotifyRequestApproved sends a notification on approval.
func (c *AgentMailClient) NotifyRequestApproved(req *db.Request, review *db.Review) error {
	subject := fmt.Sprintf("[SLB] APPROVED: %s", truncate(safeDisplay(req), 60))
	body := fmt.Sprintf("Request %s approved by %s (%s) at %s\n\nCommand: `%s`\n",
		req.ID, review.ReviewerAgent, review.ReviewerModel, review.CreatedAt.Format(time.RFC3339), safeDisplay(req))
	return c.send(subject, body, ImportanceNormal)
}

// NotifyRequestRejected sends a notification on rejection.
func (c *AgentMailClient) NotifyRequestRejected(req *db.Request, review *db.Review) error {
	subject := fmt.Sprintf("[SLB] REJECTED: %s", truncate(safeDisplay(req), 60))
	body := fmt.Sprintf("Request %s rejected by %s (%s) at %s\n\nComments: %s\nCommand: `%s`\n",
		req.ID, review.ReviewerAgent, review.ReviewerModel, review.CreatedAt.Format(time.RFC3339), review.Comments, safeDisplay(req))
	return c.send(subject, body, ImportanceNormal)
}

// NotifyRequestExecuted sends a notification on execution completion.
func (c *AgentMailClient) NotifyRequestExecuted(req *db.Request, exec *db.Execution, exitCode int) error {
	subject := fmt.Sprintf("[SLB] EXECUTED (%d): %s", exitCode, truncate(safeDisplay(req), 60))
	execTime := ""
	if exec != nil && exec.ExecutedAt != nil {
		execTime = exec.ExecutedAt.Format(time.RFC3339)
	}
	byAgent := ""
	byModel := ""
	logPath := ""
	if exec != nil {
		byAgent = exec.ExecutedByAgent
		byModel = exec.ExecutedByModel
		logPath = exec.LogPath
	}
	body := fmt.Sprintf("Request %s executed by %s (%s) at %s\nExit code: %d\nLog: %s\nCommand: `%s`\n",
		req.ID, byAgent, byModel, execTime, exitCode, logPath, safeDisplay(req))
	return c.send(subject, body, ImportanceLow)
}

// RequestNotifier defines notification hooks for request lifecycle.
type RequestNotifier interface {
	NotifyNewRequest(req *db.Request) error
	NotifyRequestApproved(req *db.Request, review *db.Review) error
	NotifyRequestRejected(req *db.Request, review *db.Review) error
	NotifyRequestExecuted(req *db.Request, exec *db.Execution, exitCode int) error
}

// NoopNotifier implements RequestNotifier and does nothing.
type NoopNotifier struct{}

func (n NoopNotifier) NotifyNewRequest(req *db.Request) error { return nil }
func (n NoopNotifier) NotifyRequestApproved(req *db.Request, review *db.Review) error {
	return nil
}
func (n NoopNotifier) NotifyRequestRejected(req *db.Request, review *db.Review) error {
	return nil
}
func (n NoopNotifier) NotifyRequestExecuted(req *db.Request, exec *db.Execution, exitCode int) error {
	return nil
}

func importanceForTier(t db.RiskTier) string {
	switch t {
	case db.RiskTierCritical:
		return ImportanceUrgent
	case db.RiskTierDangerous:
		return ImportanceNormal
	default:
		return ImportanceLow
	}
}

// safeDisplay chooses the redacted display value when available.
func safeDisplay(req *db.Request) string {
	if req.Command.DisplayRedacted != "" {
		return req.Command.DisplayRedacted
	}
	if req.Command.ContainsSensitive {
		return "[sensitive command omitted]"
	}
	return req.Command.Raw
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// send uses the Agent Mail CLI only for the legacy best-effort route. In
// journal mode the caller has committed the request/review mutation already;
// returning nil means no synchronous send is needed, NOT confirmed delivery.
func (c *AgentMailClient) send(subject, body, importance string) error {
	if c.configErr != nil {
		return fmt.Errorf("loading notification policy: %w", c.configErr)
	}
	if c.journalDelivery {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "mcp-agent-mail", "send",
		"--project", c.projectKey,
		"--from", c.sender,
		"--to", "SLB-Broadcast",
		"--subject", subject,
		"--thread", c.threadID,
		"--importance", importance,
		"--body-md", body,
	)

	// We don't treat missing CLI as fatal; capture stderr for diagnostics.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// If CLI missing, skip error to avoid blocking main flows.
		if strings.Contains(stderr.String(), "not found") || strings.Contains(err.Error(), "executable file not found") {
			return nil
		}
		return fmt.Errorf("agent mail send failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
