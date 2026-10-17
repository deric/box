package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DNSAddr is where the DNS listeners are bound inside the sandbox's network
// namespace and what the sandbox's /etc/resolv.conf points at.
const DNSAddr = "127.0.0.1:53"

// lookupUnshare finds unshare(1); tests replace it.
var lookupUnshare = func() (string, error) { return exec.LookPath("unshare") }

// probeNetns checks that the namespaces DNS needs can be set up by running
// `unshare ... self _netns` without a command, which binds the listeners
// and exits; tests replace it.
var probeNetns = func(unshare, self string) error {
	args := append(unshareArgs(os.Getuid(), os.Getgid()), self, "_netns")
	out, err := exec.Command(unshare, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// PrepareDNS is the body of `box _netns`, run in the user and network
// namespaces unshare(1) created: it brings the loopback interface up, binds
// the UDP and TCP DNS listeners to DNSAddr and places them at descriptors
// udpFD and tcpFD (where the caller's command line has them; negative skips
// that), then drops every capability, so that the runner that is executed
// next starts unprivileged as usual. The capabilities are dropped on the
// calling thread, which must be the one that goes on to exec.
func PrepareDNS(udpFD, tcpFD int) error {
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	udp, err := net.ListenPacket("udp4", DNSAddr)
	if err != nil {
		return err
	}
	defer func() { _ = udp.Close() }()
	tcp, err := net.Listen("tcp4", DNSAddr)
	if err != nil {
		return err
	}
	defer func() { _ = tcp.Close() }()
	for _, l := range []struct {
		fd   int
		conn interface{ File() (*os.File, error) }
	}{{udpFD, udp.(*net.UDPConn)}, {tcpFD, tcp.(*net.TCPListener)}} {
		if l.fd < 0 {
			continue
		}
		f, err := l.conn.File()
		if err != nil {
			return err
		}
		// dup2 clears close-on-exec on the target, as needed.
		err = unix.Dup2(int(f.Fd()), l.fd)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("placing DNS socket at fd %d: %w", l.fd, err)
		}
	}
	return dropCapabilities()
}

// loopbackUp brings lo up; the kernel assigns 127.0.0.1/8 and ::1 with it.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// capSyscall makes a capability-related system call on every thread, or,
// where the runtime does not support that (programs linked with cgo), on
// the calling thread only; the caller then execs from that thread.
func capSyscall(trap, a1, a2, a3 uintptr) syscall.Errno {
	_, _, e := syscall.AllThreadsSyscall(trap, a1, a2, a3)
	if e == syscall.ENOTSUP {
		_, _, e = syscall.RawSyscall(trap, a1, a2, a3)
	}
	return e
}

// dropCapabilities clears the bounding, ambient, inheritable, permitted and
// effective capability sets.
func dropCapabilities() error {
	for c := uintptr(0); c <= unix.CAP_LAST_CAP; c++ {
		if e := capSyscall(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, c, 0); e != 0 && e != unix.EINVAL {
			return fmt.Errorf("dropping capability %d from the bounding set: %w", c, e)
		}
	}
	if e := capSyscall(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0); e != 0 {
		return fmt.Errorf("clearing ambient capabilities: %w", e)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if e := capSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("clearing capabilities: %w", e)
	}
	return nil
}

// DNSStatus checks that DNS can be provided to sandboxes on this host: it
// finds unshare(1) and probes that it can create the user and network
// namespaces box needs. It returns the path of unshare and the util-linux
// version it reports (empty when unknown); the error says why DNS is out
// of reach, and ErrNoUnshare when unshare is not installed.
func DNSStatus() (unshare, version string, err error) {
	unshare, err = lookupUnshare()
	if err != nil {
		return "", "", ErrNoUnshare
	}
	// Prints "unshare from util-linux 2.41.5".
	if out, err := exec.Command(unshare, "--version").Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) >= 4 {
			version = f[len(f)-1]
		}
	}
	self, err := os.Executable()
	if err == nil {
		err = probeNetns(unshare, self)
	}
	return unshare, version, err
}

// ErrNoUnshare is returned by DNSStatus when unshare(1) is not installed.
var ErrNoUnshare = errors.New("unshare not found; install util-linux")

// DNSInfo describes how DNS is provided to sandboxes on this host: the
// unshare(1) that creates their network namespace, or why DNS is out of
// reach.
func DNSInfo() string {
	unshare, version, err := DNSStatus()
	if err != nil {
		return "not available: " + err.Error()
	}
	info := "via the proxy, in a network namespace from " + unshare
	if version != "" {
		info += " (util-linux " + version + ")"
	}
	return info
}
