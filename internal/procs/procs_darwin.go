package procs

import (
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"

	"github.com/deric/box/internal/sandbox"
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
	return group(all, envTag(sandbox.NameEnv, sandbox.BinaryEnv, sandbox.DirEnv)), nil
}

// IsRunning reports whether pid is a live box sandbox of the given binary,
// recognized by the environment box sets, as in Sandboxes.
func IsRunning(pid int, binary string) bool {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return false
	}
	_, env := parseProcArgs(buf)
	s, ok := envTag(sandbox.NameEnv, sandbox.BinaryEnv, sandbox.DirEnv)(&proc{env: env})
	return ok && s.Binary == binary
}
