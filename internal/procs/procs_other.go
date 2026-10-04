//go:build !linux && !darwin

package procs

import (
	"fmt"
	"runtime"
)

// Sandboxes lists the running box sandboxes.
func Sandboxes() ([]Sandbox, error) {
	return nil, fmt.Errorf("listing sandboxes is not supported on %s", runtime.GOOS)
}

// IsRunning reports whether pid is a live box sandbox for binary name. Only
// Linux sandboxes own private /tmp directories, so nothing is running here.
func IsRunning(int, string) bool { return false }
