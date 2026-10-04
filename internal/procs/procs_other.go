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
