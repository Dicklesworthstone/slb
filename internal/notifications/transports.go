package notifications

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const maxReplyBytes = 1024 * 1024
const maxTemplateBytes = 16 * 1024
const maxBodyBytes = 32 * 1024

// Renderer executes operator-supplied text templates against Alert only. There
// are no file, environment, shell, or network template functions.
type Renderer struct{ blocked, repeated *template.Template }

func NewRenderer(blockedPath, repeatedPath string) (*Renderer, error) {
	base := "SLB observed blocked hook decisions; this is not an execution report.\n\nProject: {{.Project}}\nSession label: {{.SessionID}}\nTier: {{.Tier}}\nAttempts in {{.WindowSeconds}} seconds: {{.Attempts}}\nCommand (redacted, quoted): {{printf \"%q\" .CommandRedacted}}\nFingerprint: {{.CommandHash}}\nFirst observed: {{.FirstAt}}\nLast observed: {{.LastAt}}\n\nInspect with slb audit and submit an intentional operation with slb request for independent review. This alert does not approve anything.\n"
	blocked, err := loadTemplate("blocked", blockedPath, base)
	if err != nil {
		return nil, err
	}
	repeated, err := loadTemplate("repeated", repeatedPath, "Repeated blocked attempts require review; repetition alone does not establish intent.\n\n"+base)
	if err != nil {
		return nil, err
	}
	return &Renderer{blocked: blocked, repeated: repeated}, nil
}

func loadTemplate(name, path, fallback string) (*template.Template, error) {
	text := fallback
	if path != "" {
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, path[2:])
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s alert template: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxTemplateBytes {
			return nil, errors.New("alert template must be a regular file no larger than 16 KiB")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxTemplateBytes+1))
		_ = file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(data) > maxTemplateBytes {
			return nil, errors.New("alert template exceeds 16 KiB")
		}
		text = string(data)
	}
	return template.New(name).Option("missingkey=error").Parse(text)
}

type boundedBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("notification content exceeds size limit")
	}
	return b.Buffer.Write(p)
}

func (r *Renderer) Render(a Alert) (string, string, error) {
	subject := "[SLB] Blocked " + a.Tier + " command"
	t := r.blocked
	if a.Repeated {
		subject = "[SLB] Repeated blocked command attempts"
		t = r.repeated
	}
	var body boundedBuffer
	body.max = maxBodyBytes
	if err := t.Execute(&body, a); err != nil {
		return "", "", err
	}
	return subject, body.String(), nil
}

func destinationID(kind string, values ...string) string {
	data, _ := json.Marshal(values)
	digest := sha256.Sum256(data)
	return kind + ":" + hex.EncodeToString(digest[:])
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid notification endpoint")
	}
	if u.Scheme == "https" {
		return nil
	}
	// Plain HTTP is only for an explicitly local service, never remote tokens.
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errors.New("notification endpoint requires HTTPS or loopback HTTP")
}

func notificationHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// NewWebhookRoute sends redacted alert JSON. It does not follow redirects or
// include remote response bodies/credential-bearing URLs in returned errors.
func NewWebhookRoute(endpoint string) (Route, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return Route{}, err
	}
	client := notificationHTTPClient()
	return Route{ID: destinationID("webhook", endpoint), Send: func(ctx context.Context, a Alert) error {
		event := "blocked_command"
		if a.Repeated {
			event = "repeated_blocked_command"
		}
		data, err := json.Marshal(struct {
			Event string `json:"event"`
			Alert Alert  `json:"alert"`
		}{event, a})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return errors.New("invalid webhook request")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "SLB-Blocked-Alerts/1.0")
		req.Header.Set("X-SLB-Alert-ID", destinationID("alert", a.Key, a.EventID, strconv.FormatBool(a.Repeated)))
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("webhook transport failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook rejected alert (HTTP %d)", resp.StatusCode)
		}
		return nil
	}}, nil
}

// MailConfig targets existing registered Agent Mail identities. Authentication
// and contact permissions remain the server's responsibility: no auto-register,
// broadcast alias, contact-policy override or human impersonation is attempted.
type MailConfig struct {
	URL, Project, Sender, Thread string
	Recipients                   []string
	HumanRecipient               string
	BearerToken                  string `json:"-"`
	SenderToken                  string `json:"-"`
}

func validateMailConfig(cfg MailConfig) (MailConfig, error) {
	if err := validateEndpoint(cfg.URL); err != nil {
		return cfg, err
	}
	if !filepath.IsAbs(cfg.Project) || cfg.Sender == "" || len(cfg.Recipients) == 0 || len(cfg.Recipients) > 50 {
		return cfg, errors.New("Agent Mail requires a project, registered sender and recipients")
	}
	cfg.Recipients = append([]string(nil), cfg.Recipients...)
	names := append(append([]string(nil), cfg.Recipients...), cfg.Sender)
	if cfg.HumanRecipient != "" {
		names = append(names, cfg.HumanRecipient)
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") {
			return cfg, errors.New("invalid Agent Mail identity")
		}
	}
	if cfg.Thread == "" {
		cfg.Thread = "SLB-Reviews"
	}
	return cfg, nil
}

func mailDestinationID(kind string, cfg MailConfig) string {
	idFields := []string{cfg.URL, cfg.Project, cfg.Sender, cfg.Thread, cfg.HumanRecipient}
	idFields = append(idFields, cfg.Recipients...)
	return destinationID(kind, idFields...)
}

func NewAgentMailRoute(cfg MailConfig, renderer *Renderer) (Route, error) {
	cfg, err := validateMailConfig(cfg)
	if err != nil {
		return Route{}, err
	}
	if renderer == nil {
		return Route{}, errors.New("Agent Mail requires an alert renderer")
	}
	client := notificationHTTPClient()
	return Route{ID: mailDestinationID("agent-mail", cfg), Send: func(ctx context.Context, a Alert) error {
		subject, body, err := renderer.Render(a)
		if err != nil {
			return err
		}
		if filepath.Clean(a.Project) != filepath.Clean(cfg.Project) {
			return errors.New("Agent Mail project mismatch")
		}
		return sendAgentMail(ctx, client, cfg, subject, body, a.Importance(), a.Repeated)
	}}, nil
}

// sendAgentMail is shared by blocked alerts and request lifecycle delivery.
// The content/routing policy stays with each caller; the actual protocol and
// positive send_message confirmation have a single implementation.
func sendAgentMail(ctx context.Context, client *http.Client, cfg MailConfig, subject, body, importance string, acknowledgment bool) error {
	if len(body) > maxBodyBytes || len(subject) > 512 {
		return errors.New("Agent Mail message exceeds size limit")
	}
	initial, session, err := mcpPost(ctx, client, cfg, "", "", 1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "slb-notifications", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(initial, &initialized); err != nil {
		return errors.New("invalid MCP initialize result")
	}
	version := initialized.ProtocolVersion
	if version != "2025-11-25" && version != "2025-06-18" && version != "2025-03-26" {
		return errors.New("unsupported MCP protocol version")
	}
	if session != "" {
		defer closeMCPSession(ctx, client, cfg, session, version)
	}
	if _, _, err := mcpPost(ctx, client, cfg, session, version, 0, "notifications/initialized", nil); err != nil {
		return err
	}
	args := map[string]any{"project_key": cfg.Project, "sender_name": cfg.Sender, "to": cfg.Recipients,
		"subject": subject, "body_md": body, "thread_id": cfg.Thread, "importance": importance,
		"ack_required": acknowledgment, "format": "json"}
	if cfg.SenderToken != "" {
		args["sender_token"] = cfg.SenderToken
	}
	if acknowledgment && cfg.HumanRecipient != "" {
		args["cc"] = []string{cfg.HumanRecipient}
	}
	result, _, err := mcpPost(ctx, client, cfg, session, version, 2, "tools/call", map[string]any{"name": "send_message", "arguments": args})
	if err != nil {
		return err
	}
	var tool struct {
		IsError    bool                          `json:"isError"`
		Structured json.RawMessage               `json:"structuredContent"`
		Content    []struct{ Type, Text string } `json:"content"`
	}
	if err := json.Unmarshal(result, &tool); err != nil || tool.IsError {
		return errors.New("Agent Mail send_message failed")
	}
	payload := tool.Structured
	if len(payload) == 0 || string(payload) == "null" {
		for _, content := range tool.Content {
			if content.Type == "text" {
				payload = json.RawMessage(content.Text)
				break
			}
		}
	}
	var delivered struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(payload, &delivered); err != nil || delivered.Count < 1 {
		return errors.New("Agent Mail did not confirm delivery")
	}
	return nil
}

func mcpPost(ctx context.Context, client *http.Client, cfg MailConfig, session, version string, id int, method string, params any) (json.RawMessage, string, error) {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != 0 {
		message["id"] = id
	}
	if params != nil {
		message["params"] = params
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(data))
	if err != nil {
		return nil, "", errors.New("invalid Agent Mail request")
	}
	setMCPHeaders(req, cfg, session, version)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", errors.New("Agent Mail transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Agent Mail rejected request (HTTP %d)", resp.StatusCode)
	}
	if id == 0 {
		if resp.StatusCode != http.StatusAccepted {
			return nil, "", errors.New("MCP notification was not accepted")
		}
		return nil, session, nil
	}
	newSession := resp.Header.Get("Mcp-Session-Id")
	for _, ch := range newSession {
		if ch < 0x21 || ch > 0x7e {
			return nil, "", errors.New("invalid MCP session header")
		}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", errors.New("invalid MCP response content type")
	}
	result, err := readMCPResult(resp.Body, media, id)
	return result, newSession, err
}

func setMCPHeaders(req *http.Request, cfg MailConfig, session, version string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
}

func closeMCPSession(parent context.Context, client *http.Client, cfg MailConfig, session, version string) {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.URL, nil)
	if err != nil {
		return
	}
	setMCPHeaders(req, cfg, session, version)
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// readMCPResult accepts JSON and bounded SSE, ignoring only notifications or
// server requests. An unrelated response, remote error, truncated stream or
// missing result is not delivery. It never returns server error text/secrets.
func readMCPResult(input io.Reader, media string, id int) (json.RawMessage, error) {
	decode := func(data []byte) (json.RawMessage, bool, error) {
		var response struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &response); err != nil || response.JSONRPC != "2.0" {
			return nil, false, errors.New("invalid MCP response")
		}
		if response.Method != "" {
			return nil, false, nil
		}
		if string(response.ID) != strconv.Itoa(id) {
			return nil, false, errors.New("MCP response ID mismatch")
		}
		if (len(response.Error) > 0 && string(response.Error) != "null") || len(response.Result) == 0 || string(response.Result) == "null" {
			return nil, false, errors.New("MCP request failed or returned no result")
		}
		return response.Result, true, nil
	}
	limited := &io.LimitedReader{R: input, N: maxReplyBytes + 1}
	if media == "application/json" {
		data, err := io.ReadAll(limited)
		if err != nil {
			return nil, errors.New("reading MCP response failed")
		}
		if len(data) > maxReplyBytes {
			return nil, errors.New("MCP response exceeds 1 MiB")
		}
		result, found, err := decode(data)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("MCP response missing")
		}
		return result, nil
	}
	if media != "text/event-stream" {
		return nil, errors.New("unsupported MCP response content type")
	}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxReplyBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() != 0 {
				result, found, err := decode([]byte(data.String()))
				if err != nil {
					return nil, err
				}
				if found {
					return result, nil
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimPrefix(line[5:], " ")
			if part == "" && data.Len() == 0 {
				continue
			}
			if data.Len() != 0 {
				data.WriteByte('\n')
			}
			data.WriteString(part)
		}
	}
	return nil, errors.New("MCP event stream ended without a complete response")
}

func NewDesktopRoute(renderer *Renderer) Route {
	return Route{ID: "desktop:v1", Send: func(ctx context.Context, a Alert) error {
		subject, _, err := renderer.Render(a)
		if err != nil {
			return err
		}
		// Command text stays out of shell/AppleScript source entirely.
		message := fmt.Sprintf("%d blocked attempts. Project: %s. Inspect slb audit; no execution authorized.", a.Attempts, a.Project)
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "linux":
			cmd = exec.CommandContext(ctx, "notify-send", "--", subject, message)
		case "darwin":
			cmd = exec.CommandContext(ctx, "osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", "--", subject, message)
		default:
			return errors.New("desktop blocked alerts are unsupported on this platform")
		}
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("desktop alert delivery failed")
		}
		return nil
	}}
}
