package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/deric/box/internal/config"
)

// stub replaces *p with v until the returned function is called.
func stub[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func TestBuildDNS(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	self := filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(home), "tmp", "box")
	// The host's resolv.conf is a symlink, as with systemd-resolved; the
	// sandbox's copy must cover the file it points to.
	etc := filepath.Join(filepath.Dir(home), "etc")
	resolve := filepath.Join(filepath.Dir(home), "run", "resolve")
	for _, d := range []string{etc, resolve} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(resolve, "stub-resolv.conf")
	if err := os.WriteFile(target, []byte("# generated\nnameserver 127.0.0.53\nsearch home\noptions edns0 trust-ad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(etc, "resolv.conf")
	if err := os.Symlink("../run/resolve/stub-resolv.conf", link); err != nil {
		t.Fatal(err)
	}
	// The mount lands on the resolved path (on macOS the temporary
	// directory itself sits behind a symlink).
	target = realPath(target)
	defer stub(&resolvConf, link)()
	defer stub(&lookupUnshare, func() (string, error) { return "/usr/bin/unshare", nil })()
	var probed []string
	defer stub(&probeNetns, func(unshare, self string) error {
		probed = append(probed, unshare+" "+self)
		return nil
	})()

	prof := config.Profile{Proxy: true, DNS: true, AllowHosts: []string{"github.com"}}
	opts := Options{
		Binary: "tool", Cwd: cwd, Home: home, TmpRoot: root, ID: "7", Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DNS == nil || plan.DNS.Unshare != "/usr/bin/unshare" || plan.DNS.UDPFD < 3 || plan.DNS.TCPFD < 3 || plan.DNS.UDPFD == plan.DNS.TCPFD {
		t.Fatalf("DNS = %+v", plan.DNS)
	}
	if len(probed) != 1 || probed[0] != "/usr/bin/unshare "+self {
		t.Errorf("probed = %v", probed)
	}
	if len(plan.Data) != 1 {
		t.Fatalf("Data = %v", plan.Data)
	}
	var dataFD int
	for fd := range plan.Data {
		dataFD = fd
	}
	wantConf := "# Written by box: queries go through its proxy to the host's resolver.\nnameserver 127.0.0.1\nsearch home\noptions edns0 trust-ad\n"
	if plan.Data[dataFD] != wantConf {
		t.Errorf("resolv.conf = %q, want %q", plan.Data[dataFD], wantConf)
	}
	// The runner reads the content from the descriptor, up to EOF.
	if got := readFD(t, dataFD); got != wantConf {
		t.Errorf("fd %d holds %q", dataFD, got)
	}
	udp, tcp := strconv.Itoa(plan.DNS.UDPFD), strconv.Itoa(plan.DNS.TCPFD)
	cmd := " " + strings.Join(plan.Args, " ") + " "
	for _, want := range []string{
		" --unshare-all --share-net ",
		" --perms 0444 --ro-bind-data " + strconv.Itoa(dataFD) + " " + target + " ",
		" _forward -s " + ProxySocket + " -l " + ProxyAddr + " -u " + udp + " -t " + tcp + " -- tool ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in\n%s", want, cmd)
		}
	}
	if pc := plan.ProxyCommand(); !strings.Contains(pc, " _proxy -s ") || !strings.Contains(pc, " -d -a github.com") {
		t.Errorf("ProxyCommand() = %s", pc)
	}
	prefix := fmt.Sprintf("/usr/bin/unshare -U -n --map-user=%d --map-group=%d --keep-caps -- %s _netns -u %s -t %s -n project -- bwrap --unshare-all --share-net ",
		os.Getuid(), os.Getgid(), self, udp, tcp)
	line := plan.Command()
	if !strings.HasPrefix(line, prefix) {
		t.Errorf("Command() = %s\nwant prefix %s", line, prefix)
	}
	if !strings.HasSuffix(line, " "+strconv.Itoa(dataFD)+"<<<"+shellQuote(strings.TrimSuffix(wantConf, "\n"))) {
		t.Errorf("Command() = %s\nwant the resolv.conf as a here-string at the end", line)
	}
	closeDNS(plan)

	// The namespaces cannot be prepared: a warning, and the sandbox runs
	// without DNS in a network namespace of bwrap's own.
	defer stub(&probeNetns, func(string, string) error { return fmt.Errorf("unshare failed: no space left on device") })()
	plan, err = buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	cmd = " " + strings.Join(plan.Args, " ") + " "
	if plan.DNS != nil || len(plan.Data) != 0 || strings.Contains(cmd, "--share-net") || strings.Contains(cmd, " -u ") ||
		strings.Contains(plan.ProxyCommand(), " -d ") || strings.Contains(plan.Command(), "_netns") {
		t.Errorf("DNS not disabled after a failed probe: %+v\n%s", plan.DNS, plan.Command())
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "dns disabled") || !strings.Contains(plan.Warnings[0], "no space left") {
		t.Errorf("Warnings = %v", plan.Warnings)
	}
	defer stub(&lookupUnshare, func() (string, error) { return "", exec.ErrNotFound })()
	plan, err = buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DNS != nil || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "unshare not found") {
		t.Errorf("without unshare: DNS = %+v, Warnings = %v", plan.DNS, plan.Warnings)
	}
}

func TestBuildDNSOff(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	self := filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer stub(&lookupUnshare, func() (string, error) { return "/usr/bin/unshare", nil })()
	defer stub(&probeNetns, func(string, string) error { t.Error("probed without DNS"); return nil })()
	opts := Options{
		Binary: "tool", Cwd: cwd, Home: home, TmpRoot: filepath.Join(filepath.Dir(home), "tmp", "box"), ID: "7", Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	for name, prof := range map[string]config.Profile{
		"dns = false":    {Proxy: true, DNS: false},
		"proxy = false":  {Proxy: false, DNS: true},
		"network = true": {Network: true, Proxy: true, DNS: true},
	} {
		plan, err := buildBwrap(prof, opts, "bwrap")
		if err != nil {
			t.Fatal(err)
		}
		cmd := " " + strings.Join(plan.Args, " ") + " "
		if plan.DNS != nil || len(plan.Data) != 0 || strings.Contains(cmd, "_netns") || strings.Contains(cmd, "ro-bind-data") ||
			strings.Contains(plan.ProxyCommand(), " -d ") || strings.Contains(plan.Command(), "_netns") || len(plan.Warnings) != 0 {
			t.Errorf("%s: unexpected DNS setup: %+v %v\n%s", name, plan.DNS, plan.Warnings, plan.Command())
		}
		if shared := strings.Contains(cmd, " --share-net "); shared != prof.Network {
			t.Errorf("%s: --share-net = %v", name, shared)
		}
	}

	// A host without resolv.conf: DNS, but nothing to cover.
	defer stub(&resolvConf, filepath.Join(t.TempDir(), "missing"))()
	defer stub(&probeNetns, func(string, string) error { return nil })()
	plan, err := buildBwrap(config.Profile{Proxy: true, DNS: true}, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DNS == nil || len(plan.Data) != 0 || len(plan.Warnings) != 0 || strings.Contains(strings.Join(plan.Args, " "), "ro-bind-data") {
		t.Errorf("without a host resolv.conf: DNS = %+v, Data = %v, Warnings = %v", plan.DNS, plan.Data, plan.Warnings)
	}
	closeDNS(plan)
}

// closeDNS releases the descriptors a plan with DNS holds open.
func closeDNS(p *Plan) {
	if p.DNS != nil && p.DNS.UDPFD >= 0 {
		_ = os.NewFile(uintptr(p.DNS.UDPFD), "").Close()
		_ = os.NewFile(uintptr(p.DNS.TCPFD), "").Close()
	}
	for fd := range p.Data {
		_ = os.NewFile(uintptr(fd), "").Close()
	}
}
