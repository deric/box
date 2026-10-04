package procs

import "box/internal/sandbox"

// Sandboxes lists the running box sandboxes.
func Sandboxes() ([]Sandbox, error) {
	return List("/proc", sandbox.ProcTitlePrefix)
}
