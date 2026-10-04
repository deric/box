// Package config loads and merges the box configuration file.
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// Built-in configurations for each supported platform.
var (
	//go:embed default_linux.toml
	DefaultLinuxTOML string
	//go:embed default_darwin.toml
	DefaultDarwinTOML string
)

// DefaultTOML is the configuration for the current platform, written by
// `box init` and used when no configuration file exists.
var DefaultTOML = defaultFor(runtime.GOOS)

func defaultFor(goos string) string {
	if goos == "darwin" {
		return DefaultDarwinTOML
	}
	return DefaultLinuxTOML
}

// Overlay describes an overlay mount. It decodes from either a plain string
// (the path, persist = true) or a table {path = "...", persist = bool}.
type Overlay struct {
	Path    string
	Persist bool
}

// UnmarshalTOML implements toml.Unmarshaler.
func (o *Overlay) UnmarshalTOML(v any) error {
	switch t := v.(type) {
	case string:
		o.Path, o.Persist = t, true
		return nil
	case map[string]any:
		o.Persist = true
		for k, val := range t {
			switch k {
			case "path":
				s, ok := val.(string)
				if !ok {
					return fmt.Errorf("overlay path must be a string")
				}
				o.Path = s
			case "persist":
				b, ok := val.(bool)
				if !ok {
					return fmt.Errorf("overlay persist must be a boolean")
				}
				o.Persist = b
			default:
				return fmt.Errorf("unknown overlay key %q", k)
			}
		}
		if o.Path == "" {
			return fmt.Errorf("overlay is missing a path")
		}
		return nil
	default:
		return fmt.Errorf("overlay must be a string or a table, got %T", v)
	}
}

// MarshalTOML implements toml.Marshaler so effective profiles round-trip.
func (o Overlay) MarshalTOML() ([]byte, error) {
	return []byte(fmt.Sprintf("{ path = %q, persist = %t }", o.Path, o.Persist)), nil
}

// Section is one configuration section as written in the file. Scalar fields
// are pointers so that "unset" can be told apart from a zero value.
type Section struct {
	ROBinds       []string          `toml:"ro_binds"`
	RWBinds       []string          `toml:"rw_binds"`
	DevBinds      []string          `toml:"dev_binds"`
	Tmpfs         []string          `toml:"tmpfs"`
	Overlays      []Overlay         `toml:"overlays"`
	DropBinds     []string          `toml:"drop_binds"`
	Network       *bool             `toml:"network"`
	Hostname      *string           `toml:"hostname"`
	NewSession    *bool             `toml:"new_session"`
	DieWithParent *bool             `toml:"die_with_parent"`
	ClearEnv      *bool             `toml:"clear_env"`
	Env           map[string]string `toml:"env"`
	UnsetEnv      []string          `toml:"unset_env"`
	BindBinary    *bool             `toml:"bind_binary"`
	ExtraArgs     []string          `toml:"extra_args"`
	SeatbeltRules []string          `toml:"seatbelt_rules"`
	Inherit       *bool             `toml:"inherit"`
}

// Config is the whole configuration file.
type Config struct {
	Default  Section            `toml:"default"`
	Binaries map[string]Section `toml:"binaries"`
}

// Profile is the fully resolved configuration for one binary.
type Profile struct {
	ROBinds       []string          `toml:"ro_binds"`
	RWBinds       []string          `toml:"rw_binds"`
	DevBinds      []string          `toml:"dev_binds"`
	Tmpfs         []string          `toml:"tmpfs"`
	Overlays      []Overlay         `toml:"overlays"`
	Network       bool              `toml:"network"`
	Hostname      string            `toml:"hostname"`
	NewSession    bool              `toml:"new_session"`
	DieWithParent bool              `toml:"die_with_parent"`
	ClearEnv      bool              `toml:"clear_env"`
	Env           map[string]string `toml:"env"`
	UnsetEnv      []string          `toml:"unset_env"`
	BindBinary    bool              `toml:"bind_binary"`
	ExtraArgs     []string          `toml:"extra_args"`
	SeatbeltRules []string          `toml:"seatbelt_rules"`
}

// Path returns the configuration file location: $BOX_CONFIG, else
// $XDG_CONFIG_HOME/box/box.toml, else ~/.config/box/box.toml.
func Path() (string, error) {
	if p := os.Getenv("BOX_CONFIG"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "box", "box.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "box", "box.toml"), nil
}

// StateDir returns the directory for persistent overlay layers:
// $XDG_STATE_HOME/box or ~/.local/state/box.
func StateDir() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "box"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "box"), nil
}

// Parse decodes TOML configuration text.
func Parse(text string) (*Config, error) {
	var c Config
	md, err := toml.Decode(text, &c)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown configuration keys: %s", strings.Join(keys, ", "))
	}
	return &c, nil
}

// Load reads the configuration file at path. A missing file yields the
// built-in defaults; the returned bool reports whether the file existed.
func Load(path string) (*Config, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		c, perr := Parse(DefaultTOML)
		return c, false, perr
	}
	if err != nil {
		return nil, false, err
	}
	c, err := Parse(string(data))
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	return c, true, nil
}

// WriteDefault writes DefaultTOML to path, creating parent directories. It
// refuses to overwrite an existing file.
func WriteDefault(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(DefaultTOML), 0o644)
}

// Resolve merges the default section with the section for name (the binary's
// base name) and returns the effective profile.
func (c *Config) Resolve(name string) Profile {
	base := c.Default
	sec, hasSec := c.Binaries[name]
	if hasSec && sec.Inherit != nil && !*sec.Inherit {
		base = Section{}
	}

	p := Profile{
		ROBinds:       mergeList(base.ROBinds, sec.ROBinds, sec.DropBinds),
		RWBinds:       mergeList(base.RWBinds, sec.RWBinds, sec.DropBinds),
		DevBinds:      mergeList(base.DevBinds, sec.DevBinds, sec.DropBinds),
		Tmpfs:         mergeList(base.Tmpfs, sec.Tmpfs, sec.DropBinds),
		Overlays:      mergeOverlays(base.Overlays, sec.Overlays, sec.DropBinds),
		Network:       pick(base.Network, sec.Network, true),
		Hostname:      pick(base.Hostname, sec.Hostname, ""),
		NewSession:    pick(base.NewSession, sec.NewSession, false),
		DieWithParent: pick(base.DieWithParent, sec.DieWithParent, true),
		ClearEnv:      pick(base.ClearEnv, sec.ClearEnv, false),
		Env:           map[string]string{},
		UnsetEnv:      mergeList(base.UnsetEnv, sec.UnsetEnv, nil),
		BindBinary:    pick(base.BindBinary, sec.BindBinary, true),
		ExtraArgs:     append(append([]string{}, base.ExtraArgs...), sec.ExtraArgs...),
		SeatbeltRules: append(append([]string{}, base.SeatbeltRules...), sec.SeatbeltRules...),
	}
	for k, v := range base.Env {
		p.Env[k] = v
	}
	for k, v := range sec.Env {
		p.Env[k] = v
	}
	for _, k := range p.UnsetEnv {
		delete(p.Env, k)
	}
	return p
}

func pick[T any](base, override *T, fallback T) T {
	if override != nil {
		return *override
	}
	if base != nil {
		return *base
	}
	return fallback
}

func mergeList(base, extra, drop []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range base {
		if !contains(drop, s) {
			add(s)
		}
	}
	for _, s := range extra {
		add(s)
	}
	return out
}

func mergeOverlays(base, extra []Overlay, drop []string) []Overlay {
	var out []Overlay
	seen := map[string]bool{}
	add := func(o Overlay) {
		if o.Path == "" || seen[o.Path] {
			return
		}
		seen[o.Path] = true
		out = append(out, o)
	}
	for _, o := range base {
		if !contains(drop, o.Path) {
			add(o)
		}
	}
	for _, o := range extra {
		add(o)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
