package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
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
	srv := &proxy.Server{Allow: proxy.Allowlist(allow), Log: logger}
	return srv.Serve(ln)
}

// cmdForward runs inside the sandbox as the parent of the command, passing
// its exit status through. With -s it listens on loopback and relays to the
// proxy socket; each -w path=mount names a sync_files copy that is written
// back over its host original (bound at mount) once the command exits.
func cmdForward(argv []string) error {
	fs := flag.NewFlagSet("_forward", flag.ExitOnError)
	sock := fs.String("s", "", "proxy unix socket (no proxy when empty)")
	addr := fs.String("l", sandbox.ProxyAddr, "address to listen on")
	var syncArgs stringList
	fs.Var(&syncArgs, "w", "sync file as path=mount (repeatable)")
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

	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Terminal-generated signals reach the child directly as part of the
	// foreground process group; the rest are forwarded.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)
	if err := cmd.Start(); err != nil {
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
	warnings, serr := syncer.WriteBack()
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "box: warning:", w)
	}
	if serr != nil {
		fmt.Fprintln(os.Stderr, "box:", serr)
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
