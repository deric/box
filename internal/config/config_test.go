package config

import (
	"reflect"
	"testing"
)

func TestDefaultTOMLParses(t *testing.T) {
	c, err := Parse(DefaultLinuxTOML)
	if err != nil {
		t.Fatalf("default config does not parse: %v", err)
	}
	p := c.Resolve("anything")
	if !p.Network || p.Hostname != "box" || !p.BindBinary || !p.DieWithParent {
		t.Errorf("unexpected default scalars: %+v", p)
	}
	if len(p.Overlays) != 1 || p.Overlays[0].Path != "~/.local/share/mise" || !p.Overlays[0].Persist {
		t.Errorf("unexpected default overlays: %+v", p.Overlays)
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
	for _, want := range []string{"$PWD", "?~/.claude", "?~/.claude.json"} {
		if !contains(p.RWBinds, want) {
			t.Errorf("rw_binds = %v, missing %q", p.RWBinds, want)
		}
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
	if !p.Network || len(p.Overlays) != 0 || p.Env["BOX_SANDBOX"] != "1" {
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
