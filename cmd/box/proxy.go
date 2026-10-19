package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/deric/box/internal/proxy"
	"github.com/deric/box/internal/sandbox"
)

type stringList []string

func (l *stringList) String() string     { return fmt.Sprint([]string(*l)) }
func (l *stringList) Set(s string) error { *l = append(*l, s); return nil }

// cmdProxy runs the allowlisting proxy on a unix socket. It is started by
// `box run` on the host and exits when that process (which becomes bwrap)
// goes away.
func cmdProxy(argv []string) error {
	fs := flag.NewFlagSet("_proxy", flag.ExitOnError)
	sock := fs.String("s", "", "unix socket to listen on")
	logPath := fs.String("l", "", "log file for denied requests")
	dns := fs.Bool("d", false, "relay DNS queries to the host's resolver")
	ssh := fs.Bool("ssh", false, "let CONNECT through to port 22 of allowed hosts")
	pasta := fs.String("pasta", "", "attach this pasta executable to the sandbox's namespaces on request")
	var allow stringList
	fs.Var(&allow, "a", "allowed host (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *sock == "" {
		return fmt.Errorf("_proxy: -s is required")
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		logger.SetOutput(f)
	}
	if err := os.Remove(*sock); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	// The socket lives in a shared directory; only its owner may connect.
	if err := os.Chmod(*sock, 0o600); err != nil {
		return err
	}
	if ready := os.NewFile(proxy.ReadyFD, "ready"); ready != nil {
		_, _ = ready.Write([]byte{1})
		_ = ready.Close()
	}
	// The parent-death signal normally ends this process; polling the
	// parent PID is the fallback.
	parent := os.Getppid()
	go func() {
		for range time.Tick(time.Second) {
			if os.Getppid() != parent {
				_ = os.Remove(*sock)
				os.Exit(0)
			}
		}
	}()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-sigs
		_ = os.Remove(*sock)
		os.Exit(0)
	}()
	srv := &proxy.Server{Allow: proxy.Allowlist(allow), SSH: *ssh, Log: logger}
	if *dns {
		srv.DNS = &proxy.Resolver{}
	}
	if *pasta != "" {
		srv.Pasta = func(pid int) error {
			if err := sandbox.RunPasta(*pasta, pid); err != nil {
				return err
			}
			logger.Printf("pasta attached to the namespaces of pid %d", pid)
			return nil
		}
	}
	return srv.Serve(ln)
}

// cmdConnect runs inside the sandbox as ssh's ProxyCommand (see
// internal/sandbox/ssh.go): it asks the proxy, over its unix socket, for a
// tunnel to host:port and relays stdin and stdout through it. A refusal
// ends up on stderr, where ssh shows it.
func cmdConnect(argv []string) error {
	fs := flag.NewFlagSet("_connect", flag.ExitOnError)
	sock := fs.String("s", sandbox.ProxySocket, "proxy unix socket")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("_connect: usage: box _connect [-s SOCKET] HOST PORT")
	}
	return proxy.Connect(*sock, net.JoinHostPort(fs.Arg(0), fs.Arg(1)), os.Stdin, os.Stdout)
}

// cmdNetns runs in the user and network namespaces unshare(1) created for
// a sandbox with DNS (see sandbox.DNS): it binds the DNS listeners, places
// them at the given descriptors, drops its capabilities and execs the
// command, with argv[0] tagged for `box ps` when -n is given. With -pasta
// it sets the pasta backend up instead (see sandbox.Pasta): the listeners
// stay with the resolver it starts, so -u and -t are not given. Without a
// command it only checks that the listeners can be bound (and, with
// -pasta, the firewall loaded; -wait then holds the namespace open until
// stdin is closed, see sandbox.ProbePasta).
func cmdNetns(argv []string) error {
	fs := flag.NewFlagSet("_netns", flag.ExitOnError)
	udp := fs.Int("u", -1, "descriptor to place the UDP listener at")
	tcp := fs.Int("t", -1, "descriptor to place the TCP listener at")
	name := fs.String("n", "", "sandbox name to tag the command with")
	pasta := fs.Bool("pasta", false, "set the pasta backend up")
	sock := fs.String("s", "", "proxy unix socket (pasta)")
	logPath := fs.String("l", "", "proxy log file (pasta)")
	nft := fs.String("f", "", "nft executable (pasta)")
	wait := fs.Bool("wait", false, "without a command: hold the namespace open until stdin is closed (pasta)")
	var allow stringList
	fs.Var(&allow, "a", "allowed host (pasta, repeatable)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	// The capabilities are dropped on this thread, which execs next.
	runtime.LockOSThread()
	switch {
	case *pasta && fs.NArg() == 0:
		if err := sandbox.ProbePasta(*nft, *wait); err != nil {
			return fmt.Errorf("_netns: %w", err)
		}
		return nil
	case *pasta:
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("_netns: %w", err)
		}
		warnings, err := sandbox.PreparePasta(sandbox.PastaSetup{Self: self, Socket: *sock, Log: *logPath, NFT: *nft, Allow: allow})
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "box: warning:", w)
		}
		if err != nil {
			return fmt.Errorf("_netns: %w", err)
		}
	default:
		if err := sandbox.PrepareDNS(*udp, *tcp); err != nil {
			return fmt.Errorf("_netns: %w", err)
		}
		if fs.NArg() == 0 {
			return nil
		}
	}
	cmdArgv := append([]string{}, fs.Args()...)
	if *name != "" {
		cmdArgv[0] = sandbox.ProcTitlePrefix + *name
	}
	return syscall.Exec(fs.Arg(0), cmdArgv, os.Environ())
}

// cmdFilter is the resolver of a pasta sandbox (see sandbox.Pasta), started
// by `box _netns -pasta` in the sandbox's namespaces with the DNS listeners
// at the given descriptors: it answers queries for allowed names through
// the proxy and admits their addresses into the firewall with nft, keeping
// CAP_NET_ADMIN for that and nothing else. Denied names go to the log file.
// It exits with the sandbox, whose runner is its parent.
func cmdFilter(argv []string) error {
	fs := flag.NewFlagSet("_filter", flag.ExitOnError)
	udpFD := fs.Int("u", -1, "inherited UDP DNS listener")
	tcpFD := fs.Int("t", -1, "inherited TCP DNS listener")
	sock := fs.String("s", "", "proxy unix socket")
	logPath := fs.String("l", "", "log file for denied names")
	nft := fs.String("f", "nft", "nft executable")
	var allow stringList
	fs.Var(&allow, "a", "allowed host (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *sock == "" || *udpFD < 0 || *tcpFD < 0 {
		return fmt.Errorf("_filter: -s, -u and -t are required")
	}
	if err := sandbox.KeepNetAdmin(); err != nil {
		return fmt.Errorf("_filter: %w", err)
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		logger.SetOutput(f)
	}
	pc, err := net.FilePacketConn(os.NewFile(uintptr(*udpFD), "dns-udp"))
	if err != nil {
		return fmt.Errorf("_filter: %w", err)
	}
	tl, err := net.FileListener(os.NewFile(uintptr(*tcpFD), "dns-tcp"))
	if err != nil {
		return fmt.Errorf("_filter: %w", err)
	}
	filter := &proxy.DNSFilter{
		Allow: proxy.Allowlist(allow),
		Relay: &proxy.DNSForwarder{Socket: *sock},
		Admit: (&sandbox.NFT{Exe: *nft}).Admit,
		Log:   logger,
	}
	go func() { _ = filter.ServeUDP(pc) }()
	go func() { _ = filter.ServeTCP(tl) }()
	// The parent-death signal normally ends this process; polling the
	// parent PID is the fallback.
	parent := os.Getppid()
	go func() {
		for range time.Tick(time.Second) {
			if os.Getppid() != parent {
				os.Exit(0)
			}
		}
	}()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	<-sigs
	return nil
}

// cmdForward runs inside the sandbox as the parent of the command, passing
// its exit status through. With -s it listens on loopback and relays to the
// proxy socket; with -u and -t it serves DNS on the inherited listeners
// bound by `box _netns`, relaying the queries to the proxy; with -p it
// keeps podman's pause process, whose PID it writes
// to the given file; each -w path=mount names a sync_files copy that is
// written back over its host original (bound at mount) once the command
// exits, unless the original changed on the host meanwhile: then the user
// is shown the difference and asked, and a copy kept aside lands in the
// host directory given with -W (bound at sandbox.SaveDir).
func cmdForward(argv []string) error {
	fs := flag.NewFlagSet("_forward", flag.ExitOnError)
	sock := fs.String("s", "", "proxy unix socket (no proxy when empty)")
	addr := fs.String("l", sandbox.ProxyAddr, "address to listen on")
	udpFD := fs.Int("u", -1, "inherited UDP DNS listener (none when negative)")
	tcpFD := fs.Int("t", -1, "inherited TCP DNS listener (none when negative)")
	pausePid := fs.String("p", "", "PID file of the podman pause process to keep (none when empty)")
	var syncArgs stringList
	fs.Var(&syncArgs, "w", "sync file as path=mount (repeatable)")
	saveDir := fs.String("W", "", "host directory bound at "+sandbox.SaveDir+" for sync file copies kept aside")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("_forward: missing command")
	}
	var files []sandbox.SyncFile
	for _, a := range syncArgs {
		sf, err := sandbox.ParseSyncFile(a)
		if err != nil {
			return fmt.Errorf("_forward: %w", err)
		}
		files = append(files, sf)
	}
	syncer := sandbox.NewSyncer(files)

	var ln net.Listener
	if *sock != "" {
		var err error
		if ln, err = net.Listen("tcp", *addr); err != nil {
			return fmt.Errorf("proxy forwarder: %w", err)
		}
		go func() { _ = proxy.Forward(ln, *sock) }()
	}
	var closers []io.Closer
	if *sock != "" && (*udpFD >= 0 || *tcpFD >= 0) {
		dns := &proxy.DNSForwarder{Socket: *sock}
		if *udpFD >= 0 {
			pc, err := net.FilePacketConn(os.NewFile(uintptr(*udpFD), "dns-udp"))
			if err != nil {
				return fmt.Errorf("dns forwarder: %w", err)
			}
			closers = append(closers, pc)
			go func() { _ = dns.ServeUDP(pc) }()
		}
		if *tcpFD >= 0 {
			tl, err := net.FileListener(os.NewFile(uintptr(*tcpFD), "dns-tcp"))
			if err != nil {
				return fmt.Errorf("dns forwarder: %w", err)
			}
			closers = append(closers, tl)
			go func() { _ = dns.ServeTCP(tl) }()
		}
		// FilePacketConn and FileListener work on duplicates; the
		// inherited descriptors must not reach the command.
		for _, fd := range []int{*udpFD, *tcpFD} {
			if fd >= 0 {
				syscall.CloseOnExec(fd)
			}
		}
	}

	// Without the pause process podman still runs, creating a namespace of
	// its own (noisily); the command is not held up by that.
	var pause *exec.Cmd
	if *pausePid != "" {
		self, err := os.Executable()
		if err == nil {
			pause, err = sandbox.StartPause(self, *pausePid)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "box: warning:", err)
		}
	}

	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Terminal-generated signals reach the child directly as part of the
	// foreground process group; the rest are forwarded.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			if detail := notFoundDetail(fs.Arg(0)); detail != "" {
				return fmt.Errorf("%w: %s", err, detail)
			}
		}
		return err
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range closers {
		_ = c.Close()
	}
	sandbox.StopPause(pause)
	conflicts, serr := syncer.WriteBack()
	if serr != nil {
		fmt.Fprintln(os.Stderr, "box:", serr)
	}
	if len(conflicts) > 0 {
		ask := isTerminal(int(os.Stdin.Fd())) && isTerminal(int(os.Stderr.Fd()))
		resolveConflicts(conflicts, ask, os.Stdin, os.Stderr, sandbox.SaveDir, *saveDir)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			os.Exit(128 + int(ws.Signal()))
		}
		os.Exit(ee.ExitCode())
	}
	return err
}

// notFoundDetail explains a command that $PATH does not yield inside the
// sandbox although an entry for it is there: a symlink whose target the
// sandbox does not see. That is what a persistent overlay shows after the
// program updated itself inside an earlier sandbox (the new version landed
// in the tmpfs $HOME, the symlink in the layer); box run checks for it on
// the host first, this is the fallback. Empty when there is nothing to add.
func notFoundDetail(name string) string {
	var dirs []string
	if strings.Contains(name, string(filepath.Separator)) {
		dirs = []string{""}
	} else {
		dirs = filepath.SplitList(os.Getenv("PATH"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if d == "" {
			p = name
		}
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			continue
		}
		return fmt.Sprintf("%s points at %s, which does not exist inside the sandbox", p, target)
	}
	return ""
}
