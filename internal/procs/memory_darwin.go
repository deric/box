package procs

import "golang.org/x/sys/unix"

// TotalMemory returns the amount of physical memory in bytes, or 0 when it
// cannot be determined.
func TotalMemory() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
}
