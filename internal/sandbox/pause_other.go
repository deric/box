//go:build !linux

package sandbox

import (
	"errors"
	"os/exec"
)

// StartPause fails: podman sandboxes exist on Linux only.
func StartPause(self, pidFile string) (*exec.Cmd, error) {
	return nil, errors.New("podman pause process: not supported on this platform")
}
