// Package cli provides CLI attachment collection helpers.
package cli

import (
	"context"
	"fmt"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// AttachmentFlags holds the attachment-related CLI flags.
type AttachmentFlags struct {
	Files       []string
	Contexts    []string
	Screenshots []string
}

// CollectAttachments loads and processes attachments from CLI flags.
// It returns a slice of attachments ready to be included in a request.
func CollectAttachments(ctx context.Context, flags AttachmentFlags) ([]db.Attachment, error) {
	config := core.DefaultAttachmentConfig()
	var attachments []db.Attachment

	// Process file attachments
	for _, path := range flags.Files {
		attachment, err := core.LoadAttachmentFromFile(path, &config)
		if err != nil {
			return nil, fmt.Errorf("loading file %q: %w", path, err)
		}
		attachments = append(attachments, *attachment)
	}

	// Process context command attachments
	for _, cmd := range flags.Contexts {
		attachment, err := core.RunContextCommand(ctx, cmd, &config)
		if err != nil {
			return nil, fmt.Errorf("running context command %q: %w", cmd, err)
		}
		attachments = append(attachments, *attachment)
	}

	// Process screenshot attachments
	for _, path := range flags.Screenshots {
		attachment, err := core.LoadScreenshot(path, &config)
		if err != nil {
			return nil, fmt.Errorf("loading screenshot %q: %w", path, err)
		}
		attachments = append(attachments, *attachment)
	}

	return attachments, nil
}
