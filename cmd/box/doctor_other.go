//go:build !linux

package main

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/deric/box/internal/sandbox"
)

// platformChecks covers sandbox-exec on macOS, which ships with the
// system: it is looked up and asked to run a trivial program under a
// permissive profile. DNS, limits, podman and ssh are Linux features and
// are not checked.
func platformChecks(features) []check {
	r := sandbox.LookupRunner()
	if r.Exe == "" {
		fix := "install bubblewrap; box supports Linux (bwrap) and macOS (sandbox-exec)"
		if runtime.GOOS == "darwin" {
			fix = "sandbox-exec ships with every macOS release at /usr/bin/sandbox-exec; check that /usr/bin is in PATH"
		}
		return []check{{r.Name, statusFail, "not found in PATH", fix}}
	}
	checks := []check{{r.Name, statusOK, r.Mechanism + " at " + r.Exe, ""}}
	if r.Name != "sandbox-exec" {
		return checks
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.Exe, "-p", "(version 1) (allow default)", "/usr/bin/true").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return append(checks, check{"sandbox", statusFail, "sandbox-exec cannot run a program: " + errors.New(msg).Error(),
			"sandbox-exec is deprecated by Apple but still works on every release; check Console.app for sandbox denials"})
	}
	return append(checks, check{"sandbox", statusOK, "sandbox-exec can run a program", ""})
}
