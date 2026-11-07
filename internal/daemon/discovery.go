package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode"
	"unicode/utf8"
)

const (
	runtimeRecordName     = "daemon.json"
	maxRuntimeRecordBytes = 8192
	// Leave room for the terminating NUL on macOS/BSD as well as Linux.
	maxUnixSocketPathBytes = 103
)

// ErrDiscoveryInvalid means that project discovery could not be trusted. It
// must never trigger probing other sockets, projects, or temporary directories.
var ErrDiscoveryInvalid = errors.New("invalid project daemon discovery")

// RuntimePaths binds a published listener to its physical project and process.
// Version is zero only when no record exists and the exact project default is
// selected. Even that listener must identify its project before receiving RPCs.
type RuntimePaths struct {
	Version     int    `json:"version"`
	ProjectPath string `json:"project_path"`
	SocketPath  string `json:"socket_path"`
	PIDFile     string `json:"pid_file"`
	PID         int    `json:"pid"`
}

// DiscoverRuntime reads the daemon's published paths, independently of the
// caller's TMPDIR. A missing record permits only the exact historical project
// default; an invalid record returns an error, never a fallback destination.
// On error the returned paths contain defaults, never untrusted record fields.
func DiscoverRuntime(cwd string) (RuntimePaths, error) {
	paths := RuntimePaths{SocketPath: SocketPathForCWD(cwd), PIDFile: PIDFileForCWD(cwd)}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return paths, fmt.Errorf("%w: project directory unavailable", ErrDiscoveryInvalid)
	}
	project, err := PhysicalProjectRootForCWD(abs)
	if err != nil {
		return paths, fmt.Errorf("%w: project directory unavailable", ErrDiscoveryInvalid)
	}
	paths.ProjectPath = project
	record, _, err := readRuntimeRecord(project)
	if os.IsNotExist(err) {
		return paths, nil
	}
	if err != nil {
		return paths, fmt.Errorf("%w: %w", ErrDiscoveryInvalid, err)
	}
	return record, nil
}

// Refresh only within the physical project selected at construction. If its
// marker disappears or its path is redirected, ancestor discovery must not
// silently move a reusable client into a different project's daemon.
func refreshRuntime(previous RuntimePaths) (RuntimePaths, error) {
	if previous.ProjectPath == "" {
		return previous, fmt.Errorf("%w: original project directory unavailable", ErrDiscoveryInvalid)
	}
	paths, err := DiscoverRuntime(previous.ProjectPath)
	if paths.ProjectPath != previous.ProjectPath {
		return previous, fmt.Errorf("%w: original project identity changed", ErrDiscoveryInvalid)
	}
	return paths, err
}

func readRuntimeRecord(project string) (RuntimePaths, os.FileInfo, error) {
	var record RuntimePaths
	directory := filepath.Join(project, ".slb")
	if err := validateDiscoveryDirectory(directory); err != nil {
		return record, nil, err
	}
	path := filepath.Join(directory, runtimeRecordName)
	file, info, err := openDiscoveryFile(path, os.O_RDONLY)
	if err != nil {
		if os.IsNotExist(err) {
			return record, nil, err
		}
		return record, nil, errors.New("discovery record is not a private owned regular file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeRecordBytes+1))
	if err != nil || len(data) > maxRuntimeRecordBytes || !utf8.Valid(data) {
		return record, nil, errors.New("discovery record is unreadable, invalid UTF-8, or too large")
	}
	// Decode the object explicitly to reject duplicate keys as well as unknown
	// fields and trailing JSON. Go and the standalone Python hook agree here.
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return record, nil, errors.New("discovery record must be one JSON object")
	}
	fields := make(map[string]json.RawMessage, 5)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return record, nil, errors.New("malformed discovery record")
		}
		key, ok := token.(string)
		if !ok || fields[key] != nil {
			return record, nil, errors.New("duplicate discovery record field")
		}
		switch key {
		case "version", "project_path", "socket_path", "pid_file", "pid":
		default:
			return record, nil, errors.New("unknown discovery record field")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return record, nil, errors.New("malformed discovery record field")
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return record, nil, errors.New("malformed discovery record")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || len(fields) != 5 || json.Unmarshal(data, &record) != nil {
		return record, nil, errors.New("malformed discovery record")
	}
	if record.Version != 1 || record.ProjectPath != project || record.PID <= 0 ||
		!validRuntimePath(record.SocketPath) || len(record.SocketPath) > maxUnixSocketPathBytes ||
		!validRuntimePath(record.PIDFile) || record.PIDFile == record.SocketPath {
		return RuntimePaths{}, nil, errors.New("discovery record identity or paths are invalid")
	}
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(info, current) {
		return RuntimePaths{}, nil, errors.New("discovery record changed while reading")
	}
	return record, info, nil
}

func validRuntimePath(path string) bool {
	if !utf8.ValidString(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validateDiscoveryDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || !ownedByCurrentUser(path, info) {
		return errors.New("discovery directory must be an owned directory, not a symlink")
	}
	if err := validateRuntimePermissions(path, info, false); err != nil {
		return err
	}
	return validateRuntimeParents(filepath.Dir(path))
}

func validateRuntimeSocket(path string) (os.FileInfo, error) {
	if !validRuntimePath(path) || len(path) > maxUnixSocketPathBytes {
		return nil, errors.New("invalid Unix socket path (maximum 103 bytes)")
	}
	info, err := lstatOwnedPath(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(path, info) {
		return nil, errors.New("daemon endpoint must be a socket owned by the current user")
	}
	if err := validateRuntimePermissions(path, info, true); err != nil {
		return nil, err
	}
	if err := validateRuntimeParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return info, nil
}

// claimProjectRuntime prevents daemons with different socket/PID environments
// from owning the same project. Like other leases, daemon.lock is never removed.
func claimProjectRuntime(opts ServerOptions) (*fileLease, error) {
	directory := filepath.Join(opts.ProjectDir, ".slb")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := validateDiscoveryDirectory(directory); err != nil {
		return nil, err
	}
	lease, err := acquireFileLease(filepath.Join(directory, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	record, _, err := readRuntimeRecord(opts.ProjectDir)
	if err != nil && !os.IsNotExist(err) {
		_ = lease.Close()
		return nil, fmt.Errorf("%w: %w", ErrDiscoveryInvalid, err)
	}
	if err == nil && processAlive(record.PID) {
		_ = lease.Close()
		return nil, fmt.Errorf("%w: recorded daemon process is still alive", ErrDaemonOwned)
	}
	return lease, nil
}

// publishRuntime runs while the project lease is held, after service startup.
// Readers see either the complete old record or the complete new record. The
// returned cleanup preserves any record replaced by another daemon generation.
func publishRuntime(opts ServerOptions) (func() error, error) {
	directory := filepath.Join(opts.ProjectDir, ".slb")
	if err := validateDiscoveryDirectory(directory); err != nil {
		return nil, err
	}
	if _, err := validateRuntimeSocket(opts.SocketPath); err != nil {
		return nil, err
	}
	record := RuntimePaths{Version: 1, ProjectPath: opts.ProjectDir,
		SocketPath: opts.SocketPath, PIDFile: opts.PIDFile, PID: os.Getpid()}
	if !validRuntimePath(record.PIDFile) || record.PIDFile == record.SocketPath {
		return nil, errors.New("invalid daemon PID path")
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxRuntimeRecordBytes {
		return nil, errors.New("daemon discovery record exceeds size limit")
	}
	if _, _, err := readRuntimeRecord(opts.ProjectDir); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("preserving invalid discovery record: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	pending := filepath.Join(directory, ".daemon-"+hex.EncodeToString(nonce[:])+".tmp")
	file, original, err := openDiscoveryFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close(); _ = removeOwnedPath(pending, original) }()
	if _, err := file.Write(data); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, runtimeRecordName)
	if err := os.Rename(pending, path); err != nil {
		return nil, err
	}
	return func() error {
		current, info, err := readRuntimeRecord(opts.ProjectDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != record || !os.SameFile(original, info) {
			return errors.New("discovery record ownership changed; preserving replacement")
		}
		return removeOwnedPath(path, original)
	}, nil
}
