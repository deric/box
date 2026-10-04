// Package sandbox turns a resolved profile into a bwrap command line.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"box/internal/config"
)

// Options carries the per-invocation inputs that are not part of the profile.
type Options struct {
	Binary   string   // command as typed by the user
	Args     []string // arguments for the command
	Cwd      string   // host working directory; defaults to os.Getwd()
	Home     string   // host home directory; defaults to os.UserHomeDir()
	StateDir string   // root for persistent overlay layers
	// Lookup resolves the command on the host. Defaults to exec.LookPath.
	Lookup func(string) (string, error)
}

type mountKind int

const (
	kindRO mountKind = iota
	kindRW
	kindDev
	kindTmpfs
	kindSymlink
	kindOverlay
)

type mount struct {
	kind    mountKind
	src     string // host path (bind, overlay lower) or link target (symlink)
	dest    string
	upper   string // overlay only; empty means a temporary overlay
	work    string
	order   int
	visible bool // true for binds and overlays: dest exposes host content
}

// ProcTitlePrefix starts argv[0] of every bwrap process started by Exec,
// followed by the sandboxed binary's base name. It lets `box ps` tell box
// sandboxes apart from other bwrap users.
const ProcTitlePrefix = "box:"

// Plan is a fully computed bwrap invocation.
type Plan struct {
	Name     string   // base name of the sandboxed binary
	Bwrap    string   // path to the bwrap executable
	Args     []string // bwrap arguments, without argv[0]
	Warnings []string
	// Dirs lists directories that must exist before running (overlay layers).
	Dirs []string
}

// Command returns the invocation as a shell-quoted string.
func (p *Plan) Command() string {
	parts := make([]string, 0, len(p.Args)+1)
	parts = append(parts, shellQuote(p.Bwrap))
	for _, a := range p.Args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// Exec creates any required directories and replaces the current process
// with bwrap, tagging argv[0] with ProcTitlePrefix. It only returns on error.
func (p *Plan) Exec() error {
	for _, d := range p.Dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	argv := append([]string{ProcTitlePrefix + p.Name}, p.Args...)
	return syscall.Exec(p.Bwrap, argv, os.Environ())
}

// Build computes the bwrap command line for the given profile and options.
func Build(prof config.Profile, opts Options) (*Plan, error) {
	if opts.Binary == "" {
		return nil, fmt.Errorf("no command given")
	}
	if err := fillDefaults(&opts); err != nil {
		return nil, err
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bwrap not found in PATH: install bubblewrap")
	}

	name := filepath.Base(opts.Binary)
	plan := &Plan{Name: name, Bwrap: bwrap}
	exp := expander(opts)
	var mounts []*mount
	add := func(m *mount) {
		m.dest = filepath.Clean(m.dest)
		m.order = len(mounts)
		mounts = append(mounts, m)
	}

	for _, p := range prof.ROBinds {
		if m := bindMount(exp, p, kindRO, plan); m != nil {
			add(m)
		}
	}
	for _, p := range prof.RWBinds {
		if m := bindMount(exp, p, kindRW, plan); m != nil {
			add(m)
		}
	}
	for _, p := range prof.DevBinds {
		if m := bindMount(exp, p, kindDev, plan); m != nil {
			add(m)
		}
	}
	for _, p := range prof.Tmpfs {
		dest, _ := exp(p)
		add(&mount{kind: kindTmpfs, dest: dest})
	}
	for _, o := range prof.Overlays {
		src, optional := exp(o.Path)
		fi, err := os.Stat(src)
		if err != nil {
			if !optional {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("overlay %s skipped: %v", src, err))
			}
			continue
		}
		if !fi.IsDir() {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("overlay %s skipped: not a directory", src))
			continue
		}
		m := &mount{kind: kindOverlay, src: src, dest: src, visible: true}
		if o.Persist {
			if opts.StateDir == "" {
				return nil, fmt.Errorf("persistent overlay %s requires a state directory", src)
			}
			layer := filepath.Join(opts.StateDir, "overlays", name, layerName(src))
			m.upper = filepath.Join(layer, "upper")
			m.work = filepath.Join(layer, "work")
			plan.Dirs = append(plan.Dirs, m.upper, m.work)
		}
		add(m)
	}

	// Mount the executable itself if nothing else exposes it.
	resolved, err := opts.Lookup(opts.Binary)
	if err != nil {
		return nil, fmt.Errorf("%s: command not found on host", opts.Binary)
	}
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(opts.Cwd, resolved)
	}
	if prof.BindBinary {
		candidates := []string{resolved}
		if real, err := filepath.EvalSymlinks(resolved); err == nil && real != resolved {
			candidates = append(candidates, real)
		}
		for _, c := range candidates {
			if !covered(mounts, c) {
				add(&mount{kind: kindRO, src: c, dest: c, visible: true})
			}
		}
	}

	// Parents must be mounted before children so a tmpfs on $HOME does not
	// hide a bind placed under it.
	sort.SliceStable(mounts, func(i, j int) bool {
		di, dj := depth(mounts[i].dest), depth(mounts[j].dest)
		if di != dj {
			return di < dj
		}
		return mounts[i].order < mounts[j].order
	})

	args := []string{"--unshare-all"}
	if prof.Network {
		args = append(args, "--share-net")
	}
	if prof.Hostname != "" {
		args = append(args, "--hostname", prof.Hostname)
	}
	if prof.DieWithParent {
		args = append(args, "--die-with-parent")
	}
	if prof.NewSession {
		args = append(args, "--new-session")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	for _, m := range mounts {
		args = append(args, m.args()...)
	}
	args = append(args, "--chdir", opts.Cwd)
	if prof.ClearEnv {
		args = append(args, "--clearenv")
	}
	for _, k := range sortedKeys(prof.Env) {
		args = append(args, "--setenv", k, prof.Env[k])
	}
	for _, k := range prof.UnsetEnv {
		args = append(args, "--unsetenv", k)
	}
	args = append(args, prof.ExtraArgs...)
	args = append(args, "--", opts.Binary)
	args = append(args, opts.Args...)
	plan.Args = args
	return plan, nil
}

func fillDefaults(o *Options) error {
	var err error
	if o.Cwd == "" {
		if o.Cwd, err = os.Getwd(); err != nil {
			return err
		}
	}
	if o.Home == "" {
		if o.Home, err = os.UserHomeDir(); err != nil {
			return err
		}
	}
	if o.Lookup == nil {
		o.Lookup = exec.LookPath
	}
	return nil
}

// expander returns a function expanding ~, $HOME, $PWD and other variables.
// A leading "?" marks the path as optional: it is skipped silently when it
// does not exist on the host. The returned bool reports that marker.
func expander(o Options) func(string) (string, bool) {
	mapping := func(k string) string {
		switch k {
		case "PWD":
			return o.Cwd
		case "HOME":
			return o.Home
		}
		return os.Getenv(k)
	}
	return func(p string) (string, bool) {
		optional := strings.HasPrefix(p, "?")
		p = strings.TrimPrefix(p, "?")
		if p == "~" {
			p = o.Home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(o.Home, p[2:])
		}
		return filepath.Clean(os.Expand(p, mapping)), optional
	}
}

// bindMount describes how to expose one host path. Symlinks on the host are
// recreated as symlinks (merged-/usr systems link /bin -> usr/bin); missing
// paths produce a warning (unless optional) and nil.
func bindMount(expanded func(string) (string, bool), raw string, kind mountKind, plan *Plan) *mount {
	src, optional := expanded(raw)
	fi, err := os.Lstat(src)
	if err != nil {
		if !optional {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
		}
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
			return nil
		}
		return &mount{kind: kindSymlink, src: target, dest: src}
	}
	return &mount{kind: kind, src: src, dest: src, visible: true}
}

// covered reports whether path is exposed by an existing bind or overlay and
// not hidden by a deeper tmpfs.
func covered(mounts []*mount, path string) bool {
	best := -1
	visible := false
	for _, m := range mounts {
		if !under(path, m.dest) {
			continue
		}
		d := depth(m.dest)
		if d > best {
			best, visible = d, m.visible
		}
	}
	return visible
}

func under(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../"))
}

func depth(p string) int {
	p = filepath.Clean(p)
	if p == "/" {
		return 0
	}
	return strings.Count(p, "/")
}

func layerName(p string) string {
	s := strings.Trim(p, "/")
	s = strings.ReplaceAll(s, "/", "-")
	if s == "" {
		s = "root"
	}
	return s
}

func (m *mount) args() []string {
	switch m.kind {
	case kindRO:
		return []string{"--ro-bind", m.src, m.dest}
	case kindRW:
		return []string{"--bind", m.src, m.dest}
	case kindDev:
		return []string{"--dev-bind", m.src, m.dest}
	case kindTmpfs:
		return []string{"--tmpfs", m.dest}
	case kindSymlink:
		return []string{"--symlink", m.src, m.dest}
	case kindOverlay:
		if m.upper == "" {
			return []string{"--overlay-src", m.src, "--tmp-overlay", m.dest}
		}
		return []string{"--overlay-src", m.src, "--overlay", m.upper, m.work, m.dest}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		safe := strings.ContainsRune("-_/.,:=+@%", r) ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		return !safe
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
