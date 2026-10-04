package procs

import (
	"os"
	"os/exec"
	"testing"
)

func TestSandboxesDarwin(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Env = append(os.Environ(), "BOX_NAME=box-test", "BOX_DIR=/box/test")
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
			if s.Name != "box-test" || s.Dir != "/box/test" || s.Procs != 1 || len(s.Command) != 2 {
				t.Errorf("unexpected sandbox %+v", s)
			}
			return
		}
	}
	t.Errorf("sandbox %d not listed in %+v", cmd.Process.Pid, list)
}
