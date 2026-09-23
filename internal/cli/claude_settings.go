package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude Code's settings.json is shared by many tools (other PreToolUse
// guards, remote-build shims, formatters, settings-sync timers). SLB owns
// exactly one hook object inside it: the command that runs the SLB guard.
// Everything here edits that one object in place and leaves every other key,
// entry and sibling hook untouched, in its original order (GitHub #18).

// settingsObject is a JSON object that remembers its key order, so a
// read-modify-write cycle does not reorder a file other tools also diff.
type settingsObject struct {
	keys   []string
	values map[string]any
}

func newSettingsObject() *settingsObject {
	return &settingsObject{values: map[string]any{}}
}

func (o *settingsObject) get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

func (o *settingsObject) set(key string, value any) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// decodeSettingsJSON parses a settings document preserving object key order
// and number spelling. The document must be a single JSON object.
func decodeSettingsJSON(data []byte) (*settingsObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeSettingsValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected content after the top-level JSON object")
	}
	root, ok := value.(*settingsObject)
	if !ok {
		return nil, errors.New("top-level JSON value is not an object")
	}
	return root, nil
}

func decodeSettingsValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := newSettingsObject()
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				value, err := decodeSettingsValue(decoder)
				if err != nil {
					return nil, err
				}
				object.set(key, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			array := []any{}
			for decoder.More() {
				value, err := decodeSettingsValue(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return array, nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", token)
		}
	default:
		// string, json.Number, bool or nil.
		return token, nil
	}
}

// encodeSettingsJSON renders the document with the given indent unit, without
// HTML-escaping (hook commands routinely contain &, < and >).
func encodeSettingsJSON(root *settingsObject, indent string) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeSettingsValue(&buf, root, indent, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeSettingsValue(buf *bytes.Buffer, value any, indent string, depth int) error {
	newline := func(level int) {
		buf.WriteByte('\n')
		buf.WriteString(strings.Repeat(indent, level))
	}
	switch value := value.(type) {
	case *settingsObject:
		if len(value.keys) == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteByte('{')
		for i, key := range value.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			newline(depth + 1)
			if err := writeSettingsScalar(buf, key); err != nil {
				return err
			}
			buf.WriteString(": ")
			if err := writeSettingsValue(buf, value.values[key], indent, depth+1); err != nil {
				return err
			}
		}
		newline(depth)
		buf.WriteByte('}')
	case []any:
		if len(value) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			newline(depth + 1)
			if err := writeSettingsValue(buf, item, indent, depth+1); err != nil {
				return err
			}
		}
		newline(depth)
		buf.WriteByte(']')
	default:
		return writeSettingsScalar(buf, value)
	}
	return nil
}

func writeSettingsScalar(buf *bytes.Buffer, value any) error {
	var scalar bytes.Buffer
	encoder := json.NewEncoder(&scalar)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	buf.Write(bytes.TrimSuffix(scalar.Bytes(), []byte("\n")))
	return nil
}

// settingsIndent reuses the file's own indent unit (the leading whitespace
// of its first indented line), defaulting to two spaces.
func settingsIndent(data []byte) string {
	for _, line := range strings.Split(string(data), "\n")[1:] {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// claudeSettingsFile is a loaded settings.json plus what is needed to write it
// back in the same shape.
type claudeSettingsFile struct {
	path            string
	root            *settingsObject
	indent          string
	trailingNewline bool
	exists          bool
}

func loadClaudeSettings(path string) (*claudeSettingsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &claudeSettingsFile{path: path, root: newSettingsObject(), indent: "  ", trailingNewline: true}, nil
		}
		return nil, fmt.Errorf("failed to read settings: %w", err)
	}
	file := &claudeSettingsFile{path: path, exists: true, indent: settingsIndent(data),
		trailingNewline: bytes.HasSuffix(data, []byte("\n"))}
	if len(bytes.TrimSpace(data)) == 0 {
		file.root = newSettingsObject()
		return file, nil
	}
	root, err := decodeSettingsJSON(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse settings %s: %w", path, err)
	}
	file.root = root
	return file, nil
}

// save replaces the file atomically. A symlinked settings.json (dotfile
// managers) is written through to its target instead of being replaced by a
// regular file, and an existing file keeps its permissions.
func (f *claudeSettingsFile) save() error {
	data, err := encodeSettingsJSON(f.root, f.indent)
	if err != nil {
		return fmt.Errorf("failed to encode settings: %w", err)
	}
	if f.trailingNewline {
		data = append(data, '\n')
	}
	target := f.path
	if resolved, err := filepath.EvalSymlinks(f.path); err == nil {
		target = resolved
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to resolve settings path: %w", err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("failed to create settings directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".settings.json.slb-*")
	if err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write settings: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	committed = true
	return nil
}

// preToolUse returns hooks.PreToolUse. Missing or null sections are reported
// as absent; a section of the wrong JSON type is an error, because rewriting
// it would destroy configuration SLB does not understand.
func (f *claudeSettingsFile) preToolUse() (hooks *settingsObject, entries []any, present bool, err error) {
	rawHooks, ok := f.root.get("hooks")
	if !ok || rawHooks == nil {
		return nil, nil, false, nil
	}
	hooks, ok = rawHooks.(*settingsObject)
	if !ok {
		return nil, nil, false, errors.New(`settings "hooks" is not a JSON object; refusing to rewrite it`)
	}
	rawEntries, ok := hooks.get("PreToolUse")
	if !ok || rawEntries == nil {
		return hooks, nil, false, nil
	}
	entries, ok = rawEntries.([]any)
	if !ok {
		return nil, nil, false, errors.New(`settings "hooks.PreToolUse" is not a JSON array; refusing to rewrite it`)
	}
	return hooks, entries, true, nil
}

// matcherCoversBash reports whether a PreToolUse matcher selects the Bash
// tool. Claude Code treats an empty matcher or "*" as match-all and otherwise
// accepts an exact tool name or a regular expression such as "Bash|PowerShell".
func matcherCoversBash(matcher string) bool {
	switch matcher {
	case "", "*", "Bash":
		return true
	}
	re, err := regexp.Compile(`^(?:` + matcher + `)$`)
	return err == nil && re.MatchString("Bash")
}

// slbHookLocation identifies one SLB hook object inside hooks.PreToolUse.
type slbHookLocation struct {
	entry       *settingsObject
	entryIndex  int
	hookIndex   int
	hook        *settingsObject
	command     string
	coversBash  bool
	siblingHook int // number of non-SLB hooks sharing the entry
}

func findSLBHooks(entries []any, isSLB func(string) bool) []slbHookLocation {
	var found []slbHookLocation
	for i, rawEntry := range entries {
		entry, ok := rawEntry.(*settingsObject)
		if !ok {
			continue
		}
		matcher := ""
		if rawMatcher, ok := entry.get("matcher"); ok {
			if matcher, ok = rawMatcher.(string); !ok {
				continue
			}
		}
		rawList, _ := entry.get("hooks")
		list, ok := rawList.([]any)
		if !ok {
			continue
		}
		var here []slbHookLocation
		for j, rawHook := range list {
			hook, ok := rawHook.(*settingsObject)
			if !ok {
				continue
			}
			command, _ := hook.values["command"].(string)
			if command == "" || !isSLB(command) {
				continue
			}
			here = append(here, slbHookLocation{entry: entry, entryIndex: i, hookIndex: j, hook: hook,
				command: command, coversBash: matcherCoversBash(matcher)})
		}
		for k := range here {
			here[k].siblingHook = len(list) - len(here)
		}
		found = append(found, here...)
	}
	return found
}

// removeSLBHooks deletes the given hook objects. An entry is dropped only when
// the removal left its hooks list empty; entries that still hold other tools'
// hooks stay exactly where they were.
func removeSLBHooks(entries []any, remove []slbHookLocation) []any {
	if len(remove) == 0 {
		return entries
	}
	drop := map[*settingsObject]map[int]bool{}
	for _, location := range remove {
		if drop[location.entry] == nil {
			drop[location.entry] = map[int]bool{}
		}
		drop[location.entry][location.hookIndex] = true
	}
	kept := make([]any, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(*settingsObject)
		indexes := drop[entry]
		if !ok || indexes == nil {
			kept = append(kept, rawEntry)
			continue
		}
		rawList, _ := entry.get("hooks")
		list, _ := rawList.([]any)
		remaining := make([]any, 0, len(list))
		for j, hook := range list {
			if !indexes[j] {
				remaining = append(remaining, hook)
			}
		}
		if len(remaining) == 0 {
			continue
		}
		entry.set("hooks", remaining)
		kept = append(kept, entry)
	}
	return kept
}

// slbHookInstallResult describes what installSLBHook changed.
type slbHookInstallResult struct {
	changed           bool
	found             bool
	upgraded          bool
	previousCommand   string
	duplicatesRemoved int
	siblingHooks      int
	entryMatcher      string
}

// installSLBHook registers guardCommand as a PreToolUse hook for Bash,
// editing only SLB's own hook object:
//   - an existing SLB hook (native or legacy Python) in an entry that covers
//     Bash has its command updated in place; sibling hooks and any other keys
//     on the hook object (e.g. "timeout") are kept unless force is set;
//   - extra SLB hooks elsewhere are removed so there is exactly one guard;
//   - otherwise the hook is appended to the first "Bash" entry, or a new
//     "Bash" entry is appended when there is none.
func installSLBHook(f *claudeSettingsFile, guardCommand string, isSLB func(string) bool, force bool) (slbHookInstallResult, error) {
	var result slbHookInstallResult
	hooks, entries, _, err := f.preToolUse()
	if err != nil {
		return result, err
	}
	locations := findSLBHooks(entries, isSLB)
	primary := -1
	for i, location := range locations {
		if location.coversBash {
			primary = i
			break
		}
	}

	var duplicates []slbHookLocation
	for i, location := range locations {
		if i != primary {
			duplicates = append(duplicates, location)
		}
	}

	if primary >= 0 {
		location := locations[primary]
		result.found = true
		result.previousCommand = location.command
		result.upgraded = location.command != guardCommand
		result.siblingHooks = location.siblingHook
		result.entryMatcher, _ = location.entry.values["matcher"].(string)
		if force {
			canonical := newSettingsObject()
			canonical.set("type", "command")
			canonical.set("command", guardCommand)
			rawList, _ := location.entry.get("hooks")
			list := rawList.([]any)
			if !encodedEqual(list[location.hookIndex], canonical) {
				list[location.hookIndex] = canonical
				result.changed = true
			}
		} else {
			if location.hook.values["type"] != "command" {
				location.hook.set("type", "command")
				result.changed = true
			}
			if location.command != guardCommand {
				location.hook.set("command", guardCommand)
				result.changed = true
			}
		}
	}

	if len(duplicates) > 0 {
		entries = removeSLBHooks(entries, duplicates)
		result.duplicatesRemoved = len(duplicates)
		result.changed = true
		if !result.found {
			result.previousCommand = duplicates[0].command
			result.upgraded = true
		}
	}

	if primary < 0 {
		hook := newSettingsObject()
		hook.set("type", "command")
		hook.set("command", guardCommand)
		appended := false
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(*settingsObject)
			if !ok || entry.values["matcher"] != "Bash" {
				continue
			}
			rawList, present := entry.get("hooks")
			list, ok := rawList.([]any)
			if present && rawList != nil && !ok {
				continue
			}
			result.siblingHooks = len(list)
			entry.set("hooks", append(list, hook))
			appended = true
			break
		}
		if !appended {
			entry := newSettingsObject()
			entry.set("matcher", "Bash")
			entry.set("hooks", []any{hook})
			entries = append(entries, entry)
		}
		result.entryMatcher = "Bash"
		result.changed = true
	}

	if result.changed {
		if hooks == nil {
			hooks = newSettingsObject()
			f.root.set("hooks", hooks)
		}
		hooks.set("PreToolUse", entries)
	}
	return result, nil
}

// uninstallSLBHook removes every SLB hook object and nothing else.
func uninstallSLBHook(f *claudeSettingsFile, isSLB func(string) bool) (int, error) {
	hooks, entries, present, err := f.preToolUse()
	if err != nil || !present {
		return 0, err
	}
	locations := findSLBHooks(entries, isSLB)
	if len(locations) == 0 {
		return 0, nil
	}
	hooks.set("PreToolUse", removeSLBHooks(entries, locations))
	return len(locations), nil
}

func encodedEqual(a, b any) bool {
	var left, right bytes.Buffer
	if writeSettingsValue(&left, a, "", 0) != nil || writeSettingsValue(&right, b, "", 0) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
