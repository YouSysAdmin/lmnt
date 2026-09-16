package datadir

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSessionIDs(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}

	if !ValidSessionID(id) {
		t.Errorf("generated id %q does not match the pattern", id)
	}

	for _, bad := range []string{"", "lmnt-", "lmnt-1234567", "lmnt-123456789", "lmnt-GHIJKLMN", "../x", "lmnt-xyz-12345678"} {
		if ValidSessionID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSessionsLifecycle(t *testing.T) {
	d := testDir(t)

	if got, err := d.Sessions(); err != nil || got != nil {
		t.Fatalf("empty dir: %v, %v", got, err)
	}

	id, _ := NewSessionID()
	release, err := d.LockSession(id)
	if err != nil {
		t.Fatal(err)
	}

	s := Session{ID: id, PID: os.Getpid(), Started: time.Now().Truncate(time.Second), Provider: "docker", Target: "img:/x.img"}
	if err := d.SaveSession(s); err != nil {
		t.Fatal(err)
	}

	got, err := d.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Mounted || got[0].Target != s.Target {
		t.Fatalf("got %+v", got)
	}

	// Rewriting with share details keeps a single record.
	s.Mounted = true
	s.URL = "smb://127.0.0.1:9000/lmnt"
	s.Password = "pw"
	s.Hints = []string{"a"}
	if err := d.SaveSession(s); err != nil {
		t.Fatal(err)
	}

	got, _ = d.Sessions()
	if len(got) != 1 || !got[0].Mounted || got[0].URL != s.URL || !slices.Equal(got[0].Hints, s.Hints) {
		t.Fatalf("after rewrite: %+v", got)
	}

	if entries, _ := os.ReadDir(filepath.Join(d.Path(), sessionDir)); len(entries) != 2 {
		t.Errorf("expected json + lock, got %d entries", len(entries))
	}

	release()

	if got, _ := d.Sessions(); len(got) != 0 {
		t.Fatalf("after release: %+v", got)
	}

	if err := d.DeleteSession(id); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestSessionsPruneDeadOwner(t *testing.T) {
	d := testDir(t)

	// A PID that certainly does not exist: a just-exited child.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot spawn a helper process:", err)
	}
	deadPID := cmd.Process.Pid

	id, _ := NewSessionID()
	if err := d.SaveSession(Session{ID: id, PID: deadPID, Started: time.Now(), Provider: "qemu", Target: "img:/y.img"}); err != nil {
		t.Fatal(err)
	}

	got, err := d.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("dead owner still listed: %+v", got)
	}

	if _, err := os.Stat(filepath.Join(d.Path(), sessionDir, id+".json")); !os.IsNotExist(err) {
		t.Error("dead record was not removed")
	}
}

func TestSessionsSkipGarbage(t *testing.T) {
	d := testDir(t)
	dir := filepath.Join(d.Path(), sessionDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(filepath.Join(dir, "lmnt-deadbeef.json"), []byte("{not json"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "lmnt-cafebabe.json.tmp"), []byte("{}"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "lmnt-00000001.json"), []byte(`{"id":"lmnt-00000002","pid":1}`), 0o600)

	got, err := d.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("garbage listed: %+v", got)
	}
}

func TestSaveSessionValidation(t *testing.T) {
	d := testDir(t)

	if err := d.SaveSession(Session{ID: "lmnt-12345678", PID: 0}); err == nil {
		t.Error("zero pid accepted")
	}
	if err := d.SaveSession(Session{ID: "../escape", PID: 1}); err == nil {
		t.Error("bad id accepted")
	}
	if _, err := d.LockSession("bad"); err == nil {
		t.Error("bad id accepted by LockSession")
	}
}

func TestStopRequest(t *testing.T) {
	d := testDir(t)

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}

	requested, err := d.StopRequested(id)
	if err != nil {
		t.Fatal(err)
	}

	if requested {
		t.Error("a fresh session must not be asked to stop")
	}

	if err := d.RequestStop(id); err != nil {
		t.Fatal(err)
	}

	requested, err = d.StopRequested(id)
	if err != nil {
		t.Fatal(err)
	}

	if !requested {
		t.Error("the request was not seen")
	}

	// Asking twice is not an error.
	if err := d.RequestStop(id); err != nil {
		t.Fatal(err)
	}

	if err := d.ClearStop(id); err != nil {
		t.Fatal(err)
	}

	requested, err = d.StopRequested(id)
	if err != nil {
		t.Fatal(err)
	}

	if requested {
		t.Error("the request was not cleared")
	}

	// Clearing a request nobody made is not an error either.
	if err := d.ClearStop(id); err != nil {
		t.Error(err)
	}

	if _, err := d.StopRequested("not-a-session"); err == nil {
		t.Error("a bad id must be rejected")
	}

	if err := d.RequestStop("not-a-session"); err == nil {
		t.Error("a bad id must be rejected")
	}
}

// A session that exits must leave no stop request behind for the next one.
func TestDeleteSessionClearsStopRequest(t *testing.T) {
	d := testDir(t)

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}

	if err := d.SaveSession(Session{ID: id, PID: os.Getpid(), Started: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := d.RequestStop(id); err != nil {
		t.Fatal(err)
	}

	if err := d.DeleteSession(id); err != nil {
		t.Fatal(err)
	}

	requested, err := d.StopRequested(id)
	if err != nil {
		t.Fatal(err)
	}

	if requested {
		t.Error("the stop request outlived the session")
	}
}

func TestPruneSessionLogs(t *testing.T) {
	d := testDir(t)

	live, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}

	old, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}

	recent, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}

	write := func(id string, age time.Duration) string {
		path, err := d.SessionLogPath(id)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("log\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}

		return path
	}

	livePath := write(live, 48*time.Hour)
	oldPath := write(old, 48*time.Hour)
	recentPath := write(recent, time.Minute)

	err = d.PruneSessionLogs([]Session{{ID: live, PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}

	for _, keep := range []string{livePath, recentPath} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s should have been kept: %v", keep, err)
		}
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("%s should have been pruned", oldPath)
	}
}
