package main

import (
	"os"
	"path/filepath"

	"github.com/deric/box/internal/sandbox"
)

// cmdProbe is `box _probe`, run by box doctor inside an empty sandbox: it
// starts podman's pause process the way the forwarder does (see
// sandbox.StartPause) and ends it again, so that a host where the nested
// user and mount namespace cannot be created is reported before podman
// trips over it.
func cmdProbe(argv []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "box-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pause, err := sandbox.StartPause(self, filepath.Join(dir, "pause.pid"))
	if err != nil {
		return err
	}
	sandbox.StopPause(pause)
	return nil
}
