//go:build linux || darwin

package main

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestRunOnPtyResize runs a shell through the relay with a pseudo-terminal
// standing in for the host's, resizes that one and checks that the shell
// got SIGWINCH and sees the new size, and that the exit status comes back.
func TestRunOnPtyResize(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty not installed")
	}
	host, hostFd, term, err := openPty()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	defer func() { _ = host.Close() }()
	defer func() { _ = term.Close() }()
	if err := unix.IoctlSetWinsize(hostFd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := runOnPty("/bin/sh", []string{"-c",
			`trap 'echo GOT-WINCH' WINCH; stty size; sleep 1; stty size; exit 3`},
			term, term, term)
		done <- result{code, err}
	}()

	// Resize the host terminal the way a terminal emulator does, then tell
	// box as the kernel would; it is not in the foreground of that one.
	time.Sleep(300 * time.Millisecond)
	if err := unix.IoctlSetWinsize(hostFd, unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	buf := make([]byte, 4096)
	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, err := host.Read(buf)
		out.Write(buf[:n])
		if err != nil || strings.Contains(out.String(), "30 100") {
			break
		}
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("runOnPty: %v\noutput: %q", res.err, out.String())
	}
	if res.code != 3 {
		t.Errorf("exit status %d, want 3", res.code)
	}
	got := strings.ReplaceAll(out.String(), "\r", "")
	want := "24 80\nGOT-WINCH\n30 100\n"
	if got != want {
		t.Errorf("output %q, want %q", got, want)
	}
	// The host terminal is left as it was.
	tio, err := unix.IoctlGetTermios(int(term.Fd()), ioctlReadTermios)
	if err != nil {
		t.Fatal(err)
	}
	if tio.Lflag&unix.ICANON == 0 || tio.Oflag&unix.OPOST == 0 {
		t.Errorf("host terminal left in raw mode: %+v", tio)
	}
}
