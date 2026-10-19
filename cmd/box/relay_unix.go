//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// errNoPty is returned by runOnPty when no pseudo-terminal can be
// allocated; the command can still run on the host's terminal.
var errNoPty = errors.New("cannot allocate a pseudo-terminal")

// runOnPty runs exe with argv on a pseudo-terminal of its own and relays
// the terminal box was started on (in, out) to it: input is passed through
// byte for byte, output copied back, and the size of the terminal copied
// over whenever it changes (SIGWINCH). The command starts as the leader of
// a new session with the pseudo-terminal as its controlling terminal, so
// the kernel delivers SIGWINCH to it on resize, turns ^C into SIGINT for it
// and lets it open /dev/tty, as if it ran on a terminal of the host. errOut
// goes to the pseudo-terminal too when it is a terminal and is passed on
// as is otherwise. The exit status of the command is returned: 128 plus
// the signal number when a signal ended it.
func runOnPty(exe string, argv []string, in, out, errOut *os.File) (code int, err error) {
	inFd, outFd := int(in.Fd()), int(out.Fd())
	master, mfd, slave, err := openPty()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errNoPty, err)
	}
	defer func() { _ = master.Close() }()

	// The new terminal starts out like the host's: same settings, same size.
	if err := copyTermios(inFd, int(slave.Fd())); err != nil {
		_ = slave.Close()
		return 0, fmt.Errorf("%w: %v", errNoPty, err)
	}
	if err := copyWinsize(outFd, mfd); err != nil {
		_ = slave.Close()
		return 0, fmt.Errorf("%w: %v", errNoPty, err)
	}

	cmd := exec.Command(exe, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if !isTerminal(int(errOut.Fd())) {
		cmd.Stderr = errOut
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	// The child holds the slave from here on. Not keeping it open here is
	// what makes reading the master end (EIO) once the sandbox is gone.
	_ = slave.Close()
	if err != nil {
		return 0, err
	}

	r := &relay{in: in, inFd: inFd, outFd: outFd, master: master, mfd: mfd, child: cmd.Process}
	if r.restore, err = passthroughMode(inFd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, err
	}
	defer r.restore()
	r.sigs = make(chan os.Signal, 16)
	signal.Notify(r.sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGCONT)
	defer signal.Stop(r.sigs)
	go r.signals()
	go r.input()
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, master)
		close(drained)
	}()

	werr := cmd.Wait()
	// What the sandbox wrote last may still be in the master: read it to
	// the end, or for a second should something keep the slave open.
	_ = master.SetReadDeadline(time.Now().Add(time.Second))
	<-drained
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return 0, werr
}

// relay is the state of runOnPty shared by its goroutines.
type relay struct {
	in      *os.File // the host terminal, read for input
	inFd    int
	outFd   int      // the host terminal, measured for its size
	master  *os.File // the sandbox's terminal, written and read on its master side
	mfd     int
	child   *os.Process
	sigs    chan os.Signal
	mu      sync.Mutex // serializes suspend and resume
	restore func()     // puts the host terminal back into its original mode
}

// signals handles the signals box receives while relaying: a resize is
// passed on as the new size, SIGTSTP suspends box, SIGCONT resumes it and
// the rest go to the child. Those a terminal generates (^C, ^Z, ...) do
// not come this way: the host terminal is in raw mode, and the sandbox's
// terminal generates them for the sandbox.
func (r *relay) signals() {
	for s := range r.sigs {
		switch s {
		case syscall.SIGWINCH:
			r.resize()
		case syscall.SIGTSTP:
			r.suspend()
		case syscall.SIGCONT:
			r.resume()
		default:
			_ = r.child.Signal(s)
		}
	}
}

func (r *relay) resize() {
	_ = copyWinsize(r.outFd, r.mfd)
}

// suspend stops box as ^Z would without the relay, with the host terminal
// restored for the shell: the process group gets SIGTSTP with its default
// action, which resume undoes on SIGCONT.
func (r *relay) suspend() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restore()
	signal.Reset(syscall.SIGTSTP)
	_ = syscall.Kill(0, syscall.SIGTSTP)
}

// resume puts the host terminal back into raw mode after a suspend and
// passes on the size it may have been given meanwhile.
func (r *relay) resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	signal.Notify(r.sigs, syscall.SIGTSTP)
	if restore, err := passthroughMode(r.inFd); err == nil {
		r.restore = restore
	}
	r.resize()
}

// input copies what is typed on the host terminal to the sandbox's. A
// suspend character the sandbox would be stopped by for good suspends box
// instead (see suspendAt).
func (r *relay) input() {
	buf := make([]byte, 4096)
	for {
		n, err := r.in.Read(buf)
		data := buf[:n]
		for len(data) > 0 {
			i := r.suspendAt(data)
			if i < 0 {
				if _, err := r.master.Write(data); err != nil {
					return
				}
				break
			}
			if _, err := r.master.Write(data[:i]); err != nil {
				return
			}
			r.suspend()
			data = data[i+1:]
		}
		if err != nil {
			return
		}
	}
}

// suspendAt returns the index in data of a suspend character that would
// stop the sandboxed program with nothing left to resume it, or -1. The
// sandbox's terminal turns its VSUSP character into SIGTSTP while ISIG is
// set, and only a shell doing job control inside could resume the stopped
// program; such a shell takes the terminal's foreground process group over,
// so while that is still the one the sandbox started in, the character
// suspends box itself, as typing it would without the relay, and fg
// resumes everything. Programs that take the terminal raw (editors, Claude
// Code) get the character as they would on any terminal.
func (r *relay) suspendAt(data []byte) int {
	t, err := unix.IoctlGetTermios(r.mfd, ioctlReadTermios)
	if err != nil || t.Lflag&unix.ISIG == 0 {
		return -1
	}
	susp := t.Cc[unix.VSUSP]
	if susp == 0 || susp == 0xff { // _POSIX_VDISABLE on Linux and macOS
		return -1
	}
	if pgrp, err := unix.IoctlGetInt(r.mfd, unix.TIOCGPGRP); err != nil || pgrp != r.child.Pid {
		return -1
	}
	return bytes.IndexByte(data, susp)
}
