package integrations

import "encoding/json"

// ClaudeHooksFile is a Claude Code settings.json fragment. The CLI merges this
// registration into the selected settings file without replacing other tools'
// configuration. Claude Code delivers the PreToolUse JSON payload on stdin.
type ClaudeHooksFile struct {
	Hooks ClaudeHooks `json:"hooks"`
}

type ClaudeHooks struct {
	PreToolUse []ClaudeHookMatcher `json:"PreToolUse"`
}

type ClaudeHookMatcher struct {
	Matcher string       `json:"matcher"`
	Hooks   []ClaudeHook `json:"hooks"`
}

type ClaudeHook struct {
	Type      string `json:"type"`
	Command   string `json:"command"`
	OnFailure string `json:"onFailure"`
}

// DefaultClaudeHooks describes the same native guard installed by slb hook
// install. The caller supplies its resolved command, including shell quoting,
// so previews do not quietly select a different executable through PATH.
func DefaultClaudeHooks(guardCommand string) ClaudeHooksFile {
	return ClaudeHooksFile{
		Hooks: ClaudeHooks{
			PreToolUse: []ClaudeHookMatcher{{
				Matcher: "Bash",
				Hooks:   []ClaudeHook{{Type: "command", Command: guardCommand, OnFailure: "block"}},
			}},
		},
	}
}

// MarshalClaudeHooks pretty-prints the settings fragment as JSON.
func MarshalClaudeHooks(h ClaudeHooksFile) ([]byte, error) {
	return json.MarshalIndent(h, "", "  ")
}
