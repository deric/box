// Package config loads and merges the box configuration file.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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
	RWParent      *bool             `toml:"rw_parent"`
	DevBinds      []string          `toml:"dev_binds"`
	Tmpfs         []string          `toml:"tmpfs"`
	Overlays      []Overlay         `toml:"overlays"`
	CopyFiles     []string          `toml:"copy_files"`
	SyncFiles     []string          `toml:"sync_files"`
	DropBinds     []string          `toml:"drop_binds"`
	PrivateTmp    *bool             `toml:"private_tmp"`
	Network       *bool             `toml:"network"`
	Proxy         *bool             `toml:"proxy"`
	DNS           *bool             `toml:"dns"`
	AllowHosts    []string          `toml:"allow_hosts"`
	LogDir        *string           `toml:"log_dir"`
	Hostname      *string           `toml:"hostname"`
	NewSession    *bool             `toml:"new_session"`
	DisableUserns *bool             `toml:"disable_userns"`
	DieWithParent *bool             `toml:"die_with_parent"`
	ClearEnv      *bool             `toml:"clear_env"`
	PassEnv       []string          `toml:"pass_env"`
	Env           map[string]string `toml:"env"`
	UnsetEnv      []string          `toml:"unset_env"`
	BindBinary    *bool             `toml:"bind_binary"`
	Podman        *bool             `toml:"podman"`
	SSH           *bool             `toml:"ssh"`
	SSHKey        *string           `toml:"ssh_key"`
	SSHUser       *string           `toml:"ssh_user"`
	CPUs          *float64          `toml:"cpus"`
	Memory        *string           `toml:"memory"`
	TmpfsSize     *string           `toml:"tmpfs_size"`
	ExtraArgs     []string          `toml:"extra_args"`
	SeatbeltRules []string          `toml:"seatbelt_rules"`
	Inherit       *bool             `toml:"inherit"`
}

// Config is the whole configuration file.
type Config struct {
	Default  Section            `toml:"default"`
	Binaries map[string]Section `toml:"binaries"`
	// Profiles are the directory profiles loaded from profile.d next to the
	// configuration file, in file name order (see Load and Match).
	Profiles []DirProfile `toml:"-"`
}

// DirProfile is a profile from profile.d: configuration that applies to
// sandboxes started in a working directory under one of its Paths. It is
// merged on top of the configuration file, its [default] section first and
// its [binaries.<name>] section second, with the same rules: lists are
// appended, scalars replace, drop_binds removes inherited entries.
type DirProfile struct {
	// Name is the file name without the .toml suffix.
	Name string `toml:"-"`
	// Path is the file the profile was read from.
	Path string `toml:"-"`
	// Paths are the directories the profile applies under; they accept ~,
	// $HOME and other environment variables like any configured path.
	Paths    []string           `toml:"paths"`
	Default  Section            `toml:"default"`
	Binaries map[string]Section `toml:"binaries"`
}

// DefaultLogDir is where the proxy writes its logs unless log_dir says
// otherwise; it is also where private /tmp directories and sockets live.
const DefaultLogDir = "/tmp/box"

// Profile is the fully resolved configuration for one binary.
type Profile struct {
	ROBinds []string `toml:"ro_binds"`
	RWBinds []string `toml:"rw_binds"`
	// RWParent also mounts the parent of the working directory read-write,
	// unless that parent is the root or the home directory (see package
	// sandbox). Meant for profiles whose projects reach into siblings.
	RWParent   bool      `toml:"rw_parent"`
	DevBinds   []string  `toml:"dev_binds"`
	Tmpfs      []string  `toml:"tmpfs"`
	Overlays   []Overlay `toml:"overlays"`
	CopyFiles  []string  `toml:"copy_files"`
	SyncFiles  []string  `toml:"sync_files"`
	PrivateTmp bool      `toml:"private_tmp"`
	Network    bool      `toml:"network"`
	Proxy      bool      `toml:"proxy"`
	// DNS lets a sandbox behind the proxy resolve names: a listener on
	// 127.0.0.1:53 inside relays queries through the proxy to the host's
	// resolver (see package sandbox). It has no effect with Network set
	// or Proxy unset.
	DNS           bool              `toml:"dns"`
	AllowHosts    []string          `toml:"allow_hosts"`
	LogDir        string            `toml:"log_dir"`
	Hostname      string            `toml:"hostname"`
	NewSession    bool              `toml:"new_session"`
	DisableUserns bool              `toml:"disable_userns"`
	DieWithParent bool              `toml:"die_with_parent"`
	ClearEnv      bool              `toml:"clear_env"`
	PassEnv       []string          `toml:"pass_env"`
	Env           map[string]string `toml:"env"`
	UnsetEnv      []string          `toml:"unset_env"`
	BindBinary    bool              `toml:"bind_binary"`
	// Podman sets the sandbox up for rootless podman and buildah: it keeps
	// user namespaces enabled (podman needs its own), mounts /sys read-only
	// and a per-binary image store, and writes a containers.conf and
	// storage.conf suited to a single-UID user namespace (see package
	// sandbox). It implies DisableUserns = false.
	Podman bool `toml:"podman"`
	// SSH lets the sandbox open SSH connections to AllowHosts: the proxy
	// tunnels CONNECT requests to port 22, which it refuses otherwise, and
	// an ssh_config generated inside routes every connection through it
	// (see package sandbox). SSHKey names a host private key to mount
	// read-only at its own path and use as the identity, SSHUser the user
	// name to log in as; both are optional and have no effect without SSH.
	SSH     bool   `toml:"ssh"`
	SSHKey  string `toml:"ssh_key"`
	SSHUser string `toml:"ssh_user"`
	// CPUs and Memory bound the sandbox and everything it starts: CPU time
	// per second in cores (1.5 is one and a half cores) and bytes of memory
	// (a size such as "2G", see ParseSize), swap excluded. On Linux they
	// are applied as a transient systemd scope, a cgroup of the sandbox's
	// own. Zero or empty means unlimited.
	CPUs   float64 `toml:"cpus"`
	Memory string  `toml:"memory"`
	// TmpfsSize caps each tmpfs mount (bwrap --size); empty means the
	// kernel default of half the physical memory per mount.
	TmpfsSize     string   `toml:"tmpfs_size"`
	ExtraArgs     []string `toml:"extra_args"`
	SeatbeltRules []string `toml:"seatbelt_rules"`
}

// ParseSize parses a byte count such as "2G", "512M", "1.5G" or
// "1073741824". The suffixes K, M, G and T are binary (1024-based), match
// regardless of case and may be followed by B or iB. An empty string or "0"
// is 0, which means no limit wherever a size is used.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	num := strings.TrimRight(s, "bBiI"+"kKmMgGtT")
	suffix := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(s[len(num):]), "B"), "I"))
	mult := uint64(1)
	switch suffix {
	case "":
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if num == "" || num[0] == '-' || num[0] == '+' {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n, err := strconv.ParseUint(num, 10, 64); err == nil {
		if n > ^uint64(0)/mult {
			return 0, fmt.Errorf("size %q is too large", s)
		}
		return n * mult, nil
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 || f*float64(mult) >= float64(^uint64(0)) {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(f * float64(mult)), nil
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

// ProfileDir returns the directory profiles are read from: profile.d next
// to the configuration file at configPath.
func ProfileDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "profile.d")
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
	if err := decode(text, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ParseProfile decodes the TOML text of a profile named name. A profile
// must list at least one path.
func ParseProfile(name, text string) (*DirProfile, error) {
	var p DirProfile
	if err := decode(text, &p); err != nil {
		return nil, err
	}
	p.Name = name
	if len(p.Paths) == 0 {
		return nil, fmt.Errorf("profile has no paths")
	}
	for _, path := range p.Paths {
		if path == "" {
			return nil, fmt.Errorf("profile has an empty path")
		}
	}
	return &p, nil
}

// decode decodes text into v and rejects keys v has no field for.
func decode(text string, v any) error {
	md, err := toml.Decode(text, v)
	if err != nil {
		return err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return fmt.Errorf("unknown configuration keys: %s", strings.Join(keys, ", "))
	}
	return nil
}

// LoadProfiles reads every *.toml file in dir as a profile, in file name
// order. A missing directory yields no profiles.
func LoadProfiles(dir string) ([]DirProfile, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var profiles []DirProfile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		p, err := ParseProfile(strings.TrimSuffix(e.Name(), ".toml"), string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		p.Path = path
		profiles = append(profiles, *p)
	}
	return profiles, nil
}

// Match returns the profiles that apply to a sandbox started in dir: those
// with a path that is dir or a parent of it, either as given or with
// symlinks resolved. The profile with the deepest matching path comes
// last, so it wins when they are merged in order; ties keep file name
// order.
func (c *Config) Match(dir string) []DirProfile {
	dir = filepath.Clean(dir)
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, real)
	}
	home, _ := os.UserHomeDir()
	type match struct {
		profile DirProfile
		depth   int
	}
	var matches []match
	for _, p := range c.Profiles {
		best := -1
		for _, raw := range p.Paths {
			root := expandPath(raw, home)
			if root == "" {
				continue
			}
			for _, d := range dirs {
				if d == root || strings.HasPrefix(d, strings.TrimSuffix(root, "/")+"/") {
					if n := strings.Count(root, "/"); n > best {
						best = n
					}
				}
			}
		}
		if best >= 0 {
			matches = append(matches, match{p, best})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].depth < matches[j].depth })
	out := make([]DirProfile, len(matches))
	for i, m := range matches {
		out[i] = m.profile
	}
	return out
}

// expandPath expands ~, $HOME and other environment variables in a profile
// path and cleans it. It returns "" for a path that expands to nothing.
func expandPath(p, home string) string {
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	p = os.Expand(p, func(k string) string {
		if k == "HOME" {
			return home
		}
		return os.Getenv(k)
	})
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// Load reads the configuration file at path and the profiles in profile.d
// next to it. A missing file yields the built-in defaults; the returned
// bool reports whether the file existed.
func Load(path string) (*Config, bool, error) {
	var c *Config
	data, err := os.ReadFile(path)
	exists := true
	switch {
	case os.IsNotExist(err):
		exists = false
		if c, err = Parse(DefaultTOML); err != nil {
			return nil, false, err
		}
	case err != nil:
		return nil, false, err
	default:
		if c, err = Parse(string(data)); err != nil {
			return nil, true, fmt.Errorf("%s: %w", path, err)
		}
	}
	if c.Profiles, err = LoadProfiles(ProfileDir(path)); err != nil {
		return nil, exists, err
	}
	return c, exists, nil
}

// ErrExists is returned by WriteDefault when the target file already exists.
var ErrExists = errors.New("config file already exists")

// WriteDefault writes DefaultTOML to path, creating parent directories. Unless
// force is set it refuses to overwrite an existing file and returns ErrExists.
func WriteDefault(path string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s: %w", path, ErrExists)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(DefaultTOML), 0o644)
}

// EnsureProfileDir creates the profile directory next to the configuration
// file at configPath when it is missing, and reports whether it did.
func EnsureProfileDir(configPath string) (created bool, err error) {
	dir := ProfileDir(configPath)
	if _, err := os.Stat(dir); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

// DiffDefault writes a unified diff between the file at path and the
// built-in DefaultTOML to w and reports whether the two differ. It shells out
// to colordiff(1) when available, falling back to diff(1). Nothing is written
// when the file matches the defaults.
func DiffDefault(path string, w io.Writer) (bool, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if string(current) == DefaultTOML {
		return false, nil
	}
	diff, err := lookupDiffTool()
	if err != nil {
		return false, err
	}
	cmd := exec.Command(diff, "-u", "-L", path, "-L", "box defaults", path, "-")
	cmd.Stdin = strings.NewReader(DefaultTOML)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		// diff exits 1 when the inputs differ; anything else is a failure.
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return false, fmt.Errorf("diff failed: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	return true, nil
}

// lookupDiffTool returns the path to colordiff if installed, otherwise diff.
func lookupDiffTool() (string, error) {
	if p, err := exec.LookPath("colordiff"); err == nil {
		return p, nil
	}
	p, err := exec.LookPath("diff")
	if err != nil {
		return "", fmt.Errorf("diff tool not found: %w", err)
	}
	return p, nil
}

// Resolve merges the default section, the section for name (the binary's
// base name) and then each of the given profiles, in order, and returns the
// effective profile. Profiles usually come from Match. At every step lists
// are appended, scalars replace, drop_binds removes entries merged so far
// and inherit = false discards them altogether.
func (c *Config) Resolve(name string, profiles ...DirProfile) Profile {
	sections := []Section{c.Default}
	if sec, ok := c.Binaries[name]; ok {
		sections = append(sections, sec)
	}
	for _, p := range profiles {
		sections = append(sections, p.Default)
		if sec, ok := p.Binaries[name]; ok {
			sections = append(sections, sec)
		}
	}
	var merged Section
	for _, sec := range sections {
		merged = merge(merged, sec)
	}
	return finalize(merged)
}

// merge lays over on top of base. List values are appended (minus
// over.DropBinds), scalar values set in over replace those of base, and
// over.Inherit = false discards base first.
func merge(base, over Section) Section {
	if over.Inherit != nil && !*over.Inherit {
		base = Section{}
	}
	drop := over.DropBinds
	m := Section{
		ROBinds:       mergeList(base.ROBinds, over.ROBinds, drop),
		RWBinds:       mergeList(base.RWBinds, over.RWBinds, drop),
		RWParent:      pick(base.RWParent, over.RWParent),
		DevBinds:      mergeList(base.DevBinds, over.DevBinds, drop),
		Tmpfs:         mergeList(base.Tmpfs, over.Tmpfs, drop),
		Overlays:      mergeOverlays(base.Overlays, over.Overlays, drop),
		CopyFiles:     mergeList(base.CopyFiles, over.CopyFiles, drop),
		SyncFiles:     mergeList(base.SyncFiles, over.SyncFiles, drop),
		PrivateTmp:    pick(base.PrivateTmp, over.PrivateTmp),
		Network:       pick(base.Network, over.Network),
		Proxy:         pick(base.Proxy, over.Proxy),
		DNS:           pick(base.DNS, over.DNS),
		AllowHosts:    mergeList(base.AllowHosts, over.AllowHosts, nil),
		LogDir:        pick(base.LogDir, over.LogDir),
		Hostname:      pick(base.Hostname, over.Hostname),
		NewSession:    pick(base.NewSession, over.NewSession),
		DisableUserns: pick(base.DisableUserns, over.DisableUserns),
		DieWithParent: pick(base.DieWithParent, over.DieWithParent),
		ClearEnv:      pick(base.ClearEnv, over.ClearEnv),
		PassEnv:       mergeList(base.PassEnv, over.PassEnv, nil),
		Env:           map[string]string{},
		UnsetEnv:      mergeList(base.UnsetEnv, over.UnsetEnv, nil),
		BindBinary:    pick(base.BindBinary, over.BindBinary),
		Podman:        pick(base.Podman, over.Podman),
		SSH:           pick(base.SSH, over.SSH),
		SSHKey:        pick(base.SSHKey, over.SSHKey),
		SSHUser:       pick(base.SSHUser, over.SSHUser),
		CPUs:          pick(base.CPUs, over.CPUs),
		Memory:        pick(base.Memory, over.Memory),
		TmpfsSize:     pick(base.TmpfsSize, over.TmpfsSize),
		ExtraArgs:     append(append([]string{}, base.ExtraArgs...), over.ExtraArgs...),
		SeatbeltRules: append(append([]string{}, base.SeatbeltRules...), over.SeatbeltRules...),
	}
	for k, v := range base.Env {
		m.Env[k] = v
	}
	for k, v := range over.Env {
		m.Env[k] = v
	}
	return m
}

// finalize turns a merged section into a profile, filling unset scalars
// with the built-in defaults and applying unset_env.
func finalize(s Section) Profile {
	p := Profile{
		ROBinds:       s.ROBinds,
		RWBinds:       s.RWBinds,
		RWParent:      orElse(s.RWParent, false),
		DevBinds:      s.DevBinds,
		Tmpfs:         s.Tmpfs,
		Overlays:      s.Overlays,
		CopyFiles:     s.CopyFiles,
		SyncFiles:     s.SyncFiles,
		PrivateTmp:    orElse(s.PrivateTmp, false),
		Network:       orElse(s.Network, true),
		Proxy:         orElse(s.Proxy, false),
		DNS:           orElse(s.DNS, true),
		AllowHosts:    s.AllowHosts,
		LogDir:        orElse(s.LogDir, DefaultLogDir),
		Hostname:      orElse(s.Hostname, ""),
		NewSession:    orElse(s.NewSession, false),
		DisableUserns: orElse(s.DisableUserns, false),
		DieWithParent: orElse(s.DieWithParent, true),
		ClearEnv:      orElse(s.ClearEnv, false),
		Env:           map[string]string{},
		UnsetEnv:      s.UnsetEnv,
		BindBinary:    orElse(s.BindBinary, true),
		Podman:        orElse(s.Podman, false),
		SSH:           orElse(s.SSH, false),
		SSHKey:        orElse(s.SSHKey, ""),
		SSHUser:       orElse(s.SSHUser, ""),
		CPUs:          orElse(s.CPUs, 0),
		Memory:        orElse(s.Memory, ""),
		TmpfsSize:     orElse(s.TmpfsSize, ""),
		ExtraArgs:     s.ExtraArgs,
		SeatbeltRules: s.SeatbeltRules,
	}
	if p.ExtraArgs == nil {
		p.ExtraArgs = []string{}
	}
	if p.SeatbeltRules == nil {
		p.SeatbeltRules = []string{}
	}
	if p.Podman {
		// Rootless podman re-executes itself inside a user namespace of its
		// own; with --disable-userns it cannot even start.
		p.DisableUserns = false
	}
	for k, v := range s.Env {
		p.Env[k] = v
	}
	for _, k := range p.UnsetEnv {
		delete(p.Env, k)
	}
	p.PassEnv = mergeList(s.PassEnv, nil, p.UnsetEnv)
	return p
}

// pick returns override when set, else base.
func pick[T any](base, override *T) *T {
	if override != nil {
		return override
	}
	return base
}

// orElse dereferences p, falling back to fallback when it is nil.
func orElse[T any](p *T, fallback T) T {
	if p != nil {
		return *p
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
