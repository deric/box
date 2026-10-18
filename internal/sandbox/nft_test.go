package sandbox

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestNFTRuleset(t *testing.T) {
	// The ruleset rejects by default, keeps established flows, lets
	// loopback and neighbour discovery through and consults both sets.
	for _, want := range []string{
		"table inet box {", "set allow4 {", "set allow6 {", "flags timeout",
		"type filter hook output priority filter; policy drop;",
		"ct state established,related accept",
		"ip daddr 127.0.0.0/8 accept", "ip6 daddr ::1 accept",
		"ip6 daddr fe80::/10 accept", "ip6 daddr ff02::/16 accept",
		"ip daddr @allow4 accept", "ip6 daddr @allow6 accept",
		"\n\t\treject\n",
	} {
		if !strings.Contains(nftRuleset, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	if strings.Index(nftRuleset, "@allow6 accept") > strings.Index(nftRuleset, "reject") {
		t.Error("the set rules must come before the reject")
	}
}

func TestAdmitFor(t *testing.T) {
	for ttl, want := range map[time.Duration]time.Duration{
		0:               minAdmit,
		time.Second:     minAdmit,
		time.Hour:       time.Hour,
		48 * time.Hour:  maxAdmit,
		minAdmit:        minAdmit,
		minAdmit + 1:    minAdmit + 1,
		7 * time.Minute: minAdmit,
	} {
		if got := admitFor(ttl); got != want {
			t.Errorf("admitFor(%s) = %s, want %s", ttl, got, want)
		}
	}
}

func TestAdmitScript(t *testing.T) {
	v4 := netip.MustParseAddr("140.82.121.3")
	if got, want := admitScript(v4, 900*time.Second, false), "add element inet box allow4 { 140.82.121.3 timeout 900s }\n"; got != want {
		t.Errorf("v4 = %q, want %q", got, want)
	}
	// A mapped IPv4 address goes to the IPv4 set unmapped.
	if got := admitScript(netip.MustParseAddr("::ffff:140.82.121.3"), 900*time.Second, false); !strings.Contains(got, "allow4 { 140.82.121.3 ") {
		t.Errorf("mapped v4 = %q", got)
	}
	v6 := netip.MustParseAddr("2606:50c0:8000::153")
	got := admitScript(v6, time.Hour, true)
	want := "delete element inet box allow6 { 2606:50c0:8000::153 }\nadd element inet box allow6 { 2606:50c0:8000::153 timeout 3600s }\n"
	if got != want {
		t.Errorf("renewed v6 = %q, want %q", got, want)
	}
}

func TestNFTAdmit(t *testing.T) {
	var scripts []string
	fail := false
	n := &NFT{Exe: "/usr/sbin/nft", run: func(exe, script string) error {
		if exe != "/usr/sbin/nft" {
			t.Errorf("exe = %s", exe)
		}
		scripts = append(scripts, script)
		if fail {
			return errors.New("nft: Error: Could not process rule: No such file or directory")
		}
		return nil
	}}
	if err := n.Load(); err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 1 || scripts[0] != nftRuleset {
		t.Fatalf("Load ran %q", scripts)
	}
	scripts = nil

	addr := netip.MustParseAddr("192.0.2.1")
	if err := n.Admit(addr, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 1 || scripts[0] != "add element inet box allow4 { 192.0.2.1 timeout 900s }\n" {
		t.Errorf("first Admit ran %q", scripts)
	}
	// Admitted a moment ago with most of the period left: nothing to do.
	if err := n.Admit(addr, time.Minute); err != nil || len(scripts) != 1 {
		t.Errorf("second Admit: %v, ran %q", err, scripts)
	}
	// With less than half the period left (the period has grown, here)
	// the element is renewed: deleted and added again.
	if err := n.Admit(addr, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 2 || !strings.HasPrefix(scripts[1], "delete element inet box allow4 { 192.0.2.1 }\nadd element") {
		t.Errorf("renewal ran %q", scripts)
	}
	// An address that has expired in the kernel already cannot be
	// deleted: the plain add is retried.
	n.expires[addr] = time.Now().Add(time.Second)
	scripts = nil
	calls := 0
	n.run = func(exe, script string) error {
		calls++
		scripts = append(scripts, script)
		if strings.HasPrefix(script, "delete") {
			return errors.New("nft: Error: Could not process rule: No such file or directory")
		}
		return nil
	}
	if err := n.Admit(addr, time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.HasPrefix(scripts[0], "delete") || !strings.HasPrefix(scripts[1], "add element") {
		t.Errorf("retry ran %q", scripts)
	}
	// A failure is reported and leaves the address unrecorded.
	v6 := netip.MustParseAddr("2001:db8::1")
	n.run = func(string, string) error { return errors.New("nft: boom") }
	if err := n.Admit(v6, time.Minute); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Admit error = %v", err)
	}
	if _, ok := n.expires[v6]; ok {
		t.Error("failed admission recorded")
	}
}

func TestPastaArgs(t *testing.T) {
	got := strings.Join(PastaArgs(4242), " ")
	want := "--quiet --config-net --netns /proc/4242/ns/net --userns /proc/4242/ns/user --tcp-ports none --udp-ports none --tcp-ns none --udp-ns none --no-map-gw"
	if got != want {
		t.Errorf("PastaArgs = %q\nwant %q", got, want)
	}
}
