package sandbox

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/deric/box/internal/config"
)

// sshFixture adds a key (behind a symlink), a known_hosts file and a stand-in
// for the host's /etc/ssh/ssh_config to the fixture; the latter is stubbed.
func sshFixture(t *testing.T) (home, cwd, bin, self, keyLink, keyReal, hostConf string) {
	t.Helper()
	home, cwd, _, bin = fixture(t)
	self = filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(filepath.Dir(home), "keys")
	keyReal = filepath.Join(keys, "deploy")
	keyLink = filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyLink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyReal, []byte("key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keyReal, keyLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte("github.com ssh-ed25519 AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostConf = filepath.Join(filepath.Dir(home), "etc", "ssh", "ssh_config")
	if err := os.MkdirAll(filepath.Dir(hostConf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostConf, []byte("Include /etc/ssh/ssh_config.d/*.conf\nHost *\n    HashKnownHosts yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, cwd, bin, self, keyLink, keyReal, hostConf
}

func dataMounts(t *testing.T, plan *Plan) (fd int, content string) {
	t.Helper()
	if len(plan.Data) != 1 {
		t.Fatalf("Data = %v, want one generated file", plan.Data)
	}
	for fd, content = range plan.Data {
	}
	got, err := io.ReadAll(os.NewFile(uintptr(fd), "data"))
	if err != nil || string(got) != content {
		t.Errorf("fd %d holds %q, %v; want %q", fd, got, err, content)
	}
	return fd, content
}

func TestBuildSSH(t *testing.T) {
	home, cwd, bin, self, keyLink, keyReal, hostConf := sshFixture(t)
	defer stub(&sshConfig, hostConf)()
	root := filepath.Join(filepath.Dir(home), "tmp", "box")
	prof := config.Profile{
		Proxy: true, AllowHosts: []string{"github.com"},
		SSH: true, SSHKey: "~/.ssh/id_ed25519", SSHUser: "git",
	}
	opts := Options{
		Binary: "tool", Cwd: cwd, Home: home, TmpRoot: root, ID: "7", Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings: %v", plan.Warnings)
	}
	if plan.Proxy == nil || !plan.Proxy.SSH {
		t.Fatalf("Proxy = %+v, want SSH set", plan.Proxy)
	}
	if !strings.Contains(plan.ProxyCommand(), " -ssh ") {
		t.Errorf("ProxyCommand() = %s, want -ssh", plan.ProxyCommand())
	}
	fd, conf := dataMounts(t, plan)
	want := "# Written by box: ssh connections go through its proxy to allow_hosts.\n" +
		"Host *\n" +
		"\tProxyCommand " + self + " _connect %h %p\n" +
		"\tIdentityFile " + keyLink + "\n" +
		"\tUser git\n" +
		"\n# The host's " + hostConf + " follows.\n" +
		"# skipped by box (root-owned files appear owned by nobody inside, which ssh refuses to include): Include /etc/ssh/ssh_config.d/*.conf\n" +
		"Host *\n    HashKnownHosts yes\n"
	if conf != want {
		t.Errorf("ssh_config = %q\nwant %q", conf, want)
	}
	known := filepath.Join(home, ".ssh", "known_hosts")
	var knownFD int
	for fd, path := range plan.Files {
		if path == known {
			knownFD = fd
		}
	}
	if knownFD == 0 {
		t.Fatalf("Files = %v, want a copy of %s", plan.Files, known)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	for _, want := range []string{
		// The key is bound by its real path at the configured one, read-only.
		" --ro-bind " + keyReal + " " + keyLink + " ",
		" --perms 0644 --file " + strconv.Itoa(knownFD) + " " + known + " ",
		" --perms 0644 --ro-bind-data " + strconv.Itoa(fd) + " " + hostConf + " ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in\n%s", want, cmd)
		}
	}

	// Without a system-wide ssh_config on the host the generated one goes to
	// ~/.ssh/config, which only its owner may read.
	defer stub(&sshConfig, filepath.Join(t.TempDir(), "missing"))()
	plan, err = buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	fd, conf = dataMounts(t, plan)
	if strings.Contains(conf, "follows") || !strings.HasSuffix(conf, "\tUser git\n") {
		t.Errorf("ssh_config = %q, want the generated part only", conf)
	}
	if want := " --perms 0600 --ro-bind-data " + strconv.Itoa(fd) + " " + filepath.Join(home, ".ssh", "config") + " "; !strings.Contains(" "+strings.Join(plan.Args, " ")+" ", want) {
		t.Errorf("missing %q in\n%s", want, strings.Join(plan.Args, " "))
	}
}

func TestBuildSSHHostNetwork(t *testing.T) {
	home, cwd, bin, self, keyLink, _, hostConf := sshFixture(t)
	defer stub(&sshConfig, hostConf)()
	prof := config.Profile{Network: true, SSH: true, SSHKey: keyLink}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home, Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	// ssh connects directly; there is no proxy to route it through and the
	// box executable is not needed inside.
	_, conf := dataMounts(t, plan)
	if strings.Contains(conf, "ProxyCommand") || !strings.Contains(conf, "\tIdentityFile "+keyLink+"\n") || strings.Contains(conf, "User") {
		t.Errorf("ssh_config = %q", conf)
	}
	if strings.Contains(strings.Join(plan.Args, " "), self) {
		t.Errorf("box must not be exposed without a forwarder: %v", plan.Args)
	}
}

func TestBuildSSHWithoutNetwork(t *testing.T) {
	home, cwd, bin, self, _, _, hostConf := sshFixture(t)
	defer stub(&sshConfig, hostConf)()
	prof := config.Profile{SSH: true, SSHUser: "git"}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home, Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Data) != 0 || len(plan.Files) != 0 {
		t.Errorf("nothing should be set up without network: Data %v Files %v", plan.Data, plan.Files)
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.Contains(w, "ssh ignored") }) {
		t.Errorf("want a warning about ssh, got %v", plan.Warnings)
	}
}

func TestBuildSSHKeyMissing(t *testing.T) {
	home, cwd, bin, self, _, _, hostConf := sshFixture(t)
	defer stub(&sshConfig, hostConf)()
	opts := Options{
		Binary: "tool", Cwd: cwd, Home: home, Self: self, TmpRoot: filepath.Join(t.TempDir(), "box"),
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	for _, tc := range []struct {
		key  string
		warn bool
	}{
		{"~/.ssh/nope", true},
		{"?~/.ssh/nope", false},
		{"~/.ssh", true}, // not a regular file
	} {
		plan, err := buildBwrap(config.Profile{Proxy: true, SSH: true, SSHKey: tc.key}, opts, "bwrap")
		if err != nil {
			t.Fatal(err)
		}
		warned := slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.Contains(w, "ssh_key") })
		if warned != tc.warn {
			t.Errorf("key %q: warnings = %v, want warning %v", tc.key, plan.Warnings, tc.warn)
		}
		if _, conf := dataMounts(t, plan); strings.Contains(conf, "IdentityFile") {
			t.Errorf("key %q: config names an identity: %q", tc.key, conf)
		}
		for fd := range plan.Data {
			_ = os.NewFile(uintptr(fd), "").Close()
		}
	}
}

func TestBuildSSHSeatbelt(t *testing.T) {
	home, cwd, bin, _, keyLink, _, _ := sshFixture(t)
	plan, err := buildSeatbelt(config.Profile{Network: true, SSH: true, SSHKey: keyLink, RWBinds: []string{"$PWD"}}, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Data) != 0 || len(plan.Files) != 0 || strings.Contains(strings.Join(plan.Args, " "), keyLink) {
		t.Errorf("ssh must be ignored on macOS: Data %v Files %v Args %v", plan.Data, plan.Files, plan.Args)
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.Contains(w, "ssh is not supported") }) {
		t.Errorf("want a warning about ssh, got %v", plan.Warnings)
	}
}

func TestWithoutIncludes(t *testing.T) {
	in := "Include /etc/ssh/ssh_config.d/*.conf\n  include=~/.ssh/extra\nIncludeX no\nHost *\n\tHashKnownHosts yes"
	want := "# skipped by box (root-owned files appear owned by nobody inside, which ssh refuses to include): Include /etc/ssh/ssh_config.d/*.conf\n" +
		"# skipped by box (root-owned files appear owned by nobody inside, which ssh refuses to include): include=~/.ssh/extra\n" +
		"IncludeX no\nHost *\n\tHashKnownHosts yes\n"
	if got := withoutIncludes(in); got != want {
		t.Errorf("withoutIncludes = %q\nwant %q", got, want)
	}
	if got := withoutIncludes(""); got != "\n" {
		t.Errorf("empty = %q", got)
	}
}

func TestDataMountTooLarge(t *testing.T) {
	plan := &Plan{}
	if _, err := dataMount(plan, "/x", strings.Repeat("x", pipeCapacity+1), 0o644); err == nil {
		t.Error("want an error for content larger than a pipe holds")
	}
	if len(plan.Data) != 0 {
		t.Errorf("Data = %v", plan.Data)
	}
}
