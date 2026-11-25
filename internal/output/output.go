// Package output implements consistent JSON output formatting for SLB.
// All JSON output uses snake_case keys as specified in the plan.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// Format represents the output format.
type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
	FormatTOON Format = "toon"
)

// Writer handles formatted output.
type Writer struct {
	format    Format
	out       io.Writer
	errOut    io.Writer
	showStats bool
}

// Option configures the Writer.
type Option func(*Writer)

// WithOutput sets the standard output writer.
func WithOutput(w io.Writer) Option {
	return func(wr *Writer) {
		wr.out = w
	}
}

// WithErrorOutput sets the error output writer.
func WithErrorOutput(w io.Writer) Option {
	return func(wr *Writer) {
		wr.errOut = w
	}
}

// WithStats enables token savings statistics output.
func WithStats(show bool) Option {
	return func(wr *Writer) {
		wr.showStats = show
	}
}

// New creates a new output writer.
func New(format Format, opts ...Option) *Writer {
	w := &Writer{
		format: format,
		out:    os.Stdout,
		errOut: os.Stderr,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Write outputs data in the configured format.
func (w *Writer) Write(data any) error {
	// Pre-compute JSON for stats if needed
	var jsonBytes []byte
	if w.showStats {
		var err error
		jsonBytes, err = json.Marshal(data)
		if err == nil {
			w.printStats(jsonBytes)
		}
	}

	switch w.format {
	case FormatJSON:
		enc := json.NewEncoder(w.out)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case FormatYAML:
		normalized, err := normalizeForYAML(data)
		if err != nil {
			return err
		}
		b, err := yaml.Marshal(normalized)
		if err != nil {
			return err
		}
		if len(b) == 0 || b[len(b)-1] != '\n' {
			b = append(b, '\n')
		}
		_, err = w.out.Write(b)
		return err
	case FormatText:
		// Human-friendly output goes to stderr to keep stdout clean for piping.
		_, err := fmt.Fprintf(w.errOut, "%v\n", data)
		return err
	case FormatTOON:
		return w.writeTOON(data)
	default:
		return fmt.Errorf("unsupported format: %s", w.format)
	}
}

// printStats outputs token savings comparison to stderr.
func (w *Writer) printStats(jsonBytes []byte) {
	jsonSize := len(jsonBytes)

	// Diagnostics only: a failed stderr write must not affect the command.
	if w.format == FormatTOON {
		// For TOON mode, show actual savings
		toonStr, err := EncodeTOON(json.RawMessage(jsonBytes))
		if err != nil {
			_, _ = fmt.Fprintf(w.errOut, "[slb-toon] JSON: %d bytes (TOON encoding failed)\n", jsonSize)
			return
		}
		toonSize := len(toonStr)
		savings := 0
		if jsonSize > 0 {
			savings = 100 - (toonSize * 100 / jsonSize)
		}
		_, _ = fmt.Fprintf(w.errOut, "[slb-toon] JSON: %d bytes, TOON: %d bytes (%d%% savings)\n", jsonSize, toonSize, savings)
	} else {
		// For JSON/YAML mode, show potential TOON savings
		if !TOONAvailable() {
			_, _ = fmt.Fprintf(w.errOut, "[slb-toon] JSON: %d bytes (TOON unavailable for comparison)\n", jsonSize)
			return
		}
		toonStr, err := EncodeTOON(json.RawMessage(jsonBytes))
		if err != nil {
			_, _ = fmt.Fprintf(w.errOut, "[slb-toon] JSON: %d bytes (TOON unavailable for comparison)\n", jsonSize)
			return
		}
		toonSize := len(toonStr)
		savings := 0
		if jsonSize > 0 {
			savings = 100 - (toonSize * 100 / jsonSize)
		}
		_, _ = fmt.Fprintf(w.errOut, "[slb-toon] JSON: %d bytes, TOON would be: %d bytes (%d%% potential savings)\n", jsonSize, toonSize, savings)
	}
}

// WriteNDJSON outputs data as NDJSON when in JSON mode (one JSON per line).
func (w *Writer) WriteNDJSON(data any) error {
	switch w.format {
	case FormatJSON:
		enc := json.NewEncoder(w.out)
		return enc.Encode(data)
	case FormatText:
		_, err := fmt.Fprintf(w.errOut, "%v\n", data)
		return err
	default:
		return fmt.Errorf("unsupported format: %s", w.format)
	}
}

// Success outputs a success message.
func (w *Writer) Success(msg string) {
	if w.format == FormatJSON || w.format == FormatYAML || w.format == FormatTOON {
		_ = w.Write(map[string]any{"status": "success", "message": msg})
	} else {
		_, _ = fmt.Fprintf(w.errOut, "✓ %s\n", msg)
	}
}

// Error outputs an error message.
func (w *Writer) Error(err error) {
	payload := ErrorPayload{
		Error:   "error",
		Message: err.Error(),
		Details: map[string]any{"code": 1},
	}
	switch w.format {
	case FormatJSON:
		_ = OutputJSONError(err, 1)
	case FormatTOON:
		// Use TOON encoding for error output
		_ = w.Write(payload)
	case FormatYAML:
		_ = OutputYAML(payload)
	default:
		_, _ = fmt.Fprintf(w.errOut, "✗ %s\n", err.Error())
	}
}

func normalizeForYAML(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var normalized any
	if err := dec.Decode(&normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

// OutputYAML writes YAML to stdout, preserving JSON tags/field names by converting via JSON first.
func OutputYAML(v any) error {
	normalized, err := normalizeForYAML(v)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(normalized)
	if err != nil {
		return err
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	_, err = os.Stdout.Write(b)
	return err
}
