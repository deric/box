package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deric/box/internal/config"
)

// pastaFixture prepares a host with unshare, pasta and nft and a
// resolv.conf, returning the options for a sandbox of "tool".
func pastaFixture(t *testing.T) (opts Options, resolv string) {
	t.Helper()
	home, cwd, _, bin := fixture(t)
	self := filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolv = filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 10.0.0.1\nsearch lan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stub(&resolvConf, resolv))
	t.Cleanup(stub(&lookupUnshare, func() (string, error) { return "/usr/bin/unshare", nil }))
	t.Cleanup(stub(&lookupPasta, func() (string, error) { return "/usr/bin/pasta", nil }))
	t.Cleanup(stub(&lookupNFT, func() (string, error) { return "/usr/sbin/nft", nil }))
	t.Cleanup(stub(&probeNetns, func(string, string) error { return nil }))
	t.Cleanup(stub(&sshConfig, filepath.Join(t.TempDir(), "missing-ssh_config")))
	return Options{
		Binary: "tool", Cwd: cwd, Home: home, TmpRoot: filepath.Join(filepath.Dir(home), "tmp", "box"), ID: "7", Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, resolv
}

func TestBuildPasta(t *testing.T) {
	opts, resolv := pastaFixture(t)
	prof := config.Profile{Proxy: true, DNS: true, Pasta: true, AllowHosts: []string{"github.com", "*.golang.org"}}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	defer closeDNS(plan)
	if plan.Pasta == nil || plan.Pasta.Exe != "/usr/bin/pasta" || plan.Pasta.NFT != "/usr/sbin/nft" {
		t.Fatalf("Pasta = %+v", plan.Pasta)
	}
	if plan.DNS == nil || plan.DNS.Unshare != "/usr/bin/unshare" || plan.DNS.UDPFD >= 0 || plan.DNS.TCPFD >= 0 {
		t.Fatalf("DNS = %+v", plan.DNS)
	}
	if plan.Proxy == nil || len(plan.Proxy.Allow) != 2 {
		t.Fatalf("Proxy = %+v", plan.Proxy)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("Warnings = %v", plan.Warnings)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	// The namespace is the one box prepared; nothing of the proxy reaches
	// inside: no socket, no proxy variables, no forwarder.
	if !strings.Contains(cmd, " --unshare-all --share-net ") {
		t.Errorf("missing --share-net in\n%s", cmd)
	}
	for _, unwanted := range []string{ProxySocket, "HTTP_PROXY", "NO_PROXY", "_forward", " -u ", " -t "} {
		if strings.Contains(cmd, unwanted) {
			t.Errorf("unexpected %q in\n%s", unwanted, cmd)
		}
	}
	if !strings.HasSuffix(cmd, " -- tool ") {
		t.Errorf("the command is not run directly:\n%s", cmd)
	}
	// The resolver's resolv.conf is bound over the host's (next to the
	// ssh_config pasta sandboxes always get, see TestBuildPastaSSHOff).
	if len(plan.Data) != 2 {
		t.Fatalf("Data = %v", plan.Data)
	}
	want := "# Written by box: its resolver answers for allow_hosts and admits their addresses.\nnameserver 127.0.0.1\nsearch lan\n"
	found := false
	for fd, content := range plan.Data {
		if !strings.Contains(content, "nameserver") {
			continue
		}
		found = true
		if content != want {
			t.Errorf("resolv.conf = %q, want %q", content, want)
		}
		if !strings.Contains(cmd, fmt.Sprintf(" --ro-bind-data %d %s ", fd, realPath(resolv))) {
			t.Errorf("resolv.conf not bound in\n%s", cmd)
		}
	}
	if !found {
		t.Errorf("no resolv.conf in Data = %v", plan.Data)
	}
	// The proxy only serves the resolver and starts pasta; it lets no
	// request of its own through (no -a), and ssh is not routed through it.
	pc := plan.ProxyCommand()
	if !strings.HasSuffix(pc, " _proxy -s "+shellQuote(plan.Proxy.Socket)+" -l "+shellQuote(plan.Proxy.Log)+" -d -pasta /usr/bin/pasta") {
		t.Errorf("ProxyCommand() = %s", pc)
	}
	// unshare runs `box _netns -pasta` with what the resolver needs.
	prefix := fmt.Sprintf("/usr/bin/unshare -U -n --map-user=%d --map-group=%d --keep-caps -- %s _netns -n tool -pasta -s %s -l %s -f /usr/sbin/nft -a github.com -a '*.golang.org' -- bwrap --unshare-all --share-net ",
		os.Getuid(), os.Getgid(), opts.Self, shellQuote(plan.Proxy.Socket), shellQuote(plan.Proxy.Log))
	if line := plan.Command(); !strings.HasPrefix(line, prefix) {
		t.Errorf("Command() = %s\nwant prefix %s", line, prefix)
	}
}

func TestBuildPastaSSH(t *testing.T) {
	opts, _ := pastaFixture(t)
	hostConf := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(hostConf, []byte("Host *\n\tAddKeysToAgent yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer stub(&sshConfig, hostConf)()
	prof := config.Profile{Proxy: true, DNS: true, Pasta: true, SSH: true, SSHUser: "git", AllowHosts: []string{"github.com"}}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	defer closeDNS(plan)
	// ssh connects directly through pasta: no ProxyCommand, and the proxy
	// is not told to let CONNECT through.
	var conf string
	for _, content := range plan.Data {
		if strings.Contains(content, "Host *") {
			conf = content
		}
	}
	if conf == "" || strings.Contains(conf, "ProxyCommand") || !strings.Contains(conf, "\tUser git\n") || !strings.Contains(conf, "pasta") {
		t.Errorf("ssh_config = %q", conf)
	}
	if plan.Proxy.SSH || strings.Contains(plan.ProxyCommand(), "-ssh") {
		t.Errorf("proxy told about ssh: %s", plan.ProxyCommand())
	}
}

func TestBuildPastaSSHOff(t *testing.T) {
	opts, _ := pastaFixture(t)
	hostConf := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(hostConf, []byte("Include /etc/ssh/ssh_config.d/*.conf\nHost *\n\tAddKeysToAgent yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer stub(&sshConfig, hostConf)()
	// ssh = false: the host's ssh_config still gets its Include lines
	// commented out, as ssh reaches allow_hosts through pasta anyway,
	// but no key or user is set.
	prof := config.Profile{Proxy: true, DNS: true, Pasta: true, SSHKey: "~/.ssh/id_ed25519", SSHUser: "git", AllowHosts: []string{"github.com"}}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	defer closeDNS(plan)
	var conf string
	for _, content := range plan.Data {
		if strings.Contains(content, "Host *") {
			conf = content
		}
	}
	if conf == "" || !strings.Contains(conf, "# skipped by box") || strings.Contains(conf, "\nInclude") ||
		strings.Contains(conf, "User") || strings.Contains(conf, "IdentityFile") || strings.Contains(conf, "ProxyCommand") {
		t.Errorf("ssh_config = %q", conf)
	}
	if !strings.Contains(strings.Join(plan.Args, " "), " "+realPath(hostConf)+" ") {
		t.Errorf("ssh_config not bound over %s:\n%s", hostConf, strings.Join(plan.Args, " "))
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("Warnings = %v", plan.Warnings)
	}
}

func TestBuildPastaFallsBack(t *testing.T) {
	opts, _ := pastaFixture(t)
	prof := config.Profile{Proxy: true, DNS: true, Pasta: true, AllowHosts: []string{"github.com"}}
	for name, tc := range map[string]struct {
		setup func() func()
		warn  string
	}{
		"no pasta":   {func() func() { return stub(&lookupPasta, func() (string, error) { return "", exec.ErrNotFound }) }, "pasta not found"},
		"no nft":     {func() func() { return stub(&lookupNFT, func() (string, error) { return "", exec.ErrNotFound }) }, "nft not found"},
		"no unshare": {func() func() { return stub(&lookupUnshare, func() (string, error) { return "", exec.ErrNotFound }) }, "unshare not found"},
		"no netns": {func() func() {
			return stub(&probeNetns, func(string, string) error { return fmt.Errorf("unshare: Operation not permitted") })
		}, "cannot prepare its network namespace"},
	} {
		restore := tc.setup()
		plan, err := buildBwrap(prof, opts, "bwrap")
		restore()
		if err != nil {
			t.Fatal(err)
		}
		cmd := " " + strings.Join(plan.Args, " ") + " "
		if plan.Pasta != nil || strings.Contains(plan.ProxyCommand(), "-pasta") || strings.Contains(plan.Command(), "-pasta") {
			t.Errorf("%s: pasta still planned: %s", name, plan.Command())
		}
		// The proxy takes over: its socket and variables are inside.
		if !strings.Contains(cmd, ProxySocket) || !strings.Contains(cmd, "HTTPS_PROXY") || !strings.Contains(cmd, " _forward -s ") {
			t.Errorf("%s: proxy not set up:\n%s", name, cmd)
		}
		if len(plan.Warnings) == 0 || !strings.HasPrefix(plan.Warnings[0], "pasta disabled, using the proxy: ") || !strings.Contains(plan.Warnings[0], tc.warn) {
			t.Errorf("%s: Warnings = %v", name, plan.Warnings)
		}
		// Without unshare or the namespaces, DNS through the proxy is out
		// of reach as well and says so separately.
		if name == "no unshare" || name == "no netns" {
			if len(plan.Warnings) != 2 || !strings.HasPrefix(plan.Warnings[1], "dns disabled: ") {
				t.Errorf("%s: Warnings = %v", name, plan.Warnings)
			}
		} else if plan.DNS == nil || len(plan.Warnings) != 1 {
			t.Errorf("%s: DNS = %+v, Warnings = %v", name, plan.DNS, plan.Warnings)
		}
		closeDNS(plan)
	}

	// dns = false is overridden: the resolver is the mechanism.
	plan, err := buildBwrap(config.Profile{Proxy: true, DNS: false, Pasta: true}, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	defer closeDNS(plan)
	if plan.Pasta == nil || plan.DNS == nil || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "dns = false ignored") {
		t.Errorf("dns = false: Pasta = %+v, Warnings = %v", plan.Pasta, plan.Warnings)
	}

	// Without the proxy, or with the host network, pasta has nothing to do.
	for name, prof := range map[string]config.Profile{
		"proxy = false":  {Proxy: false, Pasta: true, DNS: true},
		"network = true": {Network: true, Proxy: true, Pasta: true, DNS: true},
	} {
		plan, err := buildBwrap(prof, opts, "bwrap")
		if err != nil {
			t.Fatal(err)
		}
		if plan.Pasta != nil || plan.DNS != nil || plan.Proxy != nil || len(plan.Warnings) != 0 {
			t.Errorf("%s: Pasta = %+v, DNS = %+v, Proxy = %+v, Warnings = %v", name, plan.Pasta, plan.DNS, plan.Proxy, plan.Warnings)
		}
	}
}
