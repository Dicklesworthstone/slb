package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

func init() { rootCmd.AddCommand(newEventsCommand()) }

// Each command owns its flags, so repeated embedded invocations cannot inherit
// another consumer's cursor. Unlike watch's current-state feed, this endpoint
// replays every journaled mutation, including intermediate and deleted states.
func newEventsCommand() *cobra.Command {
	opts := core.DefaultRequestEventStreamOptions()
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Replay or follow durable request lifecycle events",
		Long: `Read a project's durable request journal as newline-delimited JSON.

By default, print one page (100 events) from the beginning, then a checkpoint.
Use --after with an opaque cursor from a COMPLETELY processed event/checkpoint
for the next page or to resume after a disconnect. --follow drains backlog and
continues polling; --tail starts after the current head instead of replaying it.
Each event carries a resume cursor. A checkpoint's has_more reports page backlog.
Consumers must handle replay idempotently and save cursors only AFTER processing.

Examples:
  slb events --limit 50
  slb events --after "$CURSOR" --follow
  slb events --tail --follow

The journal records metadata, not commands, attachments, comments, credentials,
preview output or execution receipts. request_baseline is the state observed
when an existing database was upgraded, not a reconstruction of older history.
Events and review counts are NOT execution authority; use slb execute's gate.

This command opens the existing local database selected by --db (or the current
project), applies pending schema migrations, and reads without needing a daemon.
It never switches to another database on an invalid cursor. It always emits
NDJSON, regardless of the global display format. History is retained; this
command does not prune records or acknowledge work on behalf of a consumer.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.Validate(); err != nil {
				return err
			}
			parent := cmd.Context()
			if parent == nil {
				parent = context.Background()
			}
			ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := ctx.Err(); err != nil {
				return err
			}
			project, err := projectPath()
			if err != nil {
				return err
			}
			path := GetDB()
			// A read command must not silently initialize a new empty project.
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("opening existing request journal (run slb init first): %w", err)
			}
			database, err := db.OpenWithOptions(path, db.OpenOptions{})
			if err != nil {
				return err
			}
			defer database.Close()
			if err := database.ApplyMigrations(ctx); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			stopOutput := interruptRequestEventOutput(ctx, out)
			defer stopOutput()
			err = core.StreamRequestEvents(ctx, database, project, opts, out)
			if opts.Follow && errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opts.After, "after", "", "resume strictly after this opaque event cursor")
	cmd.Flags().BoolVar(&opts.Follow, "follow", false, "drain backlog and follow new journal events")
	cmd.Flags().BoolVar(&opts.Tail, "tail", false, "start at the current head; cannot be combined with --after")
	cmd.Flags().IntVar(&opts.Limit, "limit", opts.Limit, "maximum events per page (1-1000)")
	cmd.Flags().DurationVar(&opts.PollInterval, "poll-interval", opts.PollInterval, "polling interval once caught up")
	return cmd
}

// Interrupt a pollable stdout pipe/socket without closing a caller-owned file.
// Arbitrary embedded writers must provide their own cancellation mechanism.
// Join the callback before clearing the deadline so cancellation from an old
// command cannot poison a later invocation using the same output stream.
func interruptRequestEventOutput(ctx context.Context, out io.Writer) func() {
	w, ok := out.(interface{ SetWriteDeadline(time.Time) error })
	if !ok || w.SetWriteDeadline(time.Time{}) != nil {
		return func() {}
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = w.SetWriteDeadline(time.Now())
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
		_ = w.SetWriteDeadline(time.Time{})
	}
}
