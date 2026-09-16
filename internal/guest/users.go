package guest

import (
	"context"
	"fmt"
	"strings"
)

// User describes a guest account to create.
type User struct {
	Name string
	// UID is the numeric id. Zero makes the account an alias of root, which
	// BusyBox adduser refuses, so such accounts are written to /etc/passwd
	// and /etc/shadow directly.
	UID int
	// Home defaults to MountPoint.
	Home string
	// Shell defaults to /bin/sh.
	Shell string
}

func (u User) withDefaults() User {
	if u.Home == "" {
		u.Home = MountPoint
	}

	if u.Shell == "" {
		u.Shell = "/bin/sh"
	}

	return u
}

// EnsureUser creates u unless an account with that name already exists. No
// home directory is created, Home is expected to exist (it is normally
// MountPoint).
func (g *Guest) EnsureUser(ctx context.Context, u User) error {
	if err := checkUserName(u.Name); err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}

	if u.UID < 0 {
		return fmt.Errorf("ensure user %s: negative uid %d", u.Name, u.UID)
	}

	u = u.withDefaults()

	for _, p := range []string{u.Home, u.Shell} {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, ":\n") {
			return fmt.Errorf("ensure user %s: %q is not a usable path", u.Name, p)
		}
	}

	exists := "id " + Quote(u.Name) + " >/dev/null 2>&1"

	var create string
	if u.UID == 0 {
		passwd := fmt.Sprintf("%s:x:0:0::%s:%s", u.Name, u.Home, u.Shell)
		shadow := u.Name + ":!::0:::::"
		create = "printf '%s\\n' " + Quote(passwd) + " >> /etc/passwd && printf '%s\\n' " + Quote(shadow) + " >> /etc/shadow"
	} else {
		// The group is created first so that a leftover group with the
		// same name or gid does not make adduser refuse the uid.
		create = QuoteAll("addgroup", "-g", fmt.Sprint(u.UID), u.Name) + " 2>/dev/null; " +
			QuoteAll("adduser", "-D", "-H", "-h", u.Home, "-s", u.Shell, "-u", fmt.Sprint(u.UID), "-G", u.Name, u.Name)
	}

	if _, err := g.Run(ctx, exists+" || { "+create+"; }"); err != nil {
		return fmt.Errorf("ensure user %s: %w", u.Name, err)
	}

	return nil
}

// SetPassword sets a unix account password with chpasswd.
func (g *Guest) SetPassword(ctx context.Context, user, password string) error {
	if err := checkUserName(user); err != nil {
		return fmt.Errorf("set password: %w", err)
	}

	if err := checkPassword(password); err != nil {
		return fmt.Errorf("set password for %s: %w", user, err)
	}

	if err := g.feed(ctx, "chpasswd", strings.NewReader(user+":"+password+"\n")); err != nil {
		return fmt.Errorf("set password for %s: %w", user, err)
	}

	return nil
}

// SetSambaPassword creates or updates the Samba account for user. The unix
// account must already exist.
func (g *Guest) SetSambaPassword(ctx context.Context, user, password string) error {
	if err := checkUserName(user); err != nil {
		return fmt.Errorf("set samba password: %w", err)
	}

	if err := checkPassword(password); err != nil {
		return fmt.Errorf("set samba password for %s: %w", user, err)
	}

	// -s reads the password twice from stdin, the second time to confirm.
	script := QuoteAll("smbpasswd", "-a", "-s", user)
	stdin := strings.NewReader(password + "\n" + password + "\n")

	if err := g.feed(ctx, script, stdin); err != nil {
		return fmt.Errorf("set samba password for %s: %w", user, err)
	}

	return nil
}

func checkPassword(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("password is empty")
	case strings.ContainsAny(p, "\n\r\x00"):
		return fmt.Errorf("password contains line breaks")
	default:
		return nil
	}
}
