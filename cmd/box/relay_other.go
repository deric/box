//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
	"runtime"
)

var errNoPty = errors.New("pseudo-terminals are not supported on " + runtime.GOOS)

func runOnPty(string, []string, *os.File, *os.File, *os.File) (int, error) {
	return 0, errNoPty
}
