//go:build linux || darwin

package main

import (
	"golang.org/x/sys/unix"
)

// isTerminal reports whether fd refers to a terminal.
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	return err == nil
}

// rawMode switches the terminal on fd to raw input (no echo, no line
// buffering, no signal keys) and returns a function restoring the previous
// settings. Output processing is left on, so "\n" still moves to the start
// of the next line.
func rawMode(fd int) (restore func(), err error) {
	return setRaw(fd, true)
}

// passthroughMode is rawMode with output processing off as well: bytes
// written to fd reach the terminal unchanged, for relaying the output of
// another terminal that has done the processing already (see runOnPty).
func passthroughMode(fd int) (restore func(), err error) {
	return setRaw(fd, false)
}

func setRaw(fd int, opost bool) (restore func(), err error) {
	old, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	if !opost {
		t.Oflag &^= unix.OPOST
	}
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &t); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, old) }, nil
}

// copyTermios applies the settings of the terminal on from to the one on to.
func copyTermios(from, to int) error {
	t, err := unix.IoctlGetTermios(from, ioctlReadTermios)
	if err != nil {
		return err
	}
	return unix.IoctlSetTermios(to, ioctlWriteTermios, t)
}

// copyWinsize gives the terminal on to the size of the one on from. On the
// master of a pseudo-terminal it sizes the pair, and the kernel sends
// SIGWINCH to the foreground process group of the slave when the size
// changed.
func copyWinsize(from, to int) error {
	ws, err := unix.IoctlGetWinsize(from, unix.TIOCGWINSZ)
	if err != nil {
		return err
	}
	return unix.IoctlSetWinsize(to, unix.TIOCSWINSZ, ws)
}

// termSize returns the terminal's width and height in cells.
func termSize(fd int) (width, height int, err error) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}
