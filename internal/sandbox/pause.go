package sandbox

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// StopPause ends a pause process started by StartPause and reaps it.
func StopPause(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// Pause is the body of `box _pause`: it sleeps until killed. Terminal
// signals are ignored, so that a ^C meant for the sandboxed command does
// not take podman's namespace with it; SIGTERM ends it.
func Pause() {
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	<-sigs
}
