//go:build !linux

package sandbox

import (
	"errors"
	"runtime"
)

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

// DNSInfo describes how DNS is provided to sandboxes on this host: on
// macOS a sandbox behind the proxy talks to the system resolver when dns
// is set (see SeatbeltProxyRules); elsewhere there is no sandbox.
func DNSInfo() string {
	if runtime.GOOS == "darwin" {
		return "the system resolver (mDNSResponder), reachable with dns = true"
	}
	return "not supported on this platform"
}

// PastaSetup is what `box _netns -pasta` needs on Linux.
type PastaSetup struct {
	Self, Socket, Log, NFT string
	Allow                  []string
}

// PreparePasta, ProbePasta, KeepNetAdmin and RunPasta are only used on
// Linux.
func PreparePasta(PastaSetup) ([]string, error) {
	return nil, errors.New("not supported on this platform")
}
func ProbePasta(string, bool) error { return errors.New("not supported on this platform") }
func KeepNetAdmin() error           { return errors.New("not supported on this platform") }
func RunPasta(string, int) error    { return errors.New("not supported on this platform") }

// PastaStatus checks that the pasta backend can be provided; it cannot on
// this platform.
func PastaStatus() (pasta, nft, version string, err error) {
	return "", "", "", errors.New("not supported on this platform")
}

// PastaInfo describes how the pasta backend stands on this host.
func PastaInfo() string { return "not supported on this platform" }
