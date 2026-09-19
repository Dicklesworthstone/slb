package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// ErrPreflightRequired means the caller required successful preview evidence,
// but no request was admitted. It never means the original command was run.
var ErrPreflightRequired = errors.New("a successful dry run is required")

// PreflightReport is persisted as attachment metadata in the same transaction
// as the request. Existing dry_run_command/output columns carry display-safe
// output, so readers need no schema migration to see failure or missing evidence.
// The command hash binds the report to the requested argv/CWD/shell snapshot.
type PreflightReport struct {
	Status      string    `json:"status"`
	CommandHash string    `json:"command_hash"`
	Command     string    `json:"command,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	DurationMs  int64     `json:"duration_ms"`
	Error       string    `json:"error,omitempty"`
}

func (rc *RequestCreator) prepareRequestPreflight(ctx context.Context, request *db.Request, opts CreateRequestOptions) (*PreflightReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Avoid running external tools on every queue retry when no capacity is
	// available. This read is only an optimization: AdmitRequest rechecks quota
	// and identity under the writer reservation after the preview finishes.
	if rc.config.EnableDryRun {
		capacity, err := rc.rateLimiter.CheckRateLimit(request.RequestorSessionID)
		if err != nil {
			return nil, err
		}
		if !capacity.Allowed {
			if capacity.limitError == nil {
				return nil, errors.New("capacity check denied admission without quota details")
			}
			return nil, capacity.limitError
		}
	}
	report := &PreflightReport{
		Status: "disabled", CommandHash: db.ComputeCommandHash(request.Command),
		StartedAt: time.Now().UTC(),
	}
	var preview *db.DryRunResult
	var previewErr error
	if rc.config.EnableDryRun {
		timeout := defaultDryRunTimeout
		if opts.DryRunTimeout > 0 && opts.DryRunTimeout < timeout {
			timeout = opts.DryRunTimeout
		}
		previewCtx, cancel := context.WithTimeout(ctx, timeout)
		preview, previewErr = RunDryRunContext(previewCtx, &request.Command)
		cancel()
		// Caller cancellation aborts the submission, even if a tool returned
		// output concurrently. The request has not yet been inserted/notified.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// RunDryRun also catches process signals. Those cancel the request;
		// they must not turn an interrupted preflight into a reviewable one.
		if errors.Is(previewErr, context.Canceled) {
			return nil, previewErr
		}
		switch {
		case errors.Is(previewErr, context.DeadlineExceeded):
			report.Status = "timed_out"
		case previewErr != nil:
			report.Status = "failed"
		case preview == nil:
			report.Status = "unsupported"
		default:
			report.Status = "succeeded"
		}
	}
	report.CompletedAt = time.Now().UTC()
	report.DurationMs = report.CompletedAt.Sub(report.StartedAt).Milliseconds()
	if previewErr != nil {
		report.Error = ApplyRedaction(previewErr.Error(), opts.RedactPatterns)
	}
	if preview != nil {
		report.Command = ApplyRedaction(preview.Command, opts.RedactPatterns)
	}
	if opts.RequireDryRun && report.Status != "succeeded" {
		// Do not wrap the raw process error: filenames and tool diagnostics
		// can contain secrets. The report/error text is already redacted.
		return nil, fmt.Errorf("%w: %s %s", ErrPreflightRequired, report.Status, report.Error)
	}

	note := fmt.Sprintf("SLB preflight: %s (captured %s). Advisory evidence only; not an approval.", report.Status, report.CompletedAt.Format(time.RFC3339Nano))
	if report.Error != "" {
		note += "\nPreview error: " + report.Error
	}
	if preview != nil {
		request.DryRun = &db.DryRunResult{
			Command: report.Command,
			Output:  note + "\n\n" + ApplyRedaction(preview.Output, opts.RedactPatterns),
		}
	}
	// Metadata remains machine-readable through ordinary request/review reads.
	// Copy the slice: adding generated evidence must not mutate caller attachments.
	metadata := map[string]any{
		"source": "slb", "kind": "preflight", "version": 1,
		"status": report.Status, "command_hash": report.CommandHash,
		"command": report.Command, "started_at": report.StartedAt.Format(time.RFC3339Nano),
		"completed_at": report.CompletedAt.Format(time.RFC3339Nano), "duration_ms": report.DurationMs,
	}
	if report.Error != "" {
		metadata["error"] = report.Error
	}
	attachments := make([]db.Attachment, 0, len(request.Attachments)+1)
	attachments = append(attachments, request.Attachments...)
	request.Attachments = append(attachments, db.Attachment{Type: db.AttachmentTypeContext, Content: note, Metadata: metadata})
	return report, nil
}
