//go:build !linux && !darwin

package main

import (
	"fmt"
	"runtime"
)

func isTerminal(int) bool { return false }

func rawMode(int) (func(), error) {
	return nil, fmt.Errorf("terminal control is not supported on %s", runtime.GOOS)
}

func termSize(int) (int, int, error) {
	return 0, 0, fmt.Errorf("terminal control is not supported on %s", runtime.GOOS)
}
