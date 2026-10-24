package procs

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/deric/box/internal/sandbox"
)

// helperEnv marks a run of the test binary that only sleeps, standing in
// for a sandboxed program.
const helperEnv = "BOX_PROCS_TEST_HELPER"

func TestSandboxesDarwin(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	// Inside a box sandbox the test process is itself tagged, so Sandboxes
	// folds the child started below into the enclosing sandbox's tree
	// rather than listing it on its own.
	if name := os.Getenv(sandbox.NameEnv); name != "" {
		t.Skipf("running inside box sandbox %q; the test process is itself tagged", name)
	}
	// The child is this test binary, not /bin/sleep: since macOS 15.7
	// kern.procargs2 hides the environment of Apple's platform binaries
	// from unprivileged callers, so their tags would not be visible.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestSandboxesDarwin$")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "BOX_NAME=box-test", "BOX_BINARY=sleep", "BOX_DIR=/box/test")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	list, err := Sandboxes()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.PID == cmd.Process.Pid {
			if s.Name != "box-test" || s.Binary != "sleep" || s.Dir != "/box/test" || s.Procs != 1 || len(s.Command) != 2 {
				t.Errorf("unexpected sandbox %+v", s)
			}
			return
		}
	}
	t.Errorf("sandbox %d not listed in %+v", cmd.Process.Pid, list)
}
