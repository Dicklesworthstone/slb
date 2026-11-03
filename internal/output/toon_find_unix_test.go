//go:build !windows

package output

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Texts printed by the real encoders: toon_rust 0.2.5 (crate "tru", binary
// "toon"), an older toon_rust install named "tru", and the Node.js
// @toon-format/cli 4.4.0 (binary "toon").
const (
	rustToonHelp    = "TOON reference implementation in Rust (JSON <-> TOON)\n\nUsage: toon [OPTIONS] [INPUT]"
	rustToonVersion = "toon 0.2.5"
	// Version string per v0.1.2, the last "tru" release; help text abbreviated
	// (without its Rust about line, so the version check is what accepts it).
	oldTruHelp      = "Usage: tru [OPTIONS] [INPUT]"
	oldTruVersion   = "tru 0.1.2"
	nodeToonHelp    = "toon v4.4.0\nTOON CLI – Convert between JSON and TOON\n\nUSAGE  toon [OPTIONS] [INPUT]"
	nodeToonVersion = "4.4.0"
)

// writeFakeEncoder writes an executable shell script named name into dir that
// prints help for --help, version for --version and encoded for -e.
func writeFakeEncoder(t *testing.T, dir, name, help, version, encoded string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"--help) printf '%s\\n' " + shellQuote(help) + " ;;\n" +
		"--version) printf '%s\\n' " + shellQuote(version) + " ;;\n" +
		"-e) /bin/cat >/dev/null; printf '%s\\n' " + shellQuote(encoded) + " ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake encoder: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isolateTOONSearch confines the encoder search: PATH is pathDir, HOME is a new
// temp dir (returned), and TOON_TRU_BIN, TOON_BIN and TRU_PATH are empty. The
// search also looks in fixed system locations; if one of them holds a tru or
// toon on this host, the test is skipped, since the search could find it.
func isolateTOONSearch(t *testing.T, pathDir string) string {
	t.Helper()
	for _, dir := range []string{"/usr/local/bin", "/data/projects/toon_rust/target/release", "/data/projects/toon_rust/target/debug"} {
		for _, name := range []string{"tru", "toon"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				t.Skipf("%s exists on this host, and the encoder search would find it", filepath.Join(dir, name))
			}
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", pathDir)
	for _, env := range []string{"TOON_TRU_BIN", "TOON_BIN", "TRU_PATH"} {
		t.Setenv(env, "")
	}
	return home
}

func requireFound(t *testing.T, want string) {
	t.Helper()
	got, err := findTOONBinary()
	if err != nil {
		t.Fatalf("findTOONBinary: %v", err)
	}
	if got != want {
		t.Fatalf("findTOONBinary = %q, want %q", got, want)
	}
}

func TestFindTOONBinary_AcceptsRustToonOnPath(t *testing.T) {
	bin := t.TempDir()
	want := writeFakeEncoder(t, bin, "toon", rustToonHelp, rustToonVersion, "")
	isolateTOONSearch(t, bin)
	requireFound(t, want)
}

func TestFindTOONBinary_AcceptsRustToonInHomeLocalBin(t *testing.T) {
	home := isolateTOONSearch(t, t.TempDir())
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeFakeEncoder(t, dir, "toon", rustToonHelp, rustToonVersion, "")
	requireFound(t, want)
}

func TestFindTOONBinary_AcceptsTruByVersion(t *testing.T) {
	bin := t.TempDir()
	want := writeFakeEncoder(t, bin, "tru", oldTruHelp, oldTruVersion, "")
	isolateTOONSearch(t, bin)
	requireFound(t, want)
}

func TestFindTOONBinary_PrefersTruOverToon(t *testing.T) {
	bin := t.TempDir()
	want := writeFakeEncoder(t, bin, "tru", oldTruHelp, oldTruVersion, "")
	writeFakeEncoder(t, bin, "toon", rustToonHelp, rustToonVersion, "")
	isolateTOONSearch(t, bin)
	requireFound(t, want)
}

func TestFindTOONBinary_PathToonBeatsCommonDirTru(t *testing.T) {
	bin := t.TempDir()
	want := writeFakeEncoder(t, bin, "toon", rustToonHelp, rustToonVersion, "")
	home := isolateTOONSearch(t, bin)
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeEncoder(t, dir, "tru", oldTruHelp, oldTruVersion, "")
	requireFound(t, want)
}

func TestIsToonRustBinary_RejectsNodeToon(t *testing.T) {
	path := writeFakeEncoder(t, t.TempDir(), "toon", nodeToonHelp, nodeToonVersion, "")
	if isToonRustBinary(path) {
		t.Fatalf("isToonRustBinary accepted the Node.js toon CLI at %s", path)
	}
}

func TestIsToonRustBinary_ToonNameNeedsRustHelp(t *testing.T) {
	// A binary named toon that claims a tru version but lacks toon_rust's help
	// text is refused: for that name only the help text counts.
	path := writeFakeEncoder(t, t.TempDir(), "toon", nodeToonHelp, oldTruVersion, "")
	if isToonRustBinary(path) {
		t.Fatalf("isToonRustBinary accepted a toon binary by its version alone: %s", path)
	}
}

func TestFindTOONBinary_NodeToonOnlyIsNotFound(t *testing.T) {
	bin := t.TempDir()
	writeFakeEncoder(t, bin, "toon", nodeToonHelp, nodeToonVersion, "")
	isolateTOONSearch(t, bin)
	got, err := findTOONBinary()
	if err == nil {
		t.Fatalf("findTOONBinary = %q with only the Node.js toon CLI on PATH, want an error", got)
	}
	for _, want := range []string{"cargo install tru", `binary "toon"`, "TOON_TRU_BIN/TOON_BIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "cargo install toon_rust") {
		t.Errorf("error %q names the unrelated toon_rust crate", err)
	}
}

func TestFindTOONBinary_TOONBinAcceptsRustToon(t *testing.T) {
	want := writeFakeEncoder(t, t.TempDir(), "toon", rustToonHelp, rustToonVersion, "")
	isolateTOONSearch(t, t.TempDir())
	t.Setenv("TOON_BIN", want)
	requireFound(t, want)
}

func TestFindTOONBinary_TOONBinRejectsNodeToon(t *testing.T) {
	node := writeFakeEncoder(t, t.TempDir(), "toon", nodeToonHelp, nodeToonVersion, "")
	isolateTOONSearch(t, t.TempDir())
	t.Setenv("TOON_BIN", node)
	got, err := findTOONBinary()
	if err == nil {
		t.Fatalf("findTOONBinary = %q with TOON_BIN at the Node.js toon CLI, want an error", got)
	}
	for _, want := range []string{"TOON_BIN=", "cargo install tru"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestWriter_TOONUsesRustToonBinary(t *testing.T) {
	bin := t.TempDir()
	writeFakeEncoder(t, bin, "toon", rustToonHelp, rustToonVersion, "name: Alice")
	isolateTOONSearch(t, bin)

	var out, errOut bytes.Buffer
	w := New(FormatTOON, WithOutput(&out), WithErrorOutput(&errOut))
	if err := w.Write(map[string]any{"name": "Alice"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := out.String(); got != "name: Alice\n" {
		t.Fatalf("TOON output = %q (stderr %q), want the encoder's output", got, errOut.String())
	}
	if strings.Contains(errOut.String(), "falling back to JSON") {
		t.Fatalf("Writer fell back to JSON: %q", errOut.String())
	}
}
