package config

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultTOMLParses(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatalf("default config does not parse: %v", err)
	}
	p := c.Resolve("anything")
	if p.Network || !p.Proxy || p.Hostname != "box" || !p.BindBinary || !p.DieWithParent ||
		!p.NewSession || !p.DisableUserns || !p.ClearEnv {
		t.Errorf("unexpected default scalars: %+v", p)
	}
	if !contains(p.AllowHosts, "github.com") || !contains(p.AllowHosts, "*.github.com") {
		t.Errorf("allow_hosts = %v", p.AllowHosts)
	}
	for _, want := range []string{"HOME", "PATH", "TERM", "LC_*"} {
		if !contains(p.PassEnv, want) {
			t.Errorf("pass_env = %v, missing %q", p.PassEnv, want)
		}
	}
	if len(p.Overlays) != 2 || p.Overlays[0].Path != "~/.local/share/mise" || !p.Overlays[0].Persist ||
		p.Overlays[1].Path != "?~/.local/bin" || !p.Overlays[1].Persist {
		t.Errorf("unexpected default overlays: %+v", p.Overlays)
	}
	if p.LogDir != DefaultLogDir {
		t.Errorf("LogDir = %q, want %q", p.LogDir, DefaultLogDir)
	}
	if p.Env["BOX_SANDBOX"] != "1" {
		t.Errorf("BOX_SANDBOX not set: %v", p.Env)
	}
}

func TestResolveMergesSections(t *testing.T) {
	c, err := Parse(`
[default]
ro_binds = ["/usr", "/etc"]
rw_binds = ["$PWD", "/tmp"]
overlays = ["~/.local/share/mise", { path = "/opt/tools", persist = false }]
network = true
env = { A = "1", B = "2" }

[binaries.agent]
ro_binds = ["/etc", "/opt"]
drop_binds = ["/tmp", "/opt/tools"]
tmpfs = ["/tmp"]
network = false
env = { B = "3", C = "4" }
unset_env = ["A"]
`)
	if err != nil {
		t.Fatal(err)
	}

	def := c.Resolve("other")
	if !def.Network || len(def.Overlays) != 2 || def.Overlays[1].Persist {
		t.Errorf("default profile wrong: %+v", def)
	}

	p := c.Resolve("agent")
	if got, want := p.ROBinds, []string{"/usr", "/etc", "/opt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ro_binds = %v, want %v", got, want)
	}
	if got, want := p.RWBinds, []string{"$PWD"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rw_binds = %v, want %v", got, want)
	}
	if got, want := p.Tmpfs, []string{"/tmp"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tmpfs = %v, want %v", got, want)
	}
	if len(p.Overlays) != 1 || p.Overlays[0].Path != "~/.local/share/mise" {
		t.Errorf("overlays = %+v", p.Overlays)
	}
	if p.Network {
		t.Error("network should be overridden to false")
	}
	if got, want := p.Env, map[string]string{"B": "3", "C": "4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestResolvePassEnv(t *testing.T) {
	c, err := Parse(`
[default]
clear_env = true
pass_env = ["HOME", "PATH", "TERM"]

[binaries.agent]
pass_env = ["API_*"]
unset_env = ["TERM"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Resolve("agent").PassEnv, []string{"HOME", "PATH", "API_*"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pass_env = %v, want %v", got, want)
	}
}

func TestResolveInheritFalse(t *testing.T) {
	c, err := Parse(`
[default]
ro_binds = ["/usr"]
network = false

[binaries.lonely]
inherit = false
ro_binds = ["/etc"]
`)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Resolve("lonely")
	if got, want := p.ROBinds, []string{"/etc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ro_binds = %v, want %v", got, want)
	}
	if !p.Network {
		t.Error("network should fall back to the built-in default (true)")
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	if _, err := Parse("[default]\nnetwrok = true\n"); err == nil {
		t.Error("expected error for misspelled key")
	}
	if _, err := Parse("[default]\noverlays = [{ path = \"/x\", persits = true }]\n"); err == nil {
		t.Error("expected error for unknown overlay key")
	}
}

func TestDefaultTOMLClaude(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Resolve("claude")
	for _, want := range []string{"$PWD", "?~/.claude/projects", "?~/.claude/.credentials.json"} {
		if !contains(p.RWBinds, want) {
			t.Errorf("rw_binds = %v, missing %q", p.RWBinds, want)
		}
	}
	if contains(p.RWBinds, "?~/.claude") || contains(p.RWBinds, "?~/.claude.json") {
		t.Errorf("~/.claude must be an overlay and ~/.claude.json a synced copy, not rw_binds %v", p.RWBinds)
	}
	if len(p.Overlays) != 6 || p.Overlays[2].Path != "?~/.claude" || p.Overlays[2].Persist {
		t.Errorf("want a temporary ~/.claude overlay after the default ones, got %+v", p.Overlays)
	}
	for i, want := range []string{"?~/.claude/skills", "?~/.claude/plugins", "?~/.agents/skills"} {
		if len(p.Overlays) <= 3+i {
			break
		}
		if o := p.Overlays[3+i]; o.Path != want || !o.Persist {
			t.Errorf("overlays[%d] = %+v, want persistent %s", 3+i, o, want)
		}
	}
	if contains(p.RWBinds, "?~/.claude/skills") || contains(p.ROBinds, "?~/.claude/skills") {
		t.Errorf("~/.claude/skills must be a persistent overlay, not a bind: ro %v rw %v", p.ROBinds, p.RWBinds)
	}
	if got, want := p.SyncFiles, []string{"?~/.claude.json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sync_files = %v, want %v", got, want)
	}
	if len(p.CopyFiles) != 0 {
		t.Errorf("copy_files = %v, want none (~/.claude.json is synced, not copied)", p.CopyFiles)
	}
	if !contains(p.PassEnv, "HOME") || !contains(p.PassEnv, "ANTHROPIC_*") {
		t.Errorf("pass_env = %v, want defaults plus ANTHROPIC_*", p.PassEnv)
	}
	if !contains(p.AllowHosts, "*.anthropic.com") || !contains(p.AllowHosts, "github.com") {
		t.Errorf("allow_hosts = %v, want defaults plus *.anthropic.com", p.AllowHosts)
	}
	if contains(p.RWBinds, "/tmp") || contains(p.Tmpfs, "/tmp") {
		t.Errorf("/tmp must come from private_tmp, not rw_binds %v or tmpfs %v", p.RWBinds, p.Tmpfs)
	}
	if !p.PrivateTmp {
		t.Error("private_tmp should be enabled by default")
	}
}

func TestDefaultTOMLDarwin(t *testing.T) {
	c, err := Parse(DefaultDarwinTOML)
	if err != nil {
		t.Fatalf("darwin default config does not parse: %v", err)
	}
	p := c.Resolve("claude")
	if !p.Network || p.Proxy || len(p.Overlays) != 0 || p.Env["BOX_SANDBOX"] != "1" {
		t.Errorf("unexpected darwin defaults: %+v", p)
	}
	for _, want := range []string{"/System", "/usr"} {
		if !contains(p.ROBinds, want) {
			t.Errorf("ro_binds = %v, missing %q", p.ROBinds, want)
		}
	}
	for _, want := range []string{"$PWD", "?$TMPDIR", "?~/.claude"} {
		if !contains(p.RWBinds, want) {
			t.Errorf("rw_binds = %v, missing %q", p.RWBinds, want)
		}
	}
	if !contains(p.ROBinds, "?~/.agents/skills") {
		t.Errorf("ro_binds = %v, missing ~/.agents/skills", p.ROBinds)
	}
	if defaultFor("darwin") != DefaultDarwinTOML || defaultFor("linux") != DefaultLinuxTOML {
		t.Error("defaultFor picks the wrong file")
	}
}

func TestResolveSeatbeltRules(t *testing.T) {
	c, err := Parse(`
[default]
seatbelt_rules = ["(allow a)"]
[binaries.x]
seatbelt_rules = ["(allow b)"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Resolve("x").SeatbeltRules, []string{"(allow a)", "(allow b)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("seatbelt_rules = %v, want %v", got, want)
	}
}

func TestWriteDefaultRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.toml")
	if err := WriteDefault(path, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[default]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteDefault(path, false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	if err := WriteDefault(path, true); err != nil {
		t.Fatalf("force overwrite failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != DefaultTOML {
		t.Fatal("force overwrite did not restore the defaults")
	}
}

func TestDiffDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.toml")
	if err := os.WriteFile(path, []byte(DefaultTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	differs, err := DiffDefault(path, &out)
	if err != nil {
		t.Fatal(err)
	}
	if differs || out.Len() != 0 {
		t.Fatalf("expected no diff for identical file, got:\n%s", out.String())
	}

	modified := DefaultTOML + "\n[custom]\nnetwork = false\n"
	if err := os.WriteFile(path, []byte(modified), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	differs, err = DiffDefault(path, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !differs {
		t.Fatal("expected modified file to differ")
	}
	for _, want := range []string{"--- " + path, "+++ box defaults", "-[custom]", "-network = false"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("diff missing %q:\n%s", want, out.String())
		}
	}
}

func TestLookupDiffToolPrefersColordiff(t *testing.T) {
	diff, err := exec.LookPath("diff")
	if err != nil {
		t.Skip("diff not installed")
	}
	bin := t.TempDir()
	if err := os.Symlink(diff, filepath.Join(bin, "diff")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// Only diff on PATH: fall back silently.
	got, err := lookupDiffTool()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(bin, "diff") {
		t.Fatalf("expected fallback to diff, got %s", got)
	}

	// colordiff present: prefer it.
	if err := os.Symlink(diff, filepath.Join(bin, "colordiff")); err != nil {
		t.Fatal(err)
	}
	got, err = lookupDiffTool()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(bin, "colordiff") {
		t.Fatalf("expected colordiff, got %s", got)
	}

	// Neither present: error.
	t.Setenv("PATH", t.TempDir())
	if _, err := lookupDiffTool(); err == nil {
		t.Fatal("expected error when no diff tool is available")
	}
}

func TestResolvePodman(t *testing.T) {
	c, err := Parse(`
[default]
disable_userns = true

[binaries.podman]
podman = true

[binaries.claude]
podman = false
`)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Resolve("podman"); !p.Podman || p.DisableUserns {
		t.Errorf("podman = true must keep user namespaces enabled: %+v", p)
	}
	if p := c.Resolve("claude"); p.Podman || !p.DisableUserns {
		t.Errorf("podman = false must leave disable_userns alone: %+v", p)
	}
	if p := c.Resolve("other"); p.Podman || !p.DisableUserns {
		t.Errorf("unset podman must default to false: %+v", p)
	}
}

func TestDefaultTOMLPodman(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Resolve("podman"); !p.Podman || p.DisableUserns {
		t.Errorf("the built-in podman section must enable podman: %+v", p)
	}
	for _, name := range []string{"claude", "anything"} {
		if p := c.Resolve(name); p.Podman || !p.DisableUserns {
			t.Errorf("%s must not enable podman by default: %+v", name, p)
		}
	}
	for _, want := range []string{"registry-1.docker.io", "auth.docker.io", "quay.io", "ghcr.io"} {
		if !contains(c.Resolve("podman").AllowHosts, want) {
			t.Errorf("allow_hosts is missing registry %q", want)
		}
	}
}

func TestDefaultTOMLLimits(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Resolve("anything")
	if p.CPUs != 1 || p.Memory != "2G" || p.TmpfsSize != "1G" {
		t.Errorf("default limits: cpus=%v memory=%q tmpfs_size=%q", p.CPUs, p.Memory, p.TmpfsSize)
	}
	c, err = Parse(`
[default]
cpus = 2
memory = "4G"
tmpfs_size = "1G"
[binaries.big]
cpus = 0
memory = ""
`)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Resolve("big"); p.CPUs != 0 || p.Memory != "" || p.TmpfsSize != "1G" {
		t.Errorf("override: cpus=%v memory=%q tmpfs_size=%q", p.CPUs, p.Memory, p.TmpfsSize)
	}
	if p := (&Config{}).Resolve("x"); p.CPUs != 0 || p.Memory != "" || p.TmpfsSize != "" {
		t.Errorf("unset limits are off: cpus=%v memory=%q tmpfs_size=%q", p.CPUs, p.Memory, p.TmpfsSize)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"":           0,
		"0":          0,
		"1024":       1024,
		"2G":         2 << 30,
		"2g":         2 << 30,
		"2GB":        2 << 30,
		"2GiB":       2 << 30,
		"512M":       512 << 20,
		"1.5G":       3 << 29,
		"100K":       100 << 10,
		"1T":         1 << 40,
		" 8M ":       8 << 20,
		"1073741824": 1 << 30,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"lots", "-1", "+2G", "2X", "G", "1.5.2G", "99999999999999999999G"} {
		if got, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", bad, got)
		}
	}
}

func TestResolveDNS(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Resolve("anything").DNS {
		t.Error("dns should be on by default")
	}
	c, err = Parse(`
[default]
dns = false

[binaries.tool]
dns = true
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Resolve("other").DNS || !c.Resolve("tool").DNS {
		t.Errorf("dns: other = %v, tool = %v", c.Resolve("other").DNS, c.Resolve("tool").DNS)
	}
	if !(&Config{}).Resolve("x").DNS {
		t.Error("dns should default to true without a config file")
	}
}

func TestResolveSSH(t *testing.T) {
	c, err := Parse(`
[default]
ssh_key = "~/.ssh/id_ed25519"

[binaries.agent]
ssh = true
ssh_user = "git"
`)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Resolve("agent"); !p.SSH || p.SSHKey != "~/.ssh/id_ed25519" || p.SSHUser != "git" {
		t.Errorf("agent: %+v", p)
	}
	if p := c.Resolve("other"); p.SSH || p.SSHKey != "~/.ssh/id_ed25519" || p.SSHUser != "" {
		t.Errorf("ssh must be off unless set: %+v", p)
	}
	if p, _ := Parse(DefaultLinuxTOML); p.Resolve("claude").SSH {
		t.Error("ssh must be off in the default configuration")
	}
}
