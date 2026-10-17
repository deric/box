package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/deric/box/internal/proxy"
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

// PastaSetup is what `box _netns -pasta` needs to set the pasta backend
// up in the namespaces it runs in (see Pasta).
type PastaSetup struct {
	Self   string   // the box executable, run as the resolver
	Socket string   // the proxy's unix socket on the host
	Log    string   // the proxy's log file, shared by the resolver
	NFT    string   // nft(8) on the host
	Allow  []string // allowed destination hosts
}

// pastaTimeout bounds attaching pasta to the namespace.
const pastaTimeout = 30 * time.Second

// PreparePasta is the body of `box _netns -pasta`, run in the user and
// network namespaces unshare(1) created: it brings loopback up, binds the
// DNS listeners, loads the firewall, has the proxy attach pasta to the
// namespace and starts the resolver (`box _filter`) with the listeners,
// then drops every capability like PrepareDNS. What fails after the
// listeners are bound leaves the sandbox without network, with a warning,
// rather than without a sandbox; the firewall, if loaded, stays.
func PreparePasta(o PastaSetup) (warnings []string, err error) {
	if err := loopbackUp(); err != nil {
		return nil, fmt.Errorf("loopback: %w", err)
	}
	udp, tcp, err := listenDNS()
	if err != nil {
		return nil, err
	}
	defer func() { _ = udp.Close(); _ = tcp.Close() }()
	warn := func(what string, err error) ([]string, error) {
		return []string{"no network: " + what + ": " + err.Error()}, dropCapabilities()
	}
	if err := (&NFT{Exe: o.NFT}).Load(); err != nil {
		return warn("the firewall could not be loaded", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pastaTimeout)
	defer cancel()
	if err := proxy.RequestPasta(ctx, o.Socket, os.Getpid()); err != nil {
		return warn("pasta could not be attached", err)
	}
	if err := startFilter(o, udp, tcp); err != nil {
		return warn("the resolver could not be started", err)
	}
	return nil, dropCapabilities()
}

// listenDNS binds the UDP and TCP DNS listeners to DNSAddr.
func listenDNS() (*net.UDPConn, *net.TCPListener, error) {
	udp, err := net.ListenPacket("udp4", DNSAddr)
	if err != nil {
		return nil, nil, err
	}
	tcp, err := net.Listen("tcp4", DNSAddr)
	if err != nil {
		_ = udp.Close()
		return nil, nil, err
	}
	return udp.(*net.UDPConn), tcp.(*net.TCPListener), nil
}

// startFilter starts `box _filter` with the listeners at descriptors 3
// and 4. The child inherits the ambient capabilities and is told to die
// with this process, which becomes the runner.
func startFilter(o PastaSetup, udp *net.UDPConn, tcp *net.TCPListener) error {
	uf, err := udp.File()
	if err != nil {
		return err
	}
	defer func() { _ = uf.Close() }()
	tf, err := tcp.File()
	if err != nil {
		return err
	}
	defer func() { _ = tf.Close() }()
	args := []string{"_filter", "-u", "3", "-t", "4", "-s", o.Socket, "-l", o.Log, "-f", o.NFT}
	for _, h := range o.Allow {
		args = append(args, "-a", h)
	}
	cmd := exec.Command(o.Self, args...)
	cmd.ExtraFiles = []*os.File{uf, tf}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = dieWithParent()
	if err := cmd.Start(); err != nil {
		return err
	}
	// The runner, which this process becomes, reaps it.
	_ = cmd.Process.Release()
	return nil
}

// ProbePasta is `box _netns -pasta` without a command, as run by
// PastaStatus: it checks that the listeners can be bound and the firewall
// loaded in the namespaces unshare(1) created. With wait it then prints
// "ready" on stdout and holds the namespace open until stdin is closed,
// for the caller to try attaching pasta to it.
func ProbePasta(nft string, wait bool) error {
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	udp, tcp, err := listenDNS()
	if err != nil {
		return err
	}
	defer func() { _ = udp.Close(); _ = tcp.Close() }()
	if err := (&NFT{Exe: nft}).Load(); err != nil {
		return err
	}
	if wait {
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	return nil
}

// KeepNetAdmin reduces the capabilities of the current process to
// CAP_NET_ADMIN, in every set including the ambient one, so that the nft
// it runs gets it too and nothing else. As dropCapabilities, it acts on
// every thread where the runtime allows, else on the calling one.
func KeepNetAdmin() error {
	const keep = unix.CAP_NET_ADMIN
	// Dropping from the bounding set needs CAP_SETPCAP, so that goes first.
	for c := uintptr(0); c <= unix.CAP_LAST_CAP; c++ {
		if c == keep {
			continue
		}
		if e := capSyscall(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, c, 0); e != 0 && e != unix.EINVAL {
			return fmt.Errorf("dropping capability %d from the bounding set: %w", c, e)
		}
		if e := capSyscall(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_LOWER, c); e != 0 && e != unix.EINVAL {
			return fmt.Errorf("lowering ambient capability %d: %w", c, e)
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	bit := uint32(1) << (keep % 32)
	data[keep/32] = unix.CapUserData{Effective: bit, Permitted: bit, Inheritable: bit}
	if e := capSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("setting capabilities: %w", e)
	}
	return nil
}

// PastaStatus checks that the pasta backend can be provided on this host:
// pasta and nft are installed, unshare(1) can create the namespaces, the
// firewall loads in them and pasta attaches to them. It returns the paths
// found (what was found so far on error) and pasta's version as it
// reports it (empty when unknown); the error says why pasta is out of
// reach, ErrNoPasta or ErrNoNFT when they are not installed.
func PastaStatus() (pasta, nft, version string, err error) {
	if pasta, err = lookupPasta(); err != nil {
		return "", "", "", ErrNoPasta
	}
	// Prints "pasta 2025_05_08.0ecbbd4" (or a distribution's variant).
	if out, err := exec.Command(pasta, "--version").Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) >= 2 && strings.HasPrefix(f[0], "pas") {
			version = f[1]
		}
	}
	if nft, err = lookupNFT(); err != nil {
		return pasta, "", version, ErrNoNFT
	}
	unshare, err := lookupUnshare()
	if err != nil {
		return pasta, nft, version, ErrNoUnshare
	}
	self, err := os.Executable()
	if err != nil {
		return pasta, nft, version, err
	}
	return pasta, nft, version, probePasta(unshare, self, nft, pasta)
}

// probePasta runs `unshare ... self _netns -pasta -f nft -wait`, which
// holds a prepared namespace open, attaches pasta to it the way a sandbox
// does and lets both go; tests replace it.
var probePasta = func(unshare, self, nft, pasta string) error {
	args := append(unshareArgs(os.Getuid(), os.Getgid()), self, "_netns", "-pasta", "-f", nft, "-wait")
	cmd := exec.Command(unshare, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	finish := func() error {
		_ = stdin.Close()
		err := cmd.Wait()
		msg := strings.TrimSpace(stderr.String())
		if err != nil && msg == "" {
			msg = err.Error()
		}
		if msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "ready" {
			err = fmt.Errorf("unexpected output %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			if werr := finish(); werr != nil {
				return werr
			}
			return err
		}
	case <-time.After(pastaTimeout):
		_ = cmd.Process.Kill()
		_ = finish()
		return fmt.Errorf("preparing the namespace timed out")
	}
	perr := runPasta(pasta, cmd.Process.Pid)
	if err := finish(); err != nil {
		return err
	}
	return perr
}

// runPasta attaches pasta to the namespaces of pid and waits until it is
// up: pasta forks into the background once the interface is configured,
// so its first process exiting successfully means it is. Its output goes
// to a file rather than a pipe, which the daemon would hold open.
func runPasta(exe string, pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), pastaTimeout)
	defer cancel()
	out, err := os.CreateTemp("", "box-pasta-")
	if err != nil {
		return err
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	cmd := exec.CommandContext(ctx, exe, PastaArgs(pid)...)
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	if err != nil {
		msg, _ := os.ReadFile(out.Name())
		text := strings.TrimSpace(string(msg))
		if text == "" {
			text = err.Error()
		}
		return fmt.Errorf("%s", strings.ReplaceAll(text, "\n", "; "))
	}
	return nil
}

// RunPasta attaches pasta (the executable exe) to the namespaces of
// process pid, as the proxy does on request (see proxy.PastaPath).
func RunPasta(exe string, pid int) error { return runPasta(exe, pid) }

// PastaInfo describes how the pasta backend stands on this host: the
// executables found, or why it is out of reach.
func PastaInfo() string {
	pasta, nft, version, err := PastaStatus()
	if err != nil {
		return "not available: " + err.Error()
	}
	info := pasta
	if version != "" {
		info += " (" + version + ")"
	}
	return info + " with nft at " + nft
}
