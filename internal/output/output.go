// Package output implements consistent JSON output formatting for SLB.
// All JSON output uses snake_case keys as specified in the plan.
package output

import (
	"fmt"
	"io"
	"os"
)

// Format represents the output format.
type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
)

// Writer handles formatted output.
type Writer struct {
	format Format
	out    io.Writer
	errOut io.Writer
}

// New creates a new output writer.
func New(format Format) *Writer {
	return &Writer{
		format: format,
		out:    os.Stdout,
		errOut: os.Stderr,
	}
}

// Write outputs data in the configured format.
func (w *Writer) Write(data any) error {
	switch w.format {
	case FormatJSON:
		return OutputJSON(data)
	case FormatText:
		// Human-friendly output goes to stderr to keep stdout clean for piping.
		_, err := fmt.Fprintf(w.errOut, "%v\n", data)
		return err
	default:
		return fmt.Errorf("unsupported format: %s", w.format)
	}
}

// WriteNDJSON outputs data as NDJSON when in JSON mode (one JSON per line).
func (w *Writer) WriteNDJSON(data any) error {
	switch w.format {
	case FormatJSON:
		return OutputNDJSON(data)
	case FormatText:
		_, err := fmt.Fprintf(w.errOut, "%v\n", data)
		return err
	default:
		return fmt.Errorf("unsupported format: %s", w.format)
	}
}

// Success outputs a success message.
func (w *Writer) Success(msg string) {
	if w.format == FormatJSON {
		_ = w.Write(map[string]any{"status": "success", "message": msg})
	} else {
		fmt.Fprintf(w.errOut, "✓ %s\n", msg)
	}
}

// Error outputs an error message.
func (w *Writer) Error(err error) {
	if w.format == FormatJSON {
		_ = OutputJSONError(err, 1)
	} else {
		fmt.Fprintf(w.errOut, "✗ %s\n", err.Error())
	}
}
