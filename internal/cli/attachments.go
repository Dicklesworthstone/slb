// Package cli provides CLI attachment collection helpers.
package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// AttachmentFlags holds the attachment-related CLI flags.
type AttachmentFlags struct {
	Files       []string
	Contexts    []string
	Screenshots []string
}

// AttachmentCommandScope binds evidence collection to the same requester,
// project, working directory and selected policy as the requested operation.
// File/image attachments do not execute commands and need no scope.
type AttachmentCommandScope struct {
	Database         *db.DB
	ProjectPath      string
	WorkingDirectory string
	SessionID        string
	ConfigPath       string
}

// CollectAttachments loads and processes attachments from CLI flags.
// It returns a slice of attachments ready to be included in a request.
func CollectAttachments(ctx context.Context, flags AttachmentFlags, scope *AttachmentCommandScope) ([]db.Attachment, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	config := core.DefaultAttachmentConfig()
	var attachments []db.Attachment
	commands := make([]db.CommandSpec, len(flags.Contexts))
	if len(commands) > 0 {
		if scope == nil || scope.Database == nil {
			return nil, fmt.Errorf("context commands require a project database and requester scope")
		}
		config.WorkingDirectory = scope.WorkingDirectory
		// Check the whole batch before launching any command. A later unsafe
		// attachment must not turn an otherwise refused request into execution.
		for i, command := range flags.Contexts {
			if strings.TrimSpace(command) == "" {
				return nil, fmt.Errorf("context command %d is empty", i+1)
			}
			commands[i] = db.CommandSpec{Raw: command, Cwd: scope.WorkingDirectory, Shell: true}
			if err := core.CheckSafeExecution(ctx, scope.Database, commands[i], scope.ProjectPath, scope.SessionID, scope.ConfigPath); err != nil {
				return nil, fmt.Errorf("checking context command %d: %w", i+1, err)
			}
		}
	}

	// Process file attachments
	for _, path := range flags.Files {
		attachment, err := core.LoadAttachmentFromFile(path, &config)
		if err != nil {
			return nil, fmt.Errorf("loading file %q: %w", path, err)
		}
		attachments = append(attachments, *attachment)
	}

	// Validate screenshots before any subprocess starts as well. Retain the
	// existing file/context/screenshot output order after collection succeeds.
	var screenshots []db.Attachment
	for _, path := range flags.Screenshots {
		attachment, err := core.LoadScreenshot(path, &config)
		if err != nil {
			return nil, fmt.Errorf("loading screenshot %q: %w", path, err)
		}
		screenshots = append(screenshots, *attachment)
	}

	// Process context command attachments
	for i, command := range commands {
		// Earlier context programs may have changed policy or session state.
		// A successful precheck is not a reusable permission for the next one.
		if err := core.CheckSafeExecution(ctx, scope.Database, command, scope.ProjectPath, scope.SessionID, scope.ConfigPath); err != nil {
			return nil, fmt.Errorf("rechecking context command %d: %w", i+1, err)
		}
		attachment, err := core.RunContextCommand(ctx, command.Raw, &config)
		if err != nil {
			return nil, fmt.Errorf("running context command %d: %w", i+1, err)
		}
		attachment.Metadata["policy_check"] = "safe"
		attachments = append(attachments, *attachment)
	}

	attachments = append(attachments, screenshots...)

	return attachments, nil
}
