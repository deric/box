//go:build !linux

package sandbox

import "errors"

func applyLimits(string, string, Limits) ([]string, error) {
	return nil, errors.New("not supported on this platform")
}

// LimitsInfo describes how resource limits are applied on this host.
func LimitsInfo() string {
	return "not supported on this platform"
}
