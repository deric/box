package main

import (
	"fmt"
	"os"
	"strconv"

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
	if err := unix.IoctlSetPointerInt(m, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("unlocking pty: %w", err)
	}
	n, err := unix.IoctlGetUint32(m, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("pty number: %w", err)
	}
	name := "/dev/pts/" + strconv.Itoa(int(n))
	s, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(m)
		return nil, 0, nil, fmt.Errorf("%s: %w", name, err)
	}
	return os.NewFile(uintptr(m), "/dev/ptmx"), m, os.NewFile(uintptr(s), name), nil
}
