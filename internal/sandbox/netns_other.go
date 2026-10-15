//go:build !linux

package sandbox

import "errors"

// DNSAddr is where the DNS listeners are bound inside a Linux sandbox.
const DNSAddr = "127.0.0.1:53"

var lookupUnshare = func() (string, error) { return "", errors.New("not supported on this platform") }

var probeNetns = func(unshare, self string) error { return errors.New("not supported on this platform") }

func unshareArgs(uid, gid int) []string { return nil }

// PrepareDNS is only used on Linux.
func PrepareDNS(udpFD, tcpFD int) error { return errors.New("not supported on this platform") }

// DNSInfo describes how DNS is provided to sandboxes on this host.
func DNSInfo() string { return "not supported on this platform" }
