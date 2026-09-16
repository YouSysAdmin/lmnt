//go:build live

// Live verification of the share backends against a real Alpine container.
//
// Prepare the container once:
//
//	docker run -d --rm --privileged --name lmnt-devtest \
//	    -p 127.0.0.1:2022:2022 -p 127.0.0.1:9445:445 -p 127.0.0.1:9021:21 alpine:3.24 sleep infinity
//	docker exec lmnt-devtest apk add openrc openssh samba vsftpd lvm2 device-mapper cryptsetup util-linux
//	docker exec lmnt-devtest sh -c 'mkdir -p /run/openrc && touch /run/openrc/softlevel'
//
// then run: go test -tags live -run Live -v ./internal/share/
package share

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousysadmin/lmnt/internal/guest"
)

func containerName() string {
	if n := os.Getenv("LMNT_LIVE_CONTAINER"); n != "" {
		return n
	}

	return "lmnt-devtest"
}

// dockerExec is a guest.Executor over `docker exec`.
type dockerExec struct{ name string }

func (d dockerExec) Run(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", d.name, "sh", "-c", script)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, &guest.CommandError{Script: script, ExitCode: cmd.ProcessState.ExitCode(), Stderr: stderr.String(), Cause: err}
	}

	return stdout.Bytes(), nil
}

func (d dockerExec) Start(ctx context.Context, script string, s guest.Streams) (guest.Process, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", d.name, "sh", "-c", script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return dockerProc{cmd: cmd, script: script}, nil
}

func (d dockerExec) Shell(context.Context, guest.Terminal) error { return nil }

type dockerProc struct {
	cmd    *exec.Cmd
	script string
}

func (p dockerProc) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		return &guest.CommandError{Script: p.script, ExitCode: p.cmd.ProcessState.ExitCode(), Cause: err}
	}

	return nil
}

func liveGuest(t *testing.T) (*guest.Guest, dockerExec) {
	t.Helper()

	ex := dockerExec{name: containerName()}
	if _, err := ex.Run(t.Context(), "true"); err != nil {
		t.Skipf("container %s not reachable: %v", ex.name, err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return guest.New(logger, ex), ex
}

func waitPort(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("%s never started listening", addr)
}

func TestLiveSFTP(t *testing.T) {
	g, ex := liveGuest(t)

	// Clean slate for repeat runs.
	_, _ = ex.Run(t.Context(), "pkill -f sshd_share_config; rm -f /mnt/lmnt-live-*; sed -i '/^lmnt:/d' /etc/passwd /etc/shadow")

	b := &SFTP{}
	if _, err := b.Plan(Options{}); err != nil {
		t.Fatal(err)
	}

	pw, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}

	info, err := b.Start(t.Context(), g, Credentials{User: User, Password: pw})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("info: %+v", info)

	out, err := ex.Run(t.Context(), "id lmnt")
	if err != nil || !strings.HasPrefix(string(out), "uid=0(") {
		t.Fatalf("lmnt is not a uid 0 alias: %q %v", out, err)
	}

	waitPort(t, "127.0.0.1:2022")

	// Drive the real sftp client with expect, since there is no sshpass.
	local := filepath.Join(t.TempDir(), "lmnt-live-upload.txt")
	if err := os.WriteFile(local, []byte("hello from the host\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`set timeout 20
spawn sftp -P 2022 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PreferredAuthentications=password -o PubkeyAuthentication=no lmnt@127.0.0.1
expect {
  -re "(?i)password:" { send "%s\r" }
  timeout { puts "NO PASSWORD PROMPT"; exit 2 }
}
expect {
  "sftp>" { send "put %s /mnt/lmnt-live-upload.txt\r" }
  "Permission denied" { puts "AUTH FAILED"; exit 3 }
  timeout { puts "NO PROMPT"; exit 2 }
}
expect "sftp>" { send "ls -l /mnt\r" }
expect "sftp>" { send "bye\r" }
expect eof
`, pw, local)

	cmd := exec.CommandContext(t.Context(), "expect", "-c", script)
	transcript, err := cmd.CombinedOutput()
	t.Logf("sftp transcript:\n%s", transcript)

	if err != nil {
		t.Fatalf("sftp session failed: %v", err)
	}

	out, err = ex.Run(t.Context(), "stat -c '%U %s %n' /mnt/lmnt-live-upload.txt && cat /mnt/lmnt-live-upload.txt")
	if err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}

	t.Logf("in guest: %s", out)

	if !strings.HasPrefix(string(out), "root 20 ") || !strings.Contains(string(out), "hello from the host") {
		t.Errorf("file not written as root with the right content: %q", out)
	}
}

func TestLiveSMB(t *testing.T) {
	g, ex := liveGuest(t)

	_, _ = ex.Run(t.Context(), "rc-service samba stop >/dev/null 2>&1; sed -i '/^lmnt:/d' /etc/passwd /etc/shadow; rm -f /var/lib/samba/private/passdb.tdb")

	b := &SMB{}
	if _, err := b.Plan(Options{}); err != nil {
		t.Fatal(err)
	}

	pw, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}

	info, err := b.Start(t.Context(), g, Credentials{User: User, Password: pw})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("info: %+v", info)

	waitPort(t, "127.0.0.1:9445")

	out, err := ex.Run(t.Context(), "pdbedit -L && testparm -s 2>/dev/null | grep -E 'force user|path'")
	if err != nil {
		t.Fatalf("samba not configured: %v", err)
	}

	t.Logf("samba: %s", out)

	if !strings.Contains(string(out), "lmnt:1000:") {
		t.Errorf("no samba account for lmnt: %q", out)
	}

	if os.Getenv("LMNT_LIVE_MOUNT_SMB") == "" {
		t.Log("set LMNT_LIVE_MOUNT_SMB=1 to also mount the share with mount_smbfs")
		return
	}

	mnt := filepath.Join(t.TempDir(), "smb")
	if err := os.Mkdir(mnt, 0o700); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("//lmnt:%s@127.0.0.1:9445/lmnt", pw)
	if out, err := exec.Command("mount_smbfs", url, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount_smbfs: %v: %s", err, out)
	}

	defer func() { _ = exec.Command("umount", mnt).Run() }()

	if err := os.WriteFile(filepath.Join(mnt, "lmnt-live-smb.txt"), []byte("via smb\n"), 0o644); err != nil {
		t.Fatalf("write over smb: %v", err)
	}

	out, err = ex.Run(t.Context(), "stat -c '%U %n' /mnt/lmnt-live-smb.txt")
	if err != nil || !strings.HasPrefix(string(out), "root ") {
		t.Errorf("smb write not owned by root: %q %v", out, err)
	}
}

func TestLiveFTP(t *testing.T) {
	g, ex := liveGuest(t)

	_, _ = ex.Run(t.Context(), "rc-service vsftpd stop >/dev/null 2>&1; sed -i '/^lmnt:/d' /etc/passwd /etc/shadow")

	b := &FTP{}
	if _, err := b.Plan(Options{}); err != nil {
		t.Fatal(err)
	}

	pw, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}

	info, err := b.Start(t.Context(), g, Credentials{User: User, Password: pw})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("info: %+v", info)

	waitPort(t, "127.0.0.1:9021")

	// The passive ports are not mapped into this container, so the check
	// stops at a successful login.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:9021", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)

	expect := func(code string) string {
		t.Helper()

		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("ftp read: %v", err)
			}

			t.Logf("ftp< %s", strings.TrimSpace(line))

			if strings.HasPrefix(line, code+" ") {
				return line
			}

			if len(line) >= 4 && line[3] == ' ' {
				t.Fatalf("ftp: wanted %s, got %s", code, strings.TrimSpace(line))
			}
		}
	}

	expect("220")
	fmt.Fprintf(conn, "USER lmnt\r\n")
	expect("331")
	fmt.Fprintf(conn, "PASS %s\r\n", pw)
	expect("230")
	fmt.Fprintf(conn, "PWD\r\n")
	pwd := expect("257")

	if !strings.Contains(pwd, `"/"`) {
		t.Errorf("chroot not at /mnt: %s", pwd)
	}

	fmt.Fprintf(conn, "QUIT\r\n")
}
