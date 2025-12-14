package output

import (
	"encoding/json"
	"os"
)

// ErrorPayload is the canonical JSON error shape.
type ErrorPayload struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(out *os.File, v any, pretty bool) error {
	enc := json.NewEncoder(out)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// OutputJSON writes pretty-printed JSON to stdout.
func OutputJSON(v any) error {
	return writeJSON(os.Stdout, v, true)
}

// OutputNDJSON writes a single-line JSON object to stdout (NDJSON).
func OutputNDJSON(v any) error {
	return writeJSON(os.Stdout, v, false)
}

// OutputJSONError writes a structured error payload to stdout.
// The numeric code is included in details for machine handling.
func OutputJSONError(err error, code int) error {
	return OutputJSON(ErrorPayload{
		Error:   "error",
		Message: err.Error(),
		Details: map[string]any{"code": code},
	})
}

