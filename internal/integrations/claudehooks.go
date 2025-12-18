package integrations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeHooksFile is the .claude/hooks.json schema we manage.
type ClaudeHooksFile struct {
	Hooks ClaudeHooks `json:"hooks"`
}

type ClaudeHooks struct {
	PreBash *ClaudeHook `json:"pre_bash,omitempty"`
}

type ClaudeHook struct {
	Command  string            `json:"command"`
	Input    map[string]string `json:"input,omitempty"`
	OnBlock  *ClaudeOnBlock     `json:"on_block,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

type ClaudeOnBlock struct {
	Message string `json:"message"`
}

// DefaultClaudeHooks returns the default SLB pre_bash hook configuration.
func DefaultClaudeHooks() ClaudeHooksFile {
	return ClaudeHooksFile{
		Hooks: ClaudeHooks{
			PreBash: &ClaudeHook{
				Command: `slb patterns test --exit-code`,
				Input: map[string]string{
					"command": "${COMMAND}",
				},
				OnBlock: &ClaudeOnBlock{
					Message: `This command requires slb approval. Use: slb request "${COMMAND}" --reason "..." --expected-effect "..." --goal "..." --safety "..."`,
				},
			},
		},
	}
}

// MarshalClaudeHooks pretty-prints the hooks file as JSON.
func MarshalClaudeHooks(h ClaudeHooksFile) ([]byte, error) {
	return json.MarshalIndent(h, "", "  ")
}

// InstallClaudeHooks writes (or merges) `.claude/hooks.json` under projectPath.
func InstallClaudeHooks(projectPath string, merge bool) (path string, merged bool, err error) {
	dir := filepath.Join(projectPath, ".claude")
	path = filepath.Join(dir, "hooks.json")

	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", false, fmt.Errorf("creating .claude directory: %w", err)
	}

	desired := DefaultClaudeHooks()

	// If not merging, just overwrite.
	if !merge {
		data, err := MarshalClaudeHooks(desired)
		if err != nil {
			return "", false, err
		}
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			return "", false, fmt.Errorf("writing hooks.json: %w", err)
		}
		return path, false, nil
	}

	// Merge with existing if present.
	existingData, err := os.ReadFile(path)
	if err != nil {
		// If file doesn't exist, fall back to a straight write.
		if os.IsNotExist(err) {
			data, err := MarshalClaudeHooks(desired)
			if err != nil {
				return "", false, err
			}
			if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
				return "", false, fmt.Errorf("writing hooks.json: %w", err)
			}
			return path, false, nil
		}
		return "", false, fmt.Errorf("reading existing hooks.json: %w", err)
	}

	var root map[string]any
	if err := json.Unmarshal(existingData, &root); err != nil {
		return "", false, fmt.Errorf("parsing existing hooks.json: %w", err)
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}

	// Replace/insert only the pre_bash hook we manage; preserve all other keys.
	desiredPreBash := map[string]any{}
	if desired.Hooks.PreBash != nil {
		b, _ := json.Marshal(desired.Hooks.PreBash)
		_ = json.Unmarshal(b, &desiredPreBash)
	}
	hooks["pre_bash"] = desiredPreBash

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", false, fmt.Errorf("marshaling merged hooks.json: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0644); err != nil {
		return "", false, fmt.Errorf("writing merged hooks.json: %w", err)
	}

	return path, true, nil
}

