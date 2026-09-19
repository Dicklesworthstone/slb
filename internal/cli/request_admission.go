package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	for _, cmd := range []*cobra.Command{requestCmd, runCmd} {
		cmd.Flags().Duration("queue-timeout", 0, "maximum wait for request capacity (e.g. 30s); 0 uses the configured request timeout")
		cmd.Flags().Bool("require-dry-run", false, "require a successful preview before admitting a review request")
		cmd.Flags().Duration("dry-run-timeout", 0, "shorten the 30s preview budget (e.g. 5s); 0 uses the default")
		cmd.Long += "\n\nWhen rate_limit_action=queue, wait for capacity before creating a request.\nNo request ID or pending row exists until admission. --queue-timeout bounds\nthis wait separately from --timeout, which bounds waiting for approval."
		cmd.Long += "\n\nWhen general.enable_dry_run=true, supported commands collect bounded,\nredacted preview evidence before admission. Previews are advisory, never\napprovals or a sandbox for external tools. --require-dry-run refuses a review\nrequest if its preview fails, times out, is unsupported, or is disabled.\nSAFE commands still skip review. --dry-run-timeout can shorten the 30s budget;\nthe admission/queue deadline also bounds preview collection."
	}
}

func requestPreflightFlags(cmd *cobra.Command, opts core.CreateRequestOptions) (core.CreateRequestOptions, error) {
	if cmd.Flags().Lookup("require-dry-run") != nil {
		required, err := cmd.Flags().GetBool("require-dry-run")
		if err != nil {
			return opts, err
		}
		opts.RequireDryRun = opts.RequireDryRun || required
	}
	if cmd.Flags().Lookup("dry-run-timeout") != nil {
		timeout, err := cmd.Flags().GetDuration("dry-run-timeout")
		if err != nil {
			return opts, err
		}
		if timeout != 0 {
			opts.DryRunTimeout = timeout
		}
	}
	if opts.DryRunTimeout < 0 || opts.DryRunTimeout > 30*time.Second {
		return opts, errors.New("--dry-run-timeout must be between 0 and 30s")
	}
	return opts, nil
}

func requestQueueTimeout(cmd *cobra.Command) (time.Duration, error) {
	// Direct RunE callers may supply a bare command without registered flags.
	if cmd.Flags().Lookup("queue-timeout") == nil {
		return 0, nil
	}
	duration, err := cmd.Flags().GetDuration("queue-timeout")
	if err != nil {
		return 0, err
	}
	if duration < 0 {
		return 0, errors.New("--queue-timeout must not be negative")
	}
	return duration, nil
}

// Keep cancellation alive through admission, approval waiting AND execution.
// Stopping signal handling immediately after admission could lose an interrupt
// that arrived while the request was being committed, then execute it anyway.
func beginRequestCommand(cmd *cobra.Command) (func(), error) {
	if _, err := requestQueueTimeout(cmd); err != nil {
		return nil, err
	}
	if _, err := requestPreflightFlags(cmd, core.CreateRequestOptions{}); err != nil {
		return nil, err
	}
	previous := cmd.Context()
	parent := previous
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	cmd.SetContext(ctx)
	return func() {
		stop()
		cmd.SetContext(previous)
	}, nil
}

func submitRequestWithCapacity(cmd *cobra.Command, creator *core.RequestCreator, opts core.CreateRequestOptions) (*core.CreateRequestResult, error) {
	timeout, err := requestQueueTimeout(cmd)
	if err != nil {
		return nil, err
	}
	opts, err = requestPreflightFlags(cmd, opts)
	if err != nil {
		return nil, err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	result, err := creator.CreateRequestContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	if result.RateLimit != nil && result.RateLimit.Action == core.RateLimitActionWarn && result.RateLimit.Message != "ok" {
		// Never pollute structured stdout, or turn a committed admission into a
		// reported failure merely because a best-effort warning cannot be written.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[slb] Rate limit warning: %s\n", result.RateLimit.Message)
	}
	return result, nil
}

func addRequestAdmissionMetadata(resp map[string]any, result *core.CreateRequestResult) {
	if result.Preflight != nil {
		resp["preflight"] = result.Preflight
	}
	if result.RateLimit != nil {
		resp["rate_limit"] = result.RateLimit
	}
	resp["queued"] = result.Queued
	if result.Queued {
		resp["queue_wait_ms"] = result.QueueWait.Milliseconds()
	}
}

func requestAdmissionFailureResponse(err error) map[string]any {
	code := "admission_failed"
	var limit *core.RateLimitError
	hasLimit := errors.As(err, &limit)
	switch {
	case errors.Is(err, context.Canceled):
		code = "admission_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "admission_timeout"
		if hasLimit {
			code = "queue_timeout"
		}
	case hasLimit:
		code = "rate_limit_exceeded"
	case errors.Is(err, core.ErrPreflightRequired):
		code = "preflight_required"
	}
	resp := map[string]any{
		"status": "request_failed", "code": code,
		"created": false, "executed": false, "error": err.Error(),
	}
	if hasLimit {
		details := map[string]any{
			"pending": limit.Pending, "max_pending": limit.MaxPending,
			"recent": limit.Recent, "max_per_minute": limit.MaxPerMinute,
		}
		if !limit.ResetAt.IsZero() {
			details["reset_at"] = limit.ResetAt.UTC().Format(time.RFC3339)
		}
		resp["rate_limit"] = details
	}
	return resp
}

func writeRequestAdmissionError(cmd *cobra.Command, err error) error {
	if executionOutputStructured() {
		out := output.New(output.Format(GetOutput()))
		if writeErr := out.Write(requestAdmissionFailureResponse(err)); writeErr != nil {
			return errors.Join(err, fmt.Errorf("writing admission failure: %w", writeErr))
		}
	} else {
		if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "[slb] Request not created: %v\n", err); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	return err
}
