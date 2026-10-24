package config

import (
	"bytes"
	"errors"
	"os"
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
	if len(p.Overlays) != 3 || p.Overlays[0].Path != "~/.local/share/mise" || !p.Overlays[0].Persist ||
		p.Overlays[1].Path != "?~/.local/state/mise" || !p.Overlays[1].Persist ||
		p.Overlays[2].Path != "?~/.local/bin" || !p.Overlays[2].Persist {
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
	if len(p.Overlays) != 7 || p.Overlays[3].Path != "?~/.claude" || p.Overlays[3].Persist {
		t.Errorf("want a temporary ~/.claude overlay after the default ones, got %+v", p.Overlays)
	}
	for i, want := range []string{"?~/.claude/skills", "?~/.claude/plugins", "?~/.agents/skills"} {
		if len(p.Overlays) <= 4+i {
			break
		}
		if o := p.Overlays[4+i]; o.Path != want || !o.Persist {
			t.Errorf("overlays[%d] = %+v, want persistent %s", 4+i, o, want)
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
	for _, want := range []string{"$PWD", "?~/.claude"} {
		if !contains(p.RWBinds, want) {
			t.Errorf("rw_binds = %v, missing %q", p.RWBinds, want)
		}
	}
	if contains(p.RWBinds, "/tmp") || contains(p.RWBinds, "?$TMPDIR") || !p.PrivateTmp {
		t.Errorf("/tmp and $TMPDIR must come from private_tmp, not rw_binds %v", p.RWBinds)
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

func TestEnsureProfileDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box", "box.toml")
	created, err := EnsureProfileDir(path)
	if err != nil || !created {
		t.Fatalf("first call: created=%v err=%v", created, err)
	}
	if fi, err := os.Stat(ProfileDir(path)); err != nil || !fi.IsDir() {
		t.Fatalf("profile.d not created: %v", err)
	}
	created, err = EnsureProfileDir(path)
	if err != nil || created {
		t.Fatalf("second call: created=%v err=%v", created, err)
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

func TestParseProfile(t *testing.T) {
	p, err := ParseProfile("work", `
paths = ["~/work", "/srv/projects"]

[default]
rw_parent = true
allow_hosts = ["gitlab.example.com"]

[binaries.claude]
pass_env = ["WORK_*"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "work" || len(p.Paths) != 2 || p.Default.RWParent == nil || !*p.Default.RWParent {
		t.Errorf("profile = %+v", p)
	}
	if _, err := ParseProfile("x", "[default]\nnetwork = false\n"); err == nil {
		t.Error("expected error for a profile without paths")
	}
	if _, err := ParseProfile("x", "paths = [\"\"]\n"); err == nil {
		t.Error("expected error for an empty path")
	}
	if _, err := ParseProfile("x", "paths = [\"/a\"]\npath = \"/b\"\n"); err == nil {
		t.Error("expected error for an unknown key")
	}
	if _, err := ParseProfile("x", "paths = [\"/a\"]\n[default]\nrw_parnet = true\n"); err == nil {
		t.Error("expected error for a misspelled section key")
	}
}

func TestLoadProfiles(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "box.toml")
	profDir := ProfileDir(cfgPath)
	if profDir != filepath.Join(dir, "profile.d") {
		t.Fatalf("ProfileDir = %q", profDir)
	}

	// No directory: defaults, no profiles.
	c, exists, err := Load(cfgPath)
	if err != nil || exists || len(c.Profiles) != 0 {
		t.Fatalf("Load without profile.d: exists=%v profiles=%d err=%v", exists, len(c.Profiles), err)
	}

	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"b-work.toml":  "paths = [\"/srv/work\"]\n[default]\nrw_parent = true\n",
		"a-home.toml":  "paths = [\"~/home\"]\n",
		"README":       "not a profile",
		"ignored.conf": "paths = [\"/x\"]\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(profDir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, _, err = Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Profiles) != 2 || c.Profiles[0].Name != "a-home" || c.Profiles[1].Name != "b-work" {
		t.Fatalf("profiles = %+v", c.Profiles)
	}
	if want := filepath.Join(profDir, "b-work.toml"); c.Profiles[1].Path != want {
		t.Errorf("Path = %q, want %q", c.Profiles[1].Path, want)
	}

	// A broken profile fails the whole load and names the file.
	if err := os.WriteFile(filepath.Join(profDir, "broken.toml"), []byte("paths = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(cfgPath); err == nil || !strings.Contains(err.Error(), "broken.toml") {
		t.Errorf("expected an error naming broken.toml, got %v", err)
	}
}

func TestMatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PROJECTS", "/srv/projects")
	mk := func(name string, paths ...string) DirProfile {
		return DirProfile{Name: name, Paths: paths}
	}
	c := &Config{Profiles: []DirProfile{
		mk("work", "~/work"),
		mk("secret", "$HOME/work/secret", "/does/not/matter"),
		mk("srv", "$PROJECTS/"),
		mk("root", "/"),
		mk("unset", "$NOT_SET_ANYWHERE"),
	}}
	names := func(dir string) []string {
		var out []string
		for _, p := range c.Match(dir) {
			out = append(out, p.Name)
		}
		return out
	}
	cases := []struct {
		dir  string
		want []string
	}{
		{filepath.Join(home, "work"), []string{"root", "work"}},
		{filepath.Join(home, "work", "api"), []string{"root", "work"}},
		{filepath.Join(home, "work", "secret", "x"), []string{"root", "work", "secret"}},
		{filepath.Join(home, "workshop"), []string{"root"}},
		{"/srv/projects/a", []string{"root", "srv"}},
		{"/srv", []string{"root"}},
	}
	for _, tc := range cases {
		if got := names(tc.dir); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Match(%s) = %v, want %v", tc.dir, got, tc.want)
		}
	}
	if got := (&Config{}).Match(home); len(got) != 0 {
		t.Errorf("Match without profiles = %v", got)
	}
}

func TestMatchResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real", "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatal(err)
	}
	c := &Config{Profiles: []DirProfile{{Name: "real", Paths: []string{filepath.Join(root, "real")}}}}
	if got := c.Match(filepath.Join(link, "proj")); len(got) != 1 {
		t.Errorf("a symlinked working directory should match the real path, got %v", got)
	}
	c = &Config{Profiles: []DirProfile{{Name: "link", Paths: []string{link}}}}
	if got := c.Match(real); len(got) != 1 {
		t.Errorf("a symlinked profile path should match the real working directory, got %v", got)
	}
}

func TestResolveWithProfiles(t *testing.T) {
	c, err := Parse(`
[default]
ro_binds = ["/usr"]
rw_binds = ["$PWD"]
allow_hosts = ["github.com"]
cpus = 1
env = { A = "1" }

[binaries.claude]
pass_env = ["CLAUDE_*"]
allow_hosts = ["*.anthropic.com"]
`)
	if err != nil {
		t.Fatal(err)
	}
	work, err := ParseProfile("work", `
paths = ["/w"]

[default]
rw_parent = true
ro_binds = ["/opt/work"]
drop_binds = ["/usr"]
allow_hosts = ["gitlab.example.com"]
cpus = 4
env = { B = "2" }

[binaries.claude]
pass_env = ["WORK_*"]
unset_env = ["A"]
memory = "8G"
`)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ParseProfile("secret", `
paths = ["/w/secret"]

[default]
rw_parent = false
network = false
proxy = false
`)
	if err != nil {
		t.Fatal(err)
	}

	// Without profiles nothing changes.
	if p := c.Resolve("claude"); p.RWParent || p.CPUs != 1 || len(p.ROBinds) != 1 {
		t.Errorf("Resolve without profiles = %+v", p)
	}

	p := c.Resolve("claude", *work)
	if !p.RWParent {
		t.Error("rw_parent should come from the profile")
	}
	if got, want := p.ROBinds, []string{"/opt/work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ro_binds = %v, want %v (drop_binds in a profile drops inherited entries)", got, want)
	}
	if got, want := p.RWBinds, []string{"$PWD"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rw_binds = %v, want %v", got, want)
	}
	if got, want := p.AllowHosts, []string{"github.com", "*.anthropic.com", "gitlab.example.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("allow_hosts = %v, want %v", got, want)
	}
	if got, want := p.PassEnv, []string{"CLAUDE_*", "WORK_*"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pass_env = %v, want %v", got, want)
	}
	if got, want := p.Env, map[string]string{"B": "2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
	if p.CPUs != 4 || p.Memory != "8G" {
		t.Errorf("limits: cpus=%v memory=%q", p.CPUs, p.Memory)
	}

	// The profile's binary section does not leak into other binaries.
	if o := c.Resolve("other", *work); o.Memory != "" || len(o.PassEnv) != 0 || !o.RWParent {
		t.Errorf("Resolve(other, work) = %+v", o)
	}

	// Later profiles win.
	p = c.Resolve("claude", *work, *secret)
	if p.RWParent || p.Network || p.Proxy || p.CPUs != 4 {
		t.Errorf("Resolve(claude, work, secret) = %+v", p)
	}
}

func TestResolveRWParentDefault(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatal(err)
	}
	if c.Resolve("claude").RWParent {
		t.Error("rw_parent must be off by default")
	}
	c, err = Parse("[default]\nrw_parent = true\n[binaries.x]\nrw_parent = false\n")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Resolve("y").RWParent || c.Resolve("x").RWParent {
		t.Error("rw_parent should be a scalar that binary sections override")
	}
}

func TestResolvePasta(t *testing.T) {
	c, err := Parse(`
[default]
pasta = true

[binaries.claude]
pasta = false
`)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Resolve("tool"); !p.Pasta {
		t.Errorf("pasta = true in [default]: %+v", p)
	}
	if p := c.Resolve("claude"); p.Pasta {
		t.Errorf("pasta = false in the binary's section must win: %+v", p)
	}
	if p := (&Config{}).Resolve("other"); p.Pasta {
		t.Errorf("unset pasta must default to false: %+v", p)
	}
	d, err := Parse(DefaultTOML)
	if err != nil {
		t.Fatal(err)
	}
	if p := d.Resolve("tool"); p.Pasta {
		t.Errorf("the default configuration must use the proxy: %+v", p)
	}
}
