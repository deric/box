package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/deric/box/internal/config"
)

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		got, want string
		ok        bool
	}{
		{"0.11.1", "0.10", true},
		{"0.10", "0.10", true},
		{"0.10.0", "0.10", true},
		{"0.9.0", "0.10", false},
		{"0.8", "0.8", true},
		{"0.7.9", "0.8", false},
		{"2.41.3", "2.37", true},
		{"2.36.2", "2.37", false},
		{"4.9.3", "4.4", true},
		{"4.3.1", "4.4", false},
		{"6.8.0-45-generic", "5.11", true},
		{"5.4.0-100-generic", "5.11", false},
		{"5.11.0-rc1", "5.11", true},
		{"", "0.10", false},
		{"garbage", "0.10", false},
	}
	for _, tt := range tests {
		if got := versionAtLeast(tt.got, tt.want); got != tt.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tt.got, tt.want, got, tt.ok)
		}
	}
}

func TestPackageManagerFor(t *testing.T) {
	tests := []struct {
		osRelease, want string
	}{
		{"ID=ubuntu\nID_LIKE=debian\n", "apt"},
		{"ID=debian\n", "apt"},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", "apt"},
		{"ID=fedora\n", "dnf"},
		{"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n", "dnf"},
		{"ID=arch\n", "pacman"},
		{"ID=manjaro\nID_LIKE=arch\n", "pacman"},
		{"ID=alpine\n", "apk"},
		{"ID=opensuse-tumbleweed\nID_LIKE=\"opensuse suse\"\n", "zypper"},
		{"ID=nixos\n", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := packageManagerFor(strings.NewReader(tt.osRelease)); got != tt.want {
			t.Errorf("packageManagerFor(%q) = %q, want %q", tt.osRelease, got, tt.want)
		}
	}
}

func TestInstallHintPackages(t *testing.T) {
	for tool, pkgs := range packages {
		for mgr, pkg := range pkgs {
			switch mgr {
			case "apt", "dnf", "pacman", "apk", "zypper", "brew":
			default:
				t.Errorf("%s: unknown package manager %q", tool, mgr)
			}
			if pkg == "" && mgr != "brew" {
				t.Errorf("%s: empty package for %s", tool, mgr)
			}
		}
	}
	if hint := installHint("no-such-tool"); hint != "" {
		t.Errorf("installHint of an unknown tool = %q", hint)
	}
}

func TestFeatureUsage(t *testing.T) {
	cfg, err := config.Parse(`
[default]
network = false
dns = true
proxy = true
cpus = 2
memory = "1G"
overlays = [{ path = "~/.local/bin", persist = true }]

[binaries.claude]
ssh = true
ssh_key = "~/.ssh/id_work"

[binaries.podman]
podman = true
`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles = []config.DirProfile{{Name: "work", Binaries: map[string]config.Section{"git": {}}}}
	u := featureUsage(cfg)
	if !u.overlays || !u.dns || !u.cpu || !u.memory || !u.podman || !u.ssh {
		t.Errorf("features = %+v, want every feature on", u)
	}
	if got := strings.Join(u.binaries, ","); got != "claude,git,podman" {
		t.Errorf("binaries = %q, want claude,git,podman", got)
	}
	if got := u.sshKeys["~/.ssh/id_work"]; len(got) != 2 || got[0] != "claude" || got[1] != "work/claude" {
		t.Errorf("sshKeys = %v, want ~/.ssh/id_work from claude and work/claude", u.sshKeys)
	}

	off, err := config.Parse(`
[default]
dns = false
cpus = 0
memory = ""
drop_binds = ["~/.local/share/mise", "~/.local/bin"]
`)
	if err != nil {
		t.Fatal(err)
	}
	u = featureUsage(off)
	if u.dns || u.cpu || u.memory || u.ssh || len(u.sshKeys) != 0 {
		t.Errorf("features = %+v, want dns, limits and ssh off", u)
	}
}

func TestCheckWritable(t *testing.T) {
	root := t.TempDir()
	if ch := checkWritable("x", root); ch.status != statusOK || ch.detail != root {
		t.Errorf("existing dir: %+v", ch)
	}
	missing := filepath.Join(root, "a", "b")
	if ch := checkWritable("x", missing); ch.status != statusOK || !strings.HasSuffix(ch.detail, "(will be created)") {
		t.Errorf("missing dir: %+v", ch)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ch := checkWritable("x", file); ch.status != statusFail {
		t.Errorf("file in place of dir: %+v", ch)
	}
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	ro := filepath.Join(root, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	if ch := checkWritable("x", filepath.Join(ro, "sub")); ch.status != statusFail || ch.fix == "" {
		t.Errorf("read-only parent: %+v", ch)
	}
}

func TestCheckBinaries(t *testing.T) {
	bin := t.TempDir()
	exe := filepath.Join(bin, "present")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	u := features{binaries: []string{"absent", "podman", "present"}, podman: true}
	ch := checkBinaries(u)
	if ch.status != statusWarn || !strings.Contains(ch.detail, "absent") || !strings.Contains(ch.detail, "present") {
		t.Errorf("missing binary: %+v", ch)
	}
	if strings.Contains(ch.detail, "podman") {
		t.Errorf("podman listed although it has a check of its own: %+v", ch)
	}
	u = features{binaries: []string{"present"}}
	if ch := checkBinaries(u); ch.status != statusOK {
		t.Errorf("all present: %+v", ch)
	}
}

func TestCheckSSHKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Skip("HOME is not the home directory on Windows")
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ok"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := features{ssh: true, sshKeys: map[string][]string{
		"~/.ssh/id_ok":        {"claude"},
		"$HOME/.ssh/id_gone":  {"git"},
		"?~/.ssh/id_optional": {"work/git"},
	}}
	got := map[string]status{}
	for _, ch := range checkSSH(u)[1:] { // the first is the ssh client
		got[ch.detail] = ch.status
	}
	want := map[string]status{
		filepath.Join(home, ".ssh", "id_ok") + " (claude)":                                statusOK,
		filepath.Join(home, ".ssh", "id_optional") + " (optional, not present; work/git)": statusOK,
	}
	for detail, st := range want {
		if got[detail] != st {
			t.Errorf("missing %q = %v in %v", detail, st, got)
		}
	}
	found := false
	for detail, st := range got {
		if strings.HasPrefix(detail, filepath.Join(home, ".ssh", "id_gone")) && st == statusWarn {
			found = true
		}
	}
	if !found {
		t.Errorf("missing key not warned about: %v", got)
	}
}

func TestPrintChecks(t *testing.T) {
	checks := []check{
		{"bwrap", statusOK, "bubblewrap 0.11.1 at /usr/bin/bwrap", "ignored for ok"},
		{"limits", statusWarn, "no cpu controller", "delegate it:\n  Delegate=cpu"},
		{"config", statusFail, "broken", "fix it"},
	}
	var plain bytes.Buffer
	if err := printChecks(&plain, checks, false); err != nil {
		t.Fatal(err)
	}
	want := "\u2714 ok    bwrap   bubblewrap 0.11.1 at /usr/bin/bwrap\n" +
		"\u26a0 warn  limits  no cpu controller\n" +
		"                delegate it:\n" +
		"                  Delegate=cpu\n" +
		"\u2718 FAIL  config  broken\n" +
		"                fix it\n"
	if plain.String() != want {
		t.Errorf("printChecks:\n%s\nwant:\n%s", plain.String(), want)
	}

	// Colored output aligns the same way: the escape sequences take no
	// width, and the tabwriter's escape bytes do not leak.
	var colored bytes.Buffer
	if err := printChecks(&colored, checks, true); err != nil {
		t.Fatal(err)
	}
	got := colored.String()
	if strings.Contains(got, string(tabwriter.Escape)) {
		t.Errorf("tabwriter escape leaked into output: %q", got)
	}
	if !strings.Contains(got, "\x1b[32m\u2714 ok\x1b[0m") || !strings.Contains(got, "\x1b[33m\u26a0 warn\x1b[0m") || !strings.Contains(got, "\x1b[31;1m\u2718 FAIL\x1b[0m") {
		t.Errorf("statuses not colored: %q", got)
	}
	if !strings.Contains(got, "\x1b[1mlimits\x1b[0m") || strings.Contains(got, "\x1b[1mbwrap") {
		t.Errorf("only names of warnings and failures are bold: %q", got)
	}
	if !strings.Contains(got, "\x1b[2mfix it\x1b[0m") {
		t.Errorf("fix lines not dimmed: %q", got)
	}
	strip := regexp.MustCompile("\x1b\\[[0-9;]*m")
	if stripped := strip.ReplaceAllString(got, ""); stripped != want {
		t.Errorf("colored output, stripped:\n%s\nwant:\n%s", stripped, want)
	}
}

func TestUseColor(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	t.Setenv("TERM", "xterm")
	os.Unsetenv("NO_COLOR")
	if useColor(int(devnull.Fd())) {
		t.Error("colors on for a non-terminal")
	}
	t.Setenv("NO_COLOR", "")
	if useColor(int(devnull.Fd())) {
		t.Error("colors on with NO_COLOR set")
	}
}
