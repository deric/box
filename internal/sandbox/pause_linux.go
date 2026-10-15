package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// StartPause starts podman's pause process inside the sandbox: `self _pause`
// in a new user namespace that maps the current user and group to root (the
// only mapping an unprivileged process may write) and a new mount namespace,
// which podman joins on every invocation. Its PID is written to pidFile
// (see PausePidFile). The process is detached from the terminal and is
// killed when the thread that started it (the forwarder's) exits; Stop ends
// it earlier.
func StartPause(self, pidFile string) (*exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o700); err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "_pause")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		// setgroups is denied, as it must be for an unprivileged gid map.
		GidMappingsEnableSetgroups: false,
		Setsid:                     true,
		Pdeathsig:                  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("podman pause process: %w", err)
	}
	// Podman refuses to overwrite an existing file; the runtime directory
	// is a fresh tmpfs, so there is none unless the forwarder ran twice.
	f, err := os.OpenFile(pidFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = f.WriteString(strconv.Itoa(cmd.Process.Pid))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		StopPause(cmd)
		return nil, fmt.Errorf("podman pause process: %w", err)
	}
	return cmd, nil
}
