package sandbox

import (
	"errors"
	"os/exec"
	"strconv"
)

// The pasta network backend (pasta = true).
//
// Instead of the HTTP proxy, the sandbox gets a network interface of its
// own from pasta(1), which translates its traffic into ordinary sockets on
// the host, so that any protocol works inside: ssh, git over ssh, gRPC,
// QUIC, ... The allowlist is enforced by name resolution. As with DNS, box
// creates the network namespace itself (`unshare -U -n`, see DNS) and
// `box _netns` prepares it before bwrap starts: it loads the firewall (an
// nftables ruleset that rejects every outgoing connection except to
// loopback and to the addresses in its allow sets, see NFT), asks the
// proxy process on the host to attach pasta to the namespace (the proxy
// is where pasta has to be started from, see proxy.PastaPath), and starts
// `box _filter`, the resolver, in the namespace with CAP_NET_ADMIN kept:
// it serves DNS on 127.0.0.1:53, answers only queries for names on
// allow_hosts, relaying them to the host's resolver through the proxy,
// and admits the addresses it hands out into the allow sets for their
// time to live (see proxy.DNSFilter). Every other name gets NXDOMAIN and a
// line in the log, which `box top` counts like a denied request; a
// connection to an address that was never resolved is rejected. The
// sandboxed program runs unprivileged in bwrap's nested user namespace,
// which has no say over the firewall. pasta is told to forward no ports in
// either direction and not to map the host's loopback, so nothing on the
// host is reachable and nothing inside is exposed. The proxy still runs on
// the host for the resolver's queries, but its socket is not bound into
// the sandbox and no proxy variables are set. Where pasta, nft or the
// namespaces are not available, the sandbox falls back to the proxy with
// a warning.

// Pasta describes the pasta backend of a sandbox (see above).
type Pasta struct {
	Exe string // pasta on the host
	NFT string // nft, run inside the namespace by box
}

// lookupPasta and lookupNFT find the executables; tests replace them.
var (
	lookupPasta = func() (string, error) { return exec.LookPath("pasta") }
	lookupNFT   = func() (string, error) { return exec.LookPath("nft") }
)

// Errors of PastaStatus.
var (
	ErrNoPasta = errors.New("pasta not found; install passt")
	ErrNoNFT   = errors.New("nft not found; install nftables")
)

// PastaArgs returns the pasta arguments that attach it to the network and
// user namespaces of process pid and configure the interface there with
// the host's addresses and routes. No port is forwarded in either
// direction (pasta's default is to forward every port found bound) and
// the gateway address is not mapped to the host's loopback.
func PastaArgs(pid int) []string {
	ns := "/proc/" + strconv.Itoa(pid) + "/ns/"
	return []string{
		"--quiet", "--config-net",
		"--netns", ns + "net", "--userns", ns + "user",
		"--tcp-ports", "none", "--udp-ports", "none",
		"--tcp-ns", "none", "--udp-ns", "none",
		"--no-map-gw",
	}
}

// collectPasta sets the plan up for the pasta backend and returns the
// mount of the sandbox's resolv.conf. When pasta, nft or the namespaces
// are out of reach it reports false, with a warning, and the sandbox uses
// the proxy instead.
func collectPasta(opts *Options, plan *Plan) (*mount, bool) {
	const prefix = "pasta disabled, using the proxy: "
	pasta, err := lookupPasta()
	if err != nil {
		plan.Warnings = append(plan.Warnings, prefix+ErrNoPasta.Error())
		return nil, false
	}
	nft, err := lookupNFT()
	if err != nil {
		plan.Warnings = append(plan.Warnings, prefix+ErrNoNFT.Error())
		return nil, false
	}
	unshare, ok := prepareNetns(opts, plan, prefix)
	if !ok {
		return nil, false
	}
	plan.DNS = &DNS{Unshare: unshare, UDPFD: -1, TCPFD: -1}
	plan.Pasta = &Pasta{Exe: pasta, NFT: nft}
	plan.Self = opts.Self
	return resolvConfMount(plan, "# Written by box: its resolver answers for allow_hosts and admits their addresses."), true
}
