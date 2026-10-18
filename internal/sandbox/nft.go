package sandbox

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The firewall of a pasta sandbox (see Pasta). It lives in the sandbox's
// network namespace as an nftables table that rejects every outgoing
// connection except to loopback, to the link-local and multicast IPv6
// addresses neighbour discovery needs, and to the addresses in its allow
// sets, which box's resolver fills with what the allowed names resolve
// to, each for a limited time. Established connections keep going once
// admitted. The ruleset is loaded, and the sets are updated, with nft(8)
// run inside the namespace, where box holds CAP_NET_ADMIN.

// nftRuleset is the firewall loaded into a pasta sandbox's namespace.
const nftRuleset = `table inet box {
	set allow4 {
		type ipv4_addr
		flags timeout
	}
	set allow6 {
		type ipv6_addr
		flags timeout
	}
	chain output {
		type filter hook output priority filter; policy drop;
		ct state established,related accept
		ip daddr 127.0.0.0/8 accept
		ip6 daddr ::1 accept
		ip6 daddr fe80::/10 accept
		ip6 daddr ff02::/16 accept
		ip daddr @allow4 accept
		ip6 daddr @allow6 accept
		reject
	}
}
`

// Admitted addresses stay reachable for their DNS time to live, but for
// minAdmit at least, as clients cache a resolved address well beyond a
// short TTL, and for maxAdmit at most, to keep the sets bounded.
const (
	minAdmit = 15 * time.Minute
	maxAdmit = 24 * time.Hour
)

// NFT manages the firewall through the nft executable at Exe (see above).
type NFT struct {
	Exe string
	// run executes an nft script; tests replace it.
	run func(exe, script string) error

	mu      sync.Mutex
	expires map[netip.Addr]time.Time
}

// Load installs the ruleset into the current network namespace.
func (n *NFT) Load() error {
	return n.exec(nftRuleset)
}

// Admit lets addr through the firewall for ttl, within the minAdmit and
// maxAdmit bounds. An address admitted already is left alone while more
// than half of the new period remains, else its period is renewed.
func (n *NFT) Admit(addr netip.Addr, ttl time.Duration) error {
	timeout := admitFor(ttl)
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if until, ok := n.expires[addr]; ok && until.Sub(now) > timeout/2 {
		return nil
	}
	err := n.exec(admitScript(addr, timeout, n.expires[addr].After(now)))
	if err != nil && n.expires[addr].After(now) {
		// The element may have expired between the check and now.
		err = n.exec(admitScript(addr, timeout, false))
	}
	if err != nil {
		return err
	}
	if n.expires == nil {
		n.expires = map[netip.Addr]time.Time{}
	}
	n.expires[addr] = now.Add(timeout)
	for a, until := range n.expires {
		if until.Before(now) {
			delete(n.expires, a)
		}
	}
	return nil
}

// admitFor bounds a DNS time to live to the admission period.
func admitFor(ttl time.Duration) time.Duration {
	return min(max(ttl, minAdmit), maxAdmit)
}

// admitScript is the nft script that adds addr to its set for timeout;
// with renew, the element is deleted first so the period starts afresh (a
// plain add leaves an existing element's expiry alone on older kernels).
func admitScript(addr netip.Addr, timeout time.Duration, renew bool) string {
	set := "allow6"
	if addr.Unmap().Is4() {
		set, addr = "allow4", addr.Unmap()
	}
	var b strings.Builder
	if renew {
		fmt.Fprintf(&b, "delete element inet box %s { %s }\n", set, addr)
	}
	fmt.Fprintf(&b, "add element inet box %s { %s timeout %ds }\n", set, addr, int64(timeout/time.Second))
	return b.String()
}

func (n *NFT) exec(script string) error {
	run := n.run
	if run == nil {
		run = runNFT
	}
	return run(n.Exe, script)
}

// runNFT feeds script to `nft -f -`.
func runNFT(exe, script string) error {
	cmd := exec.Command(exe, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("nft: %s", strings.ReplaceAll(msg, "\n", "; "))
	}
	return nil
}
