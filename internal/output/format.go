package output

import "sync/atomic"

// OutputMode is the global output mode used by convenience helpers.
// Prefer passing an explicit Format/Writer when possible.
type OutputMode string

const (
	OutputModeText OutputMode = "text"
	OutputModeJSON OutputMode = "json"
)

var outputMode atomic.Value

func init() {
	outputMode.Store(OutputModeText)
}

// SetOutputMode sets the global output mode.
func SetOutputMode(json bool) {
	if json {
		outputMode.Store(OutputModeJSON)
		return
	}
	outputMode.Store(OutputModeText)
}

// GetOutputMode returns the current global output mode.
func GetOutputMode() OutputMode {
	if v, ok := outputMode.Load().(OutputMode); ok {
		return v
	}
	return OutputModeText
}

// IsJSON returns true if the global output mode is JSON.
func IsJSON() bool {
	return GetOutputMode() == OutputModeJSON
}

