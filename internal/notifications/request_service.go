package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"text/template"
	"time"
)

// RequestSettings is opt-in. Explicit destinations avoid inventing a broadcast
// identity. Secrets are supplied separately, never embedded in configuration.
type RequestSettings struct {
	Enabled             bool     `toml:"enabled" mapstructure:"enabled"`
	LookbackSeconds     int      `toml:"lookback_seconds" mapstructure:"lookback_seconds"`
	MaxPerMinute        int      `toml:"max_per_minute" mapstructure:"max_per_minute"`
	AgentMailURL        string   `toml:"agent_mail_url" mapstructure:"agent_mail_url"`
	AgentMailSender     string   `toml:"agent_mail_sender" mapstructure:"agent_mail_sender"`
	AgentMailRecipients []string `toml:"agent_mail_recipients" mapstructure:"agent_mail_recipients"`
	WebhookURL          string   `toml:"webhook_url" mapstructure:"webhook_url"`
	TemplatePath        string   `toml:"template" mapstructure:"template"`
}

const requestNoticeTemplate = `SLB request lifecycle observation (not an execution permit).

Project: {{printf "%q" .Project}}
Request: {{printf "%q" .RequestID}}
Event: {{printf "%q" .Event}} at {{.OccurredAt}}
Observed status: {{printf "%q" .Status}}
Risk tier: {{printf "%q" .Tier}}
Requestor session: {{printf "%q" .RequestorSessionID}}
Command fingerprint: {{.CommandHash}}
Required approvals: {{.MinApprovals}}
Different-model requirement: {{.RequireDifferentModel}}
Submitted review rows: {{.Approvals}} approve / {{.Rejections}} reject
{{if .ReviewDecision}}Review decision recorded: {{printf "%q" .ReviewDecision}}{{end}}
{{if .ExitCode}}Recorded exit code: {{.ExitCode}}{{end}}

Read the CURRENT request and full evidence using slb review with this request ID
in the named project. These counts are submitted rows, not verified approvals.
A delayed notification may describe a request that has since changed or ended.
Never execute from a notification: SLB's execution gate must recheck permission.
For native Git authorization requests, an executed token means hook permission,
not success of the eventual Git operation.
Notification ID: {{.ID}}
`

// NewRequestMailRoute shares MCP transport with blocked alerts but has its own
// metadata-only renderer and destination checkpoint. No Alert coercion occurs.
func NewRequestMailRoute(cfg MailConfig, templatePath string) (RequestRoute, error) {
	cfg, err := validateMailConfig(cfg)
	if err != nil {
		return RequestRoute{}, err
	}
	renderer, err := loadTemplate("request", templatePath, requestNoticeTemplate)
	if err != nil {
		return RequestRoute{}, err
	}
	client := notificationHTTPClient()
	return RequestRoute{ID: mailDestinationID("request-mail", cfg), Send: func(ctx context.Context, notice RequestNotice) error {
		if notice.Project != cfg.Project {
			return errors.New("request mail project mismatch")
		}
		subject, body, importance, ack, err := renderRequestNotice(renderer, notice)
		if err != nil {
			return err
		}
		return sendAgentMail(ctx, client, cfg, subject, body, importance, ack)
	}}, nil
}

func renderRequestNotice(renderer *template.Template, notice RequestNotice) (string, string, string, bool, error) {
	notice.Cursor = "" // Custom templates must not disclose the resume token.
	var body boundedBuffer
	body.max = maxBodyBytes
	if err := renderer.Execute(&body, notice); err != nil {
		return "", "", "", false, err
	}
	// Fixed subjects never interpolate command text or untrusted identifiers.
	subject := "[SLB] Request lifecycle update"
	importance, ack := "normal", false
	switch notice.Status {
	case "pending":
		subject = "[SLB] Request awaiting independent review"
	case "approved":
		subject = "[SLB] Request approval recorded; recheck before execution"
	case "rejected":
		subject = "[SLB] Request rejected"
	case "escalated", "timeout":
		subject, importance, ack = "[SLB] Request needs operator attention", "urgent", true
	case "execution_failed", "timed_out":
		subject, importance = "[SLB] Request execution failure recorded", "urgent"
	case "executed":
		subject = "[SLB] Request completion recorded"
	case "cancelled":
		subject = "[SLB] Request cancelled"
	}
	if notice.Tier == "critical" && (notice.Status == "pending" || notice.Status == "escalated") {
		importance, ack = "urgent", true
	}
	// Tombstones and baselines retain their actual event semantics; never
	// describe a deletion or baseline as a newly created approval request.
	switch notice.Event {
	case "request_deleted", "request_removed":
		subject, importance, ack = "[SLB] Request removed from project", "normal", false
	case "request_baseline":
		subject = "[SLB] Existing request journal baseline"
	}
	return subject, body.String(), importance, ack, nil
}

func NewRequestWebhookRoute(endpoint string) (RequestRoute, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return RequestRoute{}, err
	}
	client := notificationHTTPClient()
	return RequestRoute{ID: destinationID("request-webhook", endpoint), Send: func(ctx context.Context, notice RequestNotice) error {
		data, err := json.Marshal(notice)
		if err != nil {
			return err
		}
		if len(data) > 64*1024 {
			return errors.New("request webhook payload exceeds 64 KiB")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return errors.New("invalid request webhook")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "SLB-Request-Notifications/1.0")
		req.Header.Set("X-SLB-Event-ID", notice.ID)
		response, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("request webhook transport failed")
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("request webhook rejected delivery (HTTP %d)", response.StatusCode)
		}
		return nil
	}}, nil
}

// RequestService runs outside command admission, review and execution paths.
// The daemon owns cancellation and joins it before closing the source DB.
type RequestService struct {
	dispatcher *RequestDispatcher
	routes     []RequestRoute
}

func NewRequestService(project string, settings RequestSettings, source RequestNoticeSource, bearerToken, senderToken string) (*RequestService, error) {
	if !settings.Enabled {
		return nil, nil // No validation, templates, environment, disk or source I/O.
	}
	if !filepath.IsAbs(project) || source == nil || settings.LookbackSeconds < 0 || settings.LookbackSeconds > 30*86400 {
		return nil, errors.New("invalid request notification settings")
	}
	policy, err := (RequestDeliveryPolicy{Lookback: time.Duration(settings.LookbackSeconds) * time.Second, MaxPerMinute: settings.MaxPerMinute}).normalized()
	if err != nil {
		return nil, err
	}
	service := &RequestService{dispatcher: &RequestDispatcher{Project: project, Policy: policy, Source: source,
		StatePath: filepath.Join(project, ".slb", "notifications", "request-delivery.json")}}
	if settings.AgentMailURL != "" {
		// A dedicated lifecycle thread keeps these notices separate from
		// blocked-alert conversations. The body still carries the event ID.
		route, err := NewRequestMailRoute(MailConfig{URL: settings.AgentMailURL, Project: project,
			Sender: settings.AgentMailSender, Recipients: settings.AgentMailRecipients, Thread: "SLB-Requests",
			BearerToken: bearerToken, SenderToken: senderToken}, settings.TemplatePath)
		if err != nil {
			return nil, err
		}
		service.routes = append(service.routes, route)
	}
	if settings.WebhookURL != "" {
		route, err := NewRequestWebhookRoute(settings.WebhookURL)
		if err != nil {
			return nil, err
		}
		service.routes = append(service.routes, route)
	}
	if len(service.routes) == 0 {
		return nil, errors.New("enabled request notifications require Agent Mail or a webhook destination")
	}
	return service, nil
}

func (s *RequestService) Check(ctx context.Context) (RequestDeliveryReport, error) {
	if s == nil {
		return RequestDeliveryReport{}, nil
	}
	return s.dispatcher.Dispatch(ctx, time.Now().UTC(), s.routes)
}

func (s *RequestService) Run(ctx context.Context, onError func(error)) {
	if s == nil {
		return
	}
	check := func() {
		pass, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := s.Check(pass); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
	check()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
