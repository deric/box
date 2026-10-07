package procs

import "github.com/deric/box/internal/sandbox"

// Sandboxes lists the running box sandboxes.
func Sandboxes() ([]Sandbox, error) {
	return List("/proc", sandbox.ProcTitlePrefix)
}

// IsRunning reports whether pid is a live box sandbox for binary name.
func IsRunning(pid int, name string) bool {
	return Running("/proc", sandbox.ProcTitlePrefix, pid, name)
}
