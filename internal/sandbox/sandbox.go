// Package sandbox turns a resolved profile into a sandbox command line:
// bubblewrap (bwrap) on Linux, sandbox-exec with a generated Seatbelt profile
// on macOS.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
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
	// TmpRoot holds the private /tmp directories; defaults to DefaultTmpRoot.
	TmpRoot string
	// ID identifies this sandbox in TmpRoot; defaults to the current PID,
	// which bwrap keeps because box replaces itself with it.
	ID string
	// Lookup resolves the command on the host. Defaults to exec.LookPath.
	Lookup func(string) (string, error)
	// Self is the box executable, run as the proxy on the host and as the
	// forwarder inside the sandbox. Defaults to os.Executable().
	Self string
}

// ProxyAddr is where the forwarder listens inside the sandbox, and
// ProxySocket is where the host proxy's unix socket is bound there.
const (
	ProxyAddr   = "127.0.0.1:3128"
	ProxySocket = "/run/box/proxy.sock"
)

// Proxy describes the allowlisting proxy a sandbox reaches the network
// through (see package proxy).
type Proxy struct {
	Socket string   // unix socket on the host, bound into the sandbox
	Log    string   // host file the proxy logs denied requests to
	Allow  []string // allowed destination hosts
}

// ProxyFiles returns the socket and log paths for sandbox name/id: the
// socket lives under root, next to the private /tmp directory, the log under
// logDir. The log name also carries the sandbox's working directory, without
// its leading slash and with every other slash replaced by a dash, so logs of
// the same binary started from different places can be told apart:
// <logDir>/<name>-<id>-home-user-project.log.
func ProxyFiles(root, logDir, name, id, cwd string) (socket, logFile string) {
	sep := string(filepath.Separator)
	dir := strings.ReplaceAll(strings.TrimPrefix(filepath.Clean(cwd), sep), sep, "-")
	return TmpDir(root, name, id) + ".sock", filepath.Join(logDir, name+"-"+id+"-"+dir+".log")
}

// LogDir returns the directory prof's proxy logs are written to: log_dir
// expanded like any other configured path, or opts.TmpRoot when it is empty.
func LogDir(prof config.Profile, opts Options) (string, error) {
	if err := fillDefaults(&opts); err != nil {
		return "", err
	}
	return logDir(prof, opts), nil
}

func logDir(prof config.Profile, opts Options) string {
	if dir, _ := expander(opts)(prof.LogDir); dir != "" {
		return dir
	}
	return opts.TmpRoot
}

// DefaultTmpRoot is the host directory under which private /tmp directories
// are created, one per sandbox (see TmpDir).
const DefaultTmpRoot = "/tmp/box"

// TmpDir returns the host directory mounted as /tmp inside the sandbox for
// binary name with the given id: <root>/<name>-<id>.
func TmpDir(root, name, id string) string {
	return filepath.Join(root, name+"-"+id)
}

type mountKind int

const (
	kindRO mountKind = iota
	kindRW
	kindDev
	kindTmpfs
	kindSymlink
	kindOverlay
	kindFile // a copy of a host file, passed on an inherited descriptor
)

type mount struct {
	kind    mountKind
	src     string // host path (bind, overlay lower) or link target (symlink)
	dest    string
	upper   string // overlay only; empty means a temporary overlay
	work    string
	fd      int // file only: descriptor bwrap copies from
	mode    os.FileMode
	order   int
	visible bool // true for binds and overlays: dest exposes host content
}

// ProcTitlePrefix starts argv[0] of every bwrap process started by Exec,
// followed by the sandboxed binary's base name. It lets `box ps` tell box
// sandboxes apart from other bwrap users.
const ProcTitlePrefix = "box:"

// NameEnv and DirEnv are set inside macOS sandboxes to the binary's base name
// and the working directory. Seatbelt leaves no wrapper process behind, so
// `box ps` finds sandboxes by these variables instead of ProcTitlePrefix.
const (
	NameEnv = "BOX_NAME"
	DirEnv  = "BOX_DIR"
)

// Plan is a fully computed sandbox invocation.
type Plan struct {
	Name     string   // base name of the sandboxed binary
	Exe      string   // path to the sandbox runner (bwrap or sandbox-exec)
	Args     []string // runner arguments, without argv[0]
	Warnings []string
	// Dirs lists directories that must exist before running (overlay layers).
	Dirs []string
	// TmpDir is the host directory mounted as /tmp (private_tmp); it is
	// created fresh before running.
	TmpDir string
	// Files lists host files held open on descriptors the runner inherits
	// (copy_files), by descriptor number.
	Files map[int]string
	// Proxy, when set, is started on the host before the runner.
	Proxy *Proxy
	// Sync lists copies (sync_files) written back to their host originals
	// by the forwarder when the command exits.
	Sync []SyncFile
	// Self is the box executable (Options.Self).
	Self string
	// Dir, ClearEnv, UnsetEnv and Env set up the runner's own working
	// directory and environment, for runners that cannot do it themselves.
	// Env holds KEY=VALUE pairs.
	Dir      string
	ClearEnv bool
	UnsetEnv []string
	Env      []string
}

// Command returns the invocation as a shell-quoted string.
func (p *Plan) Command() string {
	var parts []string
	if p.ClearEnv || len(p.UnsetEnv) > 0 || len(p.Env) > 0 {
		parts = append(parts, "env")
		if p.ClearEnv {
			parts = append(parts, "-i")
		}
		for _, k := range p.UnsetEnv {
			parts = append(parts, "-u", shellQuote(k))
		}
		for _, kv := range p.Env {
			parts = append(parts, shellQuote(kv))
		}
	}
	parts = append(parts, shellQuote(p.Exe))
	for _, a := range p.Args {
		parts = append(parts, shellQuote(a))
	}
	fds := make([]int, 0, len(p.Files))
	for fd := range p.Files {
		fds = append(fds, fd)
	}
	sort.Ints(fds)
	for _, fd := range fds {
		parts = append(parts, strconv.Itoa(fd)+"<"+shellQuote(p.Files[fd]))
	}
	return strings.Join(parts, " ")
}

// ProxyCommand returns the host-side proxy invocation as a shell-quoted
// string, or "" when the plan has no proxy.
func (p *Plan) ProxyCommand() string {
	if p.Proxy == nil {
		return ""
	}
	parts := []string{shellQuote(p.Self)}
	for _, a := range p.proxyArgs() {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func (p *Plan) proxyArgs() []string {
	args := []string{"_proxy", "-s", p.Proxy.Socket, "-l", p.Proxy.Log}
	for _, h := range p.Proxy.Allow {
		args = append(args, "-a", h)
	}
	return args
}

// Exec creates any required directories, starts the proxy if there is one
// and replaces the current process with the runner, tagging argv[0] with
// ProcTitlePrefix. It only returns on error.
func (p *Plan) Exec() error {
	// The proxy child gets a parent-death signal, which Linux ties to the
	// thread that forked it; keep this goroutine on one thread so the same
	// thread goes on to exec the runner and lives as long as the sandbox.
	runtime.LockOSThread()
	for _, d := range p.Dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if p.TmpDir != "" {
		if err := makeTmpDir(p.TmpDir); err != nil {
			return err
		}
	}
	if p.Dir != "" {
		if err := os.Chdir(p.Dir); err != nil {
			return err
		}
	}
	proxy, err := p.startProxy()
	if err != nil {
		return err
	}
	argv := append([]string{ProcTitlePrefix + p.Name}, p.Args...)
	err = syscall.Exec(p.Exe, argv, p.environ())
	if proxy != nil {
		_ = proxy.Kill()
	}
	return err
}

// startProxy runs `box _proxy` for p.Proxy and waits until it listens. The
// child is told to die with this process (which becomes the runner).
func (p *Plan) startProxy() (*os.Process, error) {
	if p.Proxy == nil {
		return nil, nil
	}
	if err := makeTmpRoot(filepath.Dir(p.Proxy.Socket)); err != nil {
		return nil, err
	}
	if dir := filepath.Dir(p.Proxy.Log); dir != filepath.Dir(p.Proxy.Socket) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.Remove(p.Proxy.Socket); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(p.Self, p.proxyArgs()...)
	cmd.ExtraFiles = []*os.File{w} // proxy.ReadyFD
	cmd.SysProcAttr = dieWithParent()
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("starting proxy: %w", err)
	}
	_ = w.Close()
	if n, _ := r.Read(make([]byte, 1)); n == 0 {
		_ = cmd.Wait()
		return nil, fmt.Errorf("proxy failed to start; see %s", p.Proxy.Log)
	}
	// Leave the child unwaited: this process is about to exec.
	go func() { _ = cmd.Wait() }()
	return cmd.Process, nil
}

// makeTmpRoot creates the directory holding private /tmp directories and
// proxy sockets. It is shared by all users like /tmp itself, so it is
// created world-writable with the sticky bit and must be a real directory
// that is either ours or sticky.
func makeTmpRoot(root string) error {
	if err := os.Mkdir(root, 0o777); err == nil {
		if err := os.Chmod(root, 0o777|os.ModeSticky); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() && fi.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s is owned by another user and not sticky", root)
	}
	return nil
}

// makeTmpDir creates the private /tmp directory fresh, clearing anything
// left behind by an earlier process with the same PID.
func makeTmpDir(dir string) error {
	if err := makeTmpRoot(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Mkdir(dir, 0o700)
}

func (p *Plan) environ() []string {
	var env []string
	if !p.ClearEnv {
		env = os.Environ()
	}
	drop := map[string]bool{}
	for _, k := range p.UnsetEnv {
		drop[k] = true
	}
	for _, kv := range p.Env {
		k, _, _ := strings.Cut(kv, "=")
		drop[k] = true
	}
	out := make([]string, 0, len(env)+len(p.Env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, p.Env...)
}

// Runner describes the isolation mechanism box uses on this host.
type Runner struct {
	Name      string // executable name: bwrap or sandbox-exec
	Mechanism string // human-readable name: bubblewrap or Seatbelt
	Exe       string // resolved path; empty when not installed
	Version   string // as reported by the runner; empty when unknown
}

// LookupRunner finds the runner for this platform: sandbox-exec (Seatbelt)
// on macOS, bwrap (bubblewrap) everywhere else. A missing executable is not
// an error; Exe is left empty.
func LookupRunner() Runner {
	r := Runner{Name: "bwrap", Mechanism: "bubblewrap"}
	if runtime.GOOS == "darwin" {
		r = Runner{Name: "sandbox-exec", Mechanism: "Seatbelt"}
	}
	exe, err := exec.LookPath(r.Name)
	if err != nil {
		return r
	}
	r.Exe = exe
	if r.Name == "bwrap" {
		// Prints "bubblewrap 0.11.1".
		if out, err := exec.Command(exe, "--version").Output(); err == nil {
			if f := strings.Fields(string(out)); len(f) == 2 {
				r.Version = f[1]
			}
		}
	}
	return r
}

// Build computes the sandbox invocation for the given profile and options:
// sandbox-exec on macOS, bwrap everywhere else.
func Build(prof config.Profile, opts Options) (*Plan, error) {
	r := LookupRunner()
	switch {
	case r.Exe != "" && r.Name == "sandbox-exec":
		return buildSeatbelt(prof, opts, r.Exe)
	case r.Exe != "":
		return buildBwrap(prof, opts, r.Exe)
	case r.Name == "sandbox-exec":
		return nil, fmt.Errorf("sandbox-exec not found in PATH (expected /usr/bin/sandbox-exec)")
	default:
		return nil, fmt.Errorf("bwrap not found in PATH: install bubblewrap")
	}
}

// collect resolves the profile's mounts into a parents-first list. With
// resolveLinks (Seatbelt, which matches real paths) host symlinks are followed
// and overlays degrade to read-only; otherwise symlinks are recreated and
// overlays get their layer directories.
func collect(prof config.Profile, opts *Options, plan *Plan, resolveLinks bool) ([]*mount, error) {
	if opts.Binary == "" {
		return nil, fmt.Errorf("no command given")
	}
	if err := fillDefaults(opts); err != nil {
		return nil, err
	}
	exp := expander(*opts)
	var mounts []*mount
	add := func(m *mount) {
		m.dest = filepath.Clean(m.dest)
		for _, o := range mounts {
			if o.kind == m.kind && o.dest == m.dest && o.src == m.src {
				return // e.g. /tmp and $TMPDIR resolving to the same path
			}
		}
		m.order = len(mounts)
		mounts = append(mounts, m)
	}
	bind := func(raw string, kind mountKind) {
		if m := bindMount(exp, raw, kind, plan, resolveLinks); m != nil {
			add(m)
		}
	}

	for _, p := range prof.ROBinds {
		bind(p, kindRO)
	}
	for _, p := range prof.RWBinds {
		bind(p, kindRW)
	}
	for _, p := range prof.DevBinds {
		bind(p, kindDev)
	}
	for _, p := range prof.Tmpfs {
		dest, _ := exp(p)
		if dest == "" {
			continue
		}
		if resolveLinks {
			dest = realPath(dest)
		}
		add(&mount{kind: kindTmpfs, dest: dest})
	}
	if prof.PrivateTmp {
		if resolveLinks {
			plan.Warnings = append(plan.Warnings, "private_tmp is not supported on this platform; ignored")
		} else {
			plan.TmpDir = TmpDir(opts.TmpRoot, plan.Name, opts.ID)
			add(&mount{kind: kindRW, src: plan.TmpDir, dest: "/tmp", visible: true})
		}
	}
	for _, p := range prof.CopyFiles {
		if resolveLinks {
			plan.Warnings = append(plan.Warnings, "copy_files is not supported on this platform; ignored")
			break
		}
		if m := copyFile(exp, p, plan); m != nil {
			add(m)
		}
	}
	for _, p := range prof.SyncFiles {
		if resolveLinks {
			plan.Warnings = append(plan.Warnings, "sync_files is not supported on this platform; ignored")
			break
		}
		m := copyFile(exp, p, plan)
		if m == nil {
			continue
		}
		// The original is bound where the forwarder can rewrite it in
		// place; the copy at the real path may be replaced freely.
		sf := SyncFile{Path: m.dest, Mount: filepath.Join(SyncDir, strconv.Itoa(len(plan.Sync)))}
		plan.Sync = append(plan.Sync, sf)
		plan.Self = opts.Self
		add(m)
		add(&mount{kind: kindRW, src: m.src, dest: sf.Mount})
	}
	if prof.Proxy && !prof.Network {
		if resolveLinks {
			plan.Warnings = append(plan.Warnings, "proxy is not supported on this platform; ignored (no network)")
		} else {
			sock, logFile := ProxyFiles(opts.TmpRoot, logDir(prof, *opts), plan.Name, opts.ID, opts.Cwd)
			plan.Proxy = &Proxy{Socket: sock, Log: logFile, Allow: prof.AllowHosts}
			plan.Self = opts.Self
			// The socket is created by Exec, so it cannot be checked here.
			add(&mount{kind: kindRW, src: sock, dest: ProxySocket, visible: true})
		}
	}
	for _, o := range prof.Overlays {
		src, optional := exp(o.Path)
		if src == "" {
			continue
		}
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
		if resolveLinks {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("overlay %s is read-only: overlays are not supported on this platform", src))
			src = realPath(src)
			add(&mount{kind: kindRO, src: src, dest: src, visible: true})
			continue
		}
		m := &mount{kind: kindOverlay, src: src, dest: src, visible: true}
		if o.Persist {
			if opts.StateDir == "" {
				return nil, fmt.Errorf("persistent overlay %s requires a state directory", src)
			}
			layer := filepath.Join(opts.StateDir, "overlays", plan.Name, layerName(src))
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
	var expose []string
	if prof.BindBinary {
		expose = append(expose, resolved)
	}
	if plan.Proxy != nil || len(plan.Sync) > 0 {
		expose = append(expose, opts.Self) // runs the forwarder inside
	}
	for _, e := range expose {
		candidates := []string{e}
		if real, err := filepath.EvalSymlinks(e); err == nil && real != e {
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
	return mounts, nil
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
	if o.TmpRoot == "" {
		o.TmpRoot = DefaultTmpRoot
	}
	if o.ID == "" {
		o.ID = strconv.Itoa(os.Getpid())
	}
	if o.Self == "" {
		if o.Self, err = os.Executable(); err != nil {
			return err
		}
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
		if p = os.Expand(p, mapping); p == "" {
			return "", optional // e.g. an unset variable
		}
		return filepath.Clean(p), optional
	}
}

// bindMount describes how to expose one host path. Symlinks on the host are
// either resolved (resolveLinks) or recreated as symlinks (merged-/usr
// systems link /bin -> usr/bin); missing paths produce a warning (unless
// optional) and nil.
func bindMount(expanded func(string) (string, bool), raw string, kind mountKind, plan *Plan, resolveLinks bool) *mount {
	src, optional := expanded(raw)
	if src == "" {
		return nil
	}
	fi, err := os.Lstat(src)
	if err != nil {
		if !optional {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
		}
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if resolveLinks {
			real, err := filepath.EvalSymlinks(src)
			if err != nil {
				if !optional {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
				}
				return nil
			}
			return &mount{kind: kind, src: real, dest: real, visible: true}
		}
		target, err := os.Readlink(src)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
			return nil
		}
		return &mount{kind: kindSymlink, src: target, dest: src}
	}
	if resolveLinks {
		// A parent directory may still be a symlink (/var -> private/var).
		src = realPath(src)
	}
	return &mount{kind: kind, src: src, dest: src, visible: true}
}

// copyFile opens a host file for bwrap's --file, which copies it into the
// sandbox at the same path. The descriptor is deliberately left without
// close-on-exec so that bwrap inherits it when Exec replaces this process.
func copyFile(expanded func(string) (string, bool), raw string, plan *Plan) *mount {
	src, optional := expanded(raw)
	if src == "" {
		return nil
	}
	fi, err := os.Stat(src)
	if err != nil {
		if !optional {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
		}
		return nil
	}
	if !fi.Mode().IsRegular() {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: not a regular file", src))
		return nil
	}
	fd, err := syscall.Open(src, syscall.O_RDONLY, 0)
	if err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s skipped: %v", src, err))
		return nil
	}
	if plan.Files == nil {
		plan.Files = map[int]string{}
	}
	plan.Files[fd] = src
	return &mount{kind: kindFile, src: src, dest: src, fd: fd, mode: fi.Mode().Perm(), visible: true}
}

// passEnv returns the host variables selected by prof.PassEnv as KEY=VALUE
// pairs sorted by name. Entries are names or path.Match patterns (LC_*).
// Variables that env sets or unset_env removes are left out, so those
// settings win.
func passEnv(prof config.Profile) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, fixed := prof.Env[k]; fixed || slices.Contains(prof.UnsetEnv, k) {
			continue
		}
		for _, pat := range prof.PassEnv {
			if ok, _ := path.Match(pat, k); ok {
				out = append(out, kv)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// realPath resolves symlinks in p, returning p unchanged when that fails.
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// covered reports whether the host path is visible at the same path inside
// the sandbox: exposed by a bind or overlay of the place it lives in, and
// not hidden by a deeper tmpfs or by a bind of other content (the private
// /tmp). A visible mount also covers paths under the real location of its
// source, so a binary resolved through a symlinked parent
// (/var -> /private/var) is not bound a second time.
func covered(mounts []*mount, path string) bool {
	best := -1
	visible := false
	for _, m := range mounts {
		byDest := under(path, m.dest)
		bySrc := m.visible && m.src != "" && under(path, realPath(m.src))
		if !byDest && !bySrc {
			continue
		}
		if d := depth(m.dest); d > best {
			best = d
			visible = m.visible && (bySrc || m.src == m.dest)
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
