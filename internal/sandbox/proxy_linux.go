package sandbox

import "syscall"

// dieWithParent makes the proxy child receive SIGTERM when the thread that
// forked it (which goes on to exec the runner) exits.
func dieWithParent() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
