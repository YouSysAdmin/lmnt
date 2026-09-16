package datadir

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yousysadmin/lmnt/internal/hostos"
)

// sessionDir is the subdirectory holding one record per running session.
const sessionDir = "sessions"

// Session describes a running lmnt session, so that other lmnt processes
// (the TUI) can list it and ask it to stop. The owner writes the record when
// its guest is up, rewrites it once the share is started, and deletes it on
// exit. Password is the share password: the record is 0600 and lives only as
// long as the session, but it is on disk.
type Session struct {
	ID      string    `json:"id"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`

	Provider string `json:"provider"`
	Target   string `json:"target"`

	// The fields below are set once the share is up.
	Mounted  bool     `json:"mounted"`
	Device   string   `json:"device,omitempty"`
	FSType   string   `json:"fstype,omitempty"`
	ReadOnly bool     `json:"read_only,omitempty"`
	Share    string   `json:"share,omitempty"`
	URL      string   `json:"url,omitempty"`
	User     string   `json:"user,omitempty"`
	Password string   `json:"password,omitempty"`
	Hints    []string `json:"hints,omitempty"`
}

// sessionIDPattern is the only shape a session ID may take, anything else in
// the sessions directory is ignored.
var sessionIDPattern = regexp.MustCompile(`^lmnt-[0-9a-f]{8}$`)

// ValidSessionID reports whether id could have come from NewSessionID.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// NewSessionID returns a fresh random session ID.
func NewSessionID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random session id: %w", err)
	}

	return "lmnt-" + hex.EncodeToString(b[:]), nil
}

func (d *Dir) sessionPath(id, suffix string) (string, error) {
	if !ValidSessionID(id) {
		return "", fmt.Errorf("invalid session id %q", id)
	}

	return d.join(filepath.Join(sessionDir, id+suffix))
}

// SaveSession writes (or rewrites) the record for s atomically.
func (d *Dir) SaveSession(s Session) error {
	if s.PID <= 0 {
		return fmt.Errorf("invalid pid %d", s.PID)
	}

	path, err := d.sessionPath(s.ID, ".json")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish session: %w", err)
	}

	return nil
}

// LockSession marks the session as owned by this process until the returned
// function is called. Readers use the lock, not just the PID, to decide
// whether the owner is still alive, which is immune to PID reuse. The
// release function also deletes the record.
func (d *Dir) LockSession(id string) (release func(), err error) {
	path, err := d.sessionPath(id, ".lock")
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create sessions directory: %w", err)
	}

	unlock, err := hostos.LockFile(path)
	if err != nil {
		return nil, fmt.Errorf("lock session %s: %w", id, err)
	}

	return func() {
		_ = d.DeleteSession(id)
		unlock()
	}, nil
}

// DeleteSession forgets the record for id, and any stop request left with
// it. A missing record is not an error.
func (d *Dir) DeleteSession(id string) error {
	path, err := d.sessionPath(id, ".json")
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete session: %w", err)
	}

	_ = d.ClearStop(id)

	return nil
}

// RequestStop asks the owner of a session to shut down, by creating the file
// it watches. A file rather than a signal: a stop request has to reach the
// owner so it can unmount and flush rather than be killed with a disk still
// attached, and a file is something the owner can poll for at a point where
// that is safe.
func (d *Dir) RequestStop(id string) error {
	path, err := d.sessionPath(id, ".stop")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is inside the data dir, built from a validated session id
	if err != nil {
		return fmt.Errorf("request stop of %s: %w", id, err)
	}

	return f.Close()
}

// StopRequested reports whether someone asked this session to stop.
func (d *Dir) StopRequested(id string) (bool, error) {
	path, err := d.sessionPath(id, ".stop")
	if err != nil {
		return false, err
	}

	switch _, err := os.Stat(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check stop request for %s: %w", id, err)
	}
}

// ClearStop drops a stop request, so a session that starts with the same id
// (or one that outlived a cancelled request) is not stopped at once.
func (d *Dir) ClearStop(id string) error {
	path, err := d.sessionPath(id, ".stop")
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear stop request: %w", err)
	}

	return nil
}

// SessionLogPath is where a detached session writes what it would otherwise
// have printed to the terminal.
func (d *Dir) SessionLogPath(id string) (string, error) {
	return d.sessionPath(id, ".log")
}

// logKeep is how long the log of a mount that is no longer running is kept.
// Long enough to read after a failure, short enough not to pile up.
const logKeep = 24 * time.Hour

// PruneSessionLogs deletes the logs of mounts that are neither running nor
// recent. A detached start calls it, so the directory tidies itself without a
// separate chore.
func (d *Dir) PruneSessionLogs(live []Session) error {
	dir := filepath.Join(d.root, sessionDir)

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read sessions directory: %w", err)
	}

	running := make(map[string]bool, len(live))
	for _, s := range live {
		running[s.ID] = true
	}

	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".log")
		if !ok || !ValidSessionID(id) || running[id] {
			continue
		}

		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < logKeep {
			continue
		}

		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			d.logger.Warn("Cannot remove an old session log", "id", id, "error", err)
		}
	}

	return nil
}

// Sessions lists the sessions whose owner is still running. Records of dead
// owners are deleted on the way, files that are not readable records are
// logged and skipped.
func (d *Dir) Sessions() ([]Session, error) {
	dir := filepath.Join(d.root, sessionDir)

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sessions directory: %w", err)
	}

	var live []Session
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidSessionID(id) {
			if !hasAnySuffix(e.Name(), ".lock", ".tmp", ".stop", ".log") {
				d.logger.Warn("Ignoring unexpected entry in the sessions directory", "path", filepath.Join(dir, e.Name()))
			}
			continue
		}

		path := filepath.Join(dir, e.Name())

		raw, err := os.ReadFile(path) //nolint:gosec // name matched the strict pattern above
		if err != nil {
			d.logger.Warn("Cannot read session record", "path", path, "error", err)
			continue
		}

		var s Session
		if err := json.Unmarshal(raw, &s); err != nil || s.ID != id || s.PID <= 0 {
			d.logger.Warn("Ignoring malformed session record", "path", path)
			continue
		}

		alive, err := d.sessionAlive(s)
		if err != nil {
			d.logger.Warn("Cannot tell whether a session is alive, keeping it", "id", id, "error", err)
			live = append(live, s)
			continue
		}

		if !alive {
			d.logger.Info("Removing the record of a session that is gone", "id", id, "pid", s.PID)
			base := strings.TrimSuffix(path, ".json")
			_ = os.Remove(path)
			_ = os.Remove(base + ".lock")
			_ = os.Remove(base + ".stop")
			continue
		}

		live = append(live, s)
	}

	return live, nil
}

func hasAnySuffix(name string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return false
}

// sessionAlive prefers the owner's lock, when the lock file is absent it
// falls back to asking whether the PID exists.
func (d *Dir) sessionAlive(s Session) (bool, error) {
	lockPath, err := d.sessionPath(s.ID, ".lock")
	if err != nil {
		return false, err
	}

	locked, err := hostos.FileLocked(lockPath)
	if err != nil {
		return false, err
	}

	if locked {
		return true, nil
	}

	if _, err := os.Stat(lockPath); err == nil {
		// The lock file exists but nobody holds it: the owner is gone.
		return false, nil
	}

	return hostos.ProcessAlive(s.PID)
}
