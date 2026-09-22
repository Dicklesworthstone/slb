package notifications

import (
	"context"
	"errors"
	"path/filepath"
	"time"
)

// Config is the opt-in [notifications.blocked] section. Tokens intentionally
// have no config fields: the daemon reads them from its environment instead.
type Config struct {
	Enabled             bool     `toml:"enabled" mapstructure:"enabled"`
	WindowSeconds       int      `toml:"window_seconds" mapstructure:"window_seconds"`
	RepeatThreshold     int      `toml:"repeat_threshold" mapstructure:"repeat_threshold"`
	CooldownSeconds     int      `toml:"cooldown_seconds" mapstructure:"cooldown_seconds"`
	MaxPerMinute        int      `toml:"max_per_minute" mapstructure:"max_per_minute"`
	AgentMailURL        string   `toml:"agent_mail_url" mapstructure:"agent_mail_url"`
	AgentMailSender     string   `toml:"agent_mail_sender" mapstructure:"agent_mail_sender"`
	AgentMailRecipients []string `toml:"agent_mail_recipients" mapstructure:"agent_mail_recipients"`
	HumanRecipient      string   `toml:"human_recipient" mapstructure:"human_recipient"`
	BlockedTemplate     string   `toml:"blocked_template" mapstructure:"blocked_template"`
	RepeatedTemplate    string   `toml:"repeated_template" mapstructure:"repeated_template"`
}

// Destinations reuses existing top-level channel toggles. Route credentials
// never enter the ledger, rendered messages, or returned diagnostic errors.
type Destinations struct {
	WebhookURL           string
	DesktopEnabled       bool
	AgentMailEnabled     bool
	AgentMailThread      string
	AgentMailToken       string `json:"-"`
	AgentMailSenderToken string `json:"-"`
}

// Service owns one project's dispatcher and runs separately from the hook RPC
// handler, request reconciliation and existing pending-request notifications.
type Service struct {
	Project, AuditDirectory string
	Dispatcher              *Dispatcher
	Routes                  []Route
}

// NewService performs no network I/O. A disabled configuration returns nil,
// without touching the audit store, delivery state, templates or destinations.
func NewService(project, auditDirectory string, cfg Config, destinations Destinations) (*Service, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if !filepath.IsAbs(project) || auditDirectory == "" {
		return nil, errors.New("blocked alerts require an absolute project and audit directory")
	}
	project = filepath.Clean(project)
	if canonical, err := filepath.EvalSymlinks(project); err == nil {
		project = canonical
	}
	// Check before multiplying seconds into a duration (avoid integer overflow).
	if cfg.WindowSeconds < 0 || cfg.WindowSeconds > 86400 || cfg.CooldownSeconds < 0 || cfg.CooldownSeconds > 86400 {
		return nil, errors.New("blocked alert window/cooldown must be 0..86400 seconds")
	}
	policy, err := (Policy{Window: time.Duration(cfg.WindowSeconds) * time.Second, RepeatThreshold: cfg.RepeatThreshold,
		Cooldown: time.Duration(cfg.CooldownSeconds) * time.Second, MaxPerMinute: cfg.MaxPerMinute}).normalized()
	if err != nil {
		return nil, err
	}
	renderer, err := NewRenderer(cfg.BlockedTemplate, cfg.RepeatedTemplate)
	if err != nil {
		return nil, err
	}
	routes := make([]Route, 0, 3)
	if destinations.AgentMailEnabled && cfg.AgentMailURL != "" {
		route, err := NewAgentMailRoute(MailConfig{URL: cfg.AgentMailURL, Project: project, Sender: cfg.AgentMailSender,
			Recipients: cfg.AgentMailRecipients, HumanRecipient: cfg.HumanRecipient, Thread: destinations.AgentMailThread,
			BearerToken: destinations.AgentMailToken, SenderToken: destinations.AgentMailSenderToken}, renderer)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	if destinations.WebhookURL != "" {
		route, err := NewWebhookRoute(destinations.WebhookURL)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	if destinations.DesktopEnabled {
		routes = append(routes, NewDesktopRoute(renderer))
	}
	if len(routes) == 0 {
		return nil, errors.New("blocked alerts enabled without an enabled destination")
	}
	return &Service{Project: project, AuditDirectory: auditDirectory, Routes: routes,
		Dispatcher: &Dispatcher{StatePath: filepath.Join(project, ".slb", "notifications", "blocked-delivery.json"), Policy: policy}}, nil
}

// Check only handles recent blocked audit events. Outages longer than Window
// leave older events in the audit log but do not produce stale alert floods.
func (s *Service) Check(ctx context.Context, now time.Time) (Report, error) {
	if s == nil {
		return Report{}, nil
	}
	alerts, err := ReadBlocked(ctx, s.AuditDirectory, s.Project, now, s.Dispatcher.Policy)
	if err != nil {
		return Report{}, err
	}
	return s.Dispatcher.Dispatch(ctx, now, alerts, s.Routes)
}

// Run catches up immediately, then polls. A whole pass is bounded to 15s;
// cancellation reaches scans and transports. There are no detached workers.
func (s *Service) Run(ctx context.Context, interval time.Duration, result func(Report, error)) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		pass, cancel := context.WithTimeout(ctx, 15*time.Second)
		report, err := s.Check(pass, time.Now().UTC())
		cancel()
		if ctx.Err() != nil {
			return
		}
		if result != nil {
			result(report, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
