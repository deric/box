package procs

import "github.com/deric/box/internal/sandbox"

// Sandboxes lists the running box sandboxes.
func Sandboxes() ([]Sandbox, error) {
	return List("/proc", sandbox.ProcTitlePrefix)
}

// IsRunning reports whether pid is a live box sandbox of the given binary.
func IsRunning(pid int, binary string) bool {
	return Running("/proc", sandbox.ProcTitlePrefix, pid, binary)
}
