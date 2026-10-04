package procs

import (
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"

	"box/internal/sandbox"
)

// Sandboxes lists the running box sandboxes. sandbox-exec replaces itself
// with the sandboxed program, so sandboxes are recognized by the environment
// variables box sets rather than by a wrapper process.
func Sandboxes() ([]Sandbox, error) {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,ppid=,rss=,time=,etime=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	all := parsePS(out, time.Now())
	for pid, p := range all {
		buf, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil {
			continue // exited, or not ours to read
		}
		p.argv, p.env = parseProcArgs(buf)
	}
	return group(all, envTag(sandbox.NameEnv, sandbox.DirEnv)), nil
}

// IsRunning reports whether pid is a live box sandbox for binary name. Only
// Linux sandboxes own private /tmp directories, so nothing is running here.
func IsRunning(int, string) bool { return false }
