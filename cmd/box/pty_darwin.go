package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPty allocates a pseudo-terminal and returns its master and slave. The
// master is opened non-blocking so that the file is pollable and its read
// deadline works; fd is its descriptor, for ioctls (File.Fd would put the
// file back into blocking mode).
func openPty() (master *os.File, fd int, slave *os.File, err error) {
	m, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("/dev/ptmx: %w", err)
	}
	// grantpt, unlockpt and ptsname, as libc does them.
	if err := unix.IoctlSetInt(m, unix.TIOCPTYGRANT, 0); err != nil {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("granting pty: %w", err)
	}
	if err := unix.IoctlSetInt(m, unix.TIOCPTYUNLK, 0); err != nil {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("unlocking pty: %w", err)
	}
	var buf [128]byte // TIOCPTYGNAME fills in up to 128 bytes
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(m), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("pty name: %w", errno)
	}
	name := unix.ByteSliceToString(buf[:])
	s, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("%s: %w", name, err)
	}
	return os.NewFile(uintptr(m), "/dev/ptmx"), m, os.NewFile(uintptr(s), name), nil
}
