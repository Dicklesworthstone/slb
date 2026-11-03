// Package output implements TOON format support via CLI wrapper.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// TOONBinaryName is the name older toon_rust installs use for the TOON
// encoder/decoder ("tru", chosen to avoid coreutils "tr"). It is searched
// first; see toonBinaryNames.
const TOONBinaryName = "tru"

// toonBinaryNames are the names searched for the toon_rust CLI, in order. The
// crate is "tru" (cargo install tru), and since toon_rust v0.2.0 the binary it
// installs is named "toon".
var toonBinaryNames = []string{TOONBinaryName, "toon"}

// toonInstallHint says how to get the toon_rust CLI. The crate is "tru"; the
// crates.io crate named toon_rust is a different project.
const toonInstallHint = `install with: cargo install tru (the toon_rust CLI, binary "toon") or set TOON_TRU_BIN/TOON_BIN`

// findTOONBinary searches for the toon_rust CLI: the environment variables
// first, then each name in toonBinaryNames on PATH, then each name in the
// common locations. PATH comes first because it is what the user chose to run.
func findTOONBinary() (string, error) {
	if envPath, err := toonBinaryFromEnv(); err != nil {
		return "", err
	} else if envPath != "" {
		return envPath, nil
	}

	for _, name := range toonBinaryNames {
		if path, err := exec.LookPath(name); err == nil && isToonRustBinary(path) {
			return path, nil
		}
	}

	home, _ := os.UserHomeDir()
	commonDirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		"/usr/local/bin",
		// Development locations
		"/data/projects/toon_rust/target/release",
		"/data/projects/toon_rust/target/debug",
	}

	for _, name := range toonBinaryNames {
		for _, dir := range commonDirs {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); err == nil && isToonRustBinary(path) {
				return path, nil
			}
		}
	}

	return "", fmt.Errorf("toon_rust CLI not found (looked for tru and toon); %s", toonInstallHint)
}

// TOONAvailable returns true if the TOON binary is available.
func TOONAvailable() bool {
	_, err := findTOONBinary()
	return err == nil
}

// EncodeTOON encodes data to TOON format using the CLI wrapper.
func EncodeTOON(data any) (string, error) {
	binPath, err := findTOONBinary()
	if err != nil {
		return "", err
	}

	// First marshal to JSON
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("JSON marshal failed: %w", err)
	}

	// Call the toon_rust CLI to encode
	cmd := exec.Command(binPath, "-e")
	cmd.Stdin = bytes.NewReader(jsonBytes)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("toon_rust encode failed: %s: %w", stderr.String(), err)
	}

	return stdout.String(), nil
}

// DecodeTOON decodes TOON format to data using the CLI wrapper.
func DecodeTOON(toonStr string) (any, error) {
	binPath, err := findTOONBinary()
	if err != nil {
		return nil, err
	}

	// Call the toon_rust CLI to decode
	cmd := exec.Command(binPath, "-d")
	cmd.Stdin = bytes.NewReader([]byte(toonStr))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("toon_rust decode failed: %s: %w", stderr.String(), err)
	}

	// Parse the resulting JSON
	var result any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("JSON unmarshal failed: %w", err)
	}

	return result, nil
}

func toonBinaryFromEnv() (string, error) {
	for _, env := range []string{"TOON_TRU_BIN", "TOON_BIN", "TRU_PATH"} {
		if val := strings.TrimSpace(os.Getenv(env)); val != "" {
			if !isToonRustBinary(val) {
				return "", fmt.Errorf("%s=%q does not appear to be the toon_rust CLI (tru or toon); %s", env, val, toonInstallHint)
			}
			return val, nil
		}
	}
	return "", nil
}

// isToonRustBinary reports whether path is the toon_rust CLI. Identity comes
// from the binary's own output, never from its name alone, because the name
// "toon" is shared by toon_rust (since v0.2.0) and the Node.js toon-format CLI,
// which is not allowed as the TOON backend. toon_rust's --help says
// "reference implementation in Rust"; the Node.js CLI never does, and neither
// does coreutils `tr`.
func isToonRustBinary(path string) bool {
	helpOut, _ := exec.Command(path, "--help").CombinedOutput()
	helpLower := strings.ToLower(string(helpOut))
	if strings.Contains(helpLower, "reference implementation in rust") {
		return true
	}

	// A binary named "toon" is accepted only through the help text above, so
	// the Node.js CLI stays refused whatever its --version prints.
	if base := strings.ToLower(filepathBase(path)); base == "toon" || base == "toon.exe" {
		return false
	}

	verOut, _ := exec.Command(path, "--version").CombinedOutput()
	verLower := strings.ToLower(strings.TrimSpace(string(verOut)))
	// Accept "tru X.Y.Z" or "toon_rust X.Y.Z" version formats
	return strings.HasPrefix(verLower, "tru ") || strings.HasPrefix(verLower, "toon_rust ")
}

func filepathBase(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// writeTOON writes data in TOON format.
func (w *Writer) writeTOON(data any) error {
	toonStr, err := EncodeTOON(data)
	if err != nil {
		// Fall back to JSON if TOON encoding fails
		fmt.Fprintf(w.errOut, "warning: TOON encoding failed, falling back to JSON: %v\n", err)
		enc := json.NewEncoder(w.out)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	_, err = w.out.Write([]byte(toonStr))
	return err
}
