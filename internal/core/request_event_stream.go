package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// RequestEventStreamOptions controls replay, not command execution. Consumers
// checkpoint an event's opaque cursor AFTER their own processing succeeds.
// Re-delivery after a consumer crash is possible; processing must be idempotent.
type RequestEventStreamOptions struct {
	After        string
	Tail         bool
	Follow       bool
	Limit        int
	PollInterval time.Duration
}

func DefaultRequestEventStreamOptions() RequestEventStreamOptions {
	return RequestEventStreamOptions{Limit: db.DefaultRequestEventLimit, PollInterval: 250 * time.Millisecond}
}

func (opts RequestEventStreamOptions) Validate() error {
	if opts.Tail && opts.After != "" {
		return errors.New("--tail and --after are mutually exclusive")
	}
	if opts.Limit < 1 || opts.Limit > db.MaxRequestEventLimit {
		return fmt.Errorf("--limit must be between 1 and %d", db.MaxRequestEventLimit)
	}
	if opts.PollInterval <= 0 {
		return errors.New("--poll-interval must be positive")
	}
	return nil
}

type requestEventCheckpoint struct {
	Version     int    `json:"version"`
	Event       string `json:"event"`
	ProjectPath string `json:"project_path"`
	Cursor      string `json:"cursor"`
	HasMore     bool   `json:"has_more"`
}

// StreamRequestEvents writes ordered NDJSON directly from the durable journal.
// Without Follow, emit ONE bounded page and its checkpoint. With Follow, drain
// backlog pages immediately and poll only after catching up. No channel or
// in-memory event queue can overflow, and no read transaction spans an output
// write or a wait. Slow consumers leave their backlog safely in the database.
//
// Output errors stop immediately: there is no newer checkpoint after a partial
// write. The caller owns out and must arrange interruption for blocking writers;
// context cancellation is checked between writes, reads and waits. This method
// never silently resets an invalid cursor or replays/executes a command.
func StreamRequestEvents(ctx context.Context, database *db.DB, project string, opts RequestEventStreamOptions, out io.Writer) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if database == nil || out == nil {
		return errors.New("request event database and output are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cursor := opts.After
	if opts.Tail {
		var err error
		cursor, err = database.RequestEventHead(ctx, project)
		if err != nil {
			return err
		}
	}
	lastCheckpoint := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := database.ReadRequestEvents(ctx, project, cursor, opts.Limit)
		if err != nil {
			return fmt.Errorf("reading durable request events: %w", err)
		}
		for _, event := range page.Events {
			if err := writeRequestEventRecord(ctx, out, event); err != nil {
				return err
			}
		}
		if !opts.Follow || page.Cursor != lastCheckpoint {
			if err := writeRequestEventRecord(ctx, out, requestEventCheckpoint{
				Version: 1, Event: "checkpoint", ProjectPath: project, Cursor: page.Cursor, HasMore: page.HasMore,
			}); err != nil {
				return err
			}
			lastCheckpoint = page.Cursor
		}
		cursor = page.Cursor
		if !opts.Follow {
			return nil
		}
		if page.HasMore {
			continue
		}
		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func writeRequestEventRecord(ctx context.Context, out io.Writer, record any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding request event: %w", err)
	}
	data = append(data, '\n')
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return fmt.Errorf("writing request event: %w", err)
	}
	return nil
}
