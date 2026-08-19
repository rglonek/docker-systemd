package supervisor

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"docker-systemd/internal/paths"
	"golang.org/x/sys/unix"
)

// Notification is one sd_notify assignment together with the kernel-supplied
// sender pid.
type Notification struct {
	SenderPID int
	Key       string
	Value     string
}

// NotifyListener serves a unit's $NOTIFY_SOCKET.
//
// sd_notify is the only mechanism in the main-PID table that is exact rather
// than heuristic (03 §7). v0.5.x treated Type=notify as forking and never set
// $NOTIFY_SOCKET at all (defect B12).
type NotifyListener struct {
	path string
	fd   int
	ch   chan Notification
	once sync.Once
}

// NewNotifyListener binds a per-unit datagram socket with SO_PASSCRED enabled
// and hands it to the unit's own credentials.
//
// The socket is mode 0666 because a unit that dropped to User= must write to
// it, and it is owned by that unit's uid/gid. Its directories are 0711: a
// non-root unit process cannot reach a path whose parents it may not search,
// however well it knows the path, so the 0700 the design originally called for
// made $NOTIFY_SOCKET undeliverable for exactly the units that needed the 0666
// mode in the first place. Spoofing is prevented by SO_PASSCRED attribution
// rather than by the directory mode.
func NewNotifyListener(path string, cred credentials) (*NotifyListener, error) {
	if err := os.MkdirAll(paths.NotifyDir, paths.ModeNotifyDir); err != nil {
		return nil, err
	}
	// MkdirAll is a no-op on a directory the manager already created, and it
	// applies the umask; chmod both components explicitly so the search bit is
	// there whatever created them.
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{paths.RuntimeDir, paths.ModeRuntimeDir},
		{paths.NotifyDir, paths.ModeNotifyDir},
	} {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(path)
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	// The sender pid comes from SCM_CREDENTIALS, never from the message body:
	// MAINPID= alone is attacker-controlled by any process that can reach the
	// socket.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := os.Chmod(path, paths.ModeNotifySck); err != nil {
		unix.Close(fd)
		return nil, err
	}
	// Hand the socket to the unit's own identity as well. This is ownership
	// hygiene rather than the thing that grants access — the 0666 mode above is
	// — so a filesystem that refuses the chown must not fail the unit.
	if cred.uid != nil || cred.gid != nil {
		uid, gid := -1, -1
		if cred.uid != nil {
			uid = *cred.uid
		}
		if cred.gid != nil {
			gid = *cred.gid
		}
		_ = os.Chown(path, uid, gid)
	}
	l := &NotifyListener{path: path, fd: fd, ch: make(chan Notification, 64)}
	go l.loop()
	return l, nil
}

// Path returns the socket path to export as $NOTIFY_SOCKET.
func (l *NotifyListener) Path() string { return l.path }

// Notifications delivers parsed assignments.
func (l *NotifyListener) Notifications() <-chan Notification { return l.ch }

// Close shuts the listener down and removes the socket.
func (l *NotifyListener) Close() {
	l.once.Do(func() {
		unix.Close(l.fd)
		_ = os.Remove(l.path)
		close(l.ch)
	})
}

func (l *NotifyListener) loop() {
	buf := make([]byte, 8192)
	oob := make([]byte, unix.CmsgSpace(unix.SizeofUcred))
	for {
		n, oobn, _, _, err := unix.Recvmsg(l.fd, buf, oob, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return
		}
		pid := credentialPID(oob[:oobn])
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				continue
			}
			select {
			case l.ch <- Notification{SenderPID: pid, Key: line[:eq], Value: line[eq+1:]}:
			default:
				// A daemon spamming STATUS= must not be able to stall the
				// supervisor's own loop.
			}
		}
	}
}

// credentialPID extracts the sender pid from SCM_CREDENTIALS.
func credentialPID(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_CREDENTIALS {
			continue
		}
		ucred, err := unix.ParseUnixCredentials(&m)
		if err != nil {
			continue
		}
		return int(ucred.Pid)
	}
	return 0
}

// SendNotify implements the client half, used by the `systemd-notify` helper.
func SendNotify(socket string, assignments []string) error {
	if socket == "" {
		return fmt.Errorf("$NOTIFY_SOCKET is not set")
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// An abstract-namespace socket path starts with '@' in the sd_notify
	// convention; translate it to the leading NUL the kernel expects.
	name := socket
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	payload := strings.Join(assignments, "\n")
	return unix.Sendto(fd, []byte(payload), 0, &unix.SockaddrUnix{Name: name})
}
