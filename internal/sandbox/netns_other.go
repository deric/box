//go:build !linux

package sandbox

import "errors"

// DNSAddr is where the DNS listeners are bound inside a Linux sandbox.
const DNSAddr = "127.0.0.1:53"

var lookupUnshare = func() (string, error) { return "", errors.New("not supported on this platform") }

var probeNetns = func(unshare, self string) error { return errors.New("not supported on this platform") }

// PrepareDNS is only used on Linux.
func PrepareDNS(udpFD, tcpFD int) error { return errors.New("not supported on this platform") }

// ErrNoUnshare is returned by DNSStatus when unshare(1) is not installed.
var ErrNoUnshare = errors.New("unshare not found; install util-linux")

// DNSStatus checks that DNS can be provided to sandboxes; it cannot on
// this platform.
func DNSStatus() (unshare, version string, err error) {
	return "", "", errors.New("not supported on this platform")
}

// DNSInfo describes how DNS is provided to sandboxes on this host.
func DNSInfo() string { return "not supported on this platform" }
