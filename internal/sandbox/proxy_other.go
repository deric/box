//go:build !linux

package sandbox

import "syscall"

// dieWithParent is a no-op: the proxy is only started on Linux, where the
// parent-death signal exists.
func dieWithParent() *syscall.SysProcAttr { return nil }
