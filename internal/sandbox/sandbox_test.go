package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"box/internal/config"
)

// fixture builds a fake host tree: home with a project dir, a tool dir to
// overlay, a merged-usr style symlink, and a binary outside all mounts.
func fixture(t *testing.T) (home, cwd, tools, bin string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	cwd = filepath.Join(home, "project")
	tools = filepath.Join(home, ".local", "share", "tools")
	bin = filepath.Join(root, "elsewhere", "bin")
	for _, d := range []string{cwd, tools, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	return home, cwd, tools, bin
}

func TestBuildOrdersAndExpands(t *testing.T) {
	home, cwd, tools, bin := fixture(t)
	root := filepath.Dir(home)
	state := filepath.Join(root, "state")

	prof := config.Profile{
		ROBinds:    []string{filepath.Join(root, "bin"), "?/does/not/exist", "/also/missing"},
		RWBinds:    []string{"$PWD"},
		Tmpfs:      []string{"$HOME"},
		Overlays:   []config.Overlay{{Path: "~/.local/share/tools", Persist: true}},
		Network:    true,
		Hostname:   "box",
		Env:        map[string]string{"B": "2", "A": "1"},
		UnsetEnv:   []string{"GONE"},
		BindBinary: true,
		ExtraArgs:  []string{"--cap-drop", "ALL"},
	}
	plan, err := buildBwrap(prof, Options{
		Binary:   "tool",
		Args:     []string{"--flag"},
		Cwd:      cwd,
		Home:     home,
		StateDir: state,
		Lookup:   func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "

	for _, want := range []string{
		" --unshare-all --share-net --hostname box ",
		" --symlink usr/bin " + filepath.Join(root, "bin") + " ",
		" --tmpfs " + home + " ",
		" --bind " + cwd + " " + cwd + " ",
		" --overlay-src " + tools + " --overlay " + filepath.Join(state, "overlays", "tool", layerName(tools), "upper") + " " +
			filepath.Join(state, "overlays", "tool", layerName(tools), "work") + " " + tools + " ",
		" --ro-bind " + filepath.Join(bin, "tool") + " " + filepath.Join(bin, "tool") + " ",
		" --chdir " + cwd + " ",
		" --setenv A 1 --setenv B 2 --unsetenv GONE --cap-drop ALL -- tool --flag ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in\n%s", want, cmd)
		}
	}
	if strings.Index(cmd, "--tmpfs "+home) > strings.Index(cmd, "--bind "+cwd) {
		t.Error("tmpfs on $HOME must be mounted before the bind underneath it")
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "/also/missing") {
		t.Errorf("want exactly one warning about /also/missing, got %v", plan.Warnings)
	}
	if len(plan.Dirs) != 2 {
		t.Errorf("want upper and work dirs, got %v", plan.Dirs)
	}
}

func TestBuildSkipsBinaryBindWhenCovered(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	prof := config.Profile{ROBinds: []string{filepath.Dir(bin)}, BindBinary: true}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(plan.Args, " "), "--ro-bind"); n != 1 {
		t.Errorf("binary should not be bound separately when its directory is mounted; got %d ro-binds", n)
	}
}

func TestBuildTmpOverlayAndNoNetwork(t *testing.T) {
	home, cwd, tools, bin := fixture(t)
	prof := config.Profile{
		Overlays: []config.Overlay{{Path: tools, Persist: false}},
	}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(plan.Args, " ")
	if strings.Contains(cmd, "--share-net") {
		t.Error("network should be disabled")
	}
	if !strings.Contains(cmd, "--overlay-src "+tools+" --tmp-overlay "+tools) {
		t.Errorf("expected tmp overlay in %s", cmd)
	}
}

func TestBuildPrivateTmp(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	root := filepath.Join(filepath.Dir(home), "tmp", "box")
	if err := os.Mkdir(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	prof := config.Profile{PrivateTmp: true, Tmpfs: []string{"/tmp"}, BindBinary: true}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home, TmpRoot: root, ID: "42",
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "tool-42")
	if plan.TmpDir != want {
		t.Errorf("TmpDir = %q, want %q", plan.TmpDir, want)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	if !strings.Contains(cmd, " --bind "+want+" /tmp ") {
		t.Errorf("missing private /tmp bind in\n%s", cmd)
	}
	if strings.Index(cmd, " --tmpfs /tmp ") > strings.Index(cmd, " --bind "+want) {
		t.Error("the private /tmp bind must come after a tmpfs on /tmp so it wins")
	}
	// The private /tmp hides host paths under /tmp, so a binary there (the
	// test fixture lives under the temp dir) still needs its own bind.
	if tool := filepath.Join(bin, "tool"); !strings.Contains(cmd, " --ro-bind "+tool+" "+tool+" ") {
		t.Errorf("binary under a bind of other content must be bound itself:\n%s", cmd)
	}

	// Exec creates the root shared like /tmp and the directory itself fresh,
	// clearing anything left behind by an earlier process with the same PID.
	if err := makeTmpDir(want); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode()&os.ModeSticky == 0 || fi.Mode().Perm() != 0o777 {
		t.Errorf("want sticky world-writable %s, got %v, %v", root, fi.Mode(), err)
	}
	if err := os.WriteFile(filepath.Join(want, "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := makeTmpDir(want); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(want); err != nil || len(entries) != 0 {
		t.Errorf("want empty %s, got %v, %v", want, entries, err)
	}

	// Default when private_tmp is off: no bind, no directory.
	plan, err = buildBwrap(config.Profile{}, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.TmpDir != "" || strings.Contains(" "+strings.Join(plan.Args, " ")+" ", " /tmp ") {
		t.Errorf("unexpected /tmp handling: %q %v", plan.TmpDir, plan.Args)
	}
}

func TestBuildCopyFiles(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	file := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	prof := config.Profile{
		Tmpfs:     []string{"$HOME"},
		CopyFiles: []string{"~/.claude.json", "?~/missing", "~/project"},
	}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 {
		t.Fatalf("Files = %v, want one descriptor", plan.Files)
	}
	var fd int
	for k := range plan.Files {
		fd = k
	}
	if plan.Files[fd] != file {
		t.Errorf("Files[%d] = %q, want %q", fd, plan.Files[fd], file)
	}
	// The descriptor must survive exec, so it is opened without CLOEXEC.
	if flags, err := unixFcntl(fd); err != nil || flags&syscall.FD_CLOEXEC != 0 {
		t.Errorf("descriptor %d must not be close-on-exec (flags %#x, %v)", fd, flags, err)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	want := " --perms 0600 --file " + strconv.Itoa(fd) + " " + file + " "
	if !strings.Contains(cmd, want) {
		t.Errorf("missing %q in\n%s", want, cmd)
	}
	if strings.Index(cmd, " --tmpfs "+home+" ") > strings.Index(cmd, want) {
		t.Error("the copy must be written after the tmpfs on $HOME is mounted")
	}
	if !strings.HasSuffix(plan.Command(), " "+strconv.Itoa(fd)+"<"+file) {
		t.Errorf("Command() should redirect the file onto the descriptor: %s", plan.Command())
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "not a regular file") {
		t.Errorf("want one warning about the directory, got %v", plan.Warnings)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}
}

func unixFcntl(fd int) (int, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(flags), nil
}

func TestBuildClearEnvPassthrough(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	t.Setenv("BOX_T_KEEP", "kept")
	t.Setenv("BOX_T_LC_ALL", "C")
	t.Setenv("BOX_T_LC_TIME", "de")
	t.Setenv("BOX_T_FIXED", "host")
	t.Setenv("BOX_T_GONE", "host")
	t.Setenv("BOX_T_OTHER", "host")
	prof := config.Profile{
		ClearEnv:      true,
		DisableUserns: true,
		NewSession:    true,
		PassEnv:       []string{"BOX_T_KEEP", "BOX_T_LC_*", "BOX_T_FIXED", "BOX_T_GONE", "BOX_T_MISSING"},
		Env:           map[string]string{"BOX_T_FIXED": "box"},
		UnsetEnv:      []string{"BOX_T_GONE"},
	}
	opts := Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	want := " --clearenv --setenv BOX_T_KEEP kept --setenv BOX_T_LC_ALL C --setenv BOX_T_LC_TIME de" +
		" --setenv BOX_T_FIXED box --unsetenv BOX_T_GONE "
	if !strings.Contains(cmd, want) {
		t.Errorf("missing %q in\n%s", want, cmd)
	}
	if strings.Contains(cmd, "BOX_T_OTHER") || strings.Contains(cmd, "BOX_T_MISSING") {
		t.Errorf("unselected variables leaked into %s", cmd)
	}
	if !strings.Contains(cmd, " --unshare-all --unshare-user --disable-userns ") || !strings.Contains(cmd, " --new-session ") {
		t.Errorf("missing --disable-userns or --new-session in %s", cmd)
	}

	// Without clear_env the host environment is inherited; nothing to pass.
	prof.ClearEnv = false
	plan, err = buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if cmd := strings.Join(plan.Args, " "); strings.Contains(cmd, "--setenv BOX_T_KEEP") || strings.Contains(cmd, "--clearenv") {
		t.Errorf("unexpected passthrough without clear_env: %s", cmd)
	}
}

func TestBuildProxy(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	self := filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(home), "tmp", "box")
	prof := config.Profile{Proxy: true, AllowHosts: []string{"api.anthropic.com", "*.github.com"}}
	opts := Options{
		Binary: "tool", Args: []string{"--flag"}, Cwd: cwd, Home: home, TmpRoot: root, ID: "7", Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "tool-7.sock")
	if plan.Proxy == nil || plan.Proxy.Socket != sock || plan.Proxy.Log != filepath.Join(root, "tool-7.log") ||
		len(plan.Proxy.Allow) != 2 {
		t.Fatalf("Proxy = %+v", plan.Proxy)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	for _, want := range []string{
		" --bind " + sock + " " + ProxySocket + " ",
		" --ro-bind " + self + " " + self + " ",
		" --setenv HTTPS_PROXY http://" + ProxyAddr + " ",
		" --setenv NO_PROXY localhost,127.0.0.1,::1 ",
		" -- " + self + " _forward -s " + ProxySocket + " -l " + ProxyAddr + " -- tool --flag ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--share-net") {
		t.Error("network must not be shared when going through the proxy")
	}
	want := self + " _proxy -s " + sock + " -l " + filepath.Join(root, "tool-7.log") + " -a api.anthropic.com -a '*.github.com'"
	if got := plan.ProxyCommand(); got != want {
		t.Errorf("ProxyCommand() = %s, want %s", got, want)
	}

	// Sharing the host network makes the proxy pointless; none is set up.
	prof.Network = true
	plan, err = buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Proxy != nil || plan.ProxyCommand() != "" || strings.Contains(strings.Join(plan.Args, " "), "_forward") {
		t.Errorf("unexpected proxy with network = true: %v", plan.Args)
	}
}

func TestBuildUnknownCommand(t *testing.T) {
	_, err := buildBwrap(config.Profile{}, Options{
		Binary: "nope", Cwd: "/", Home: "/",
		Lookup: func(string) (string, error) { return "", os.ErrNotExist },
	}, "bwrap")
	if err == nil {
		t.Error("expected error for unknown command")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "plain",
		"/a/b-c.d":  "/a/b-c.d",
		"has space": "'has space'",
		"it's":      `'it'\''s'`,
		"":          "''",
		"$HOME":     "'$HOME'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// On macOS t.TempDir lives under /var, a symlink to /private/var. The binary
// resolves to the real location while the directory bind keeps the given
// path; the two must still be recognised as the same mount.
func TestBuildSkipsBinaryBindUnderSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "private", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private", filepath.Join(root, "var")); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "var", "bin")
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	prof := config.Profile{ROBinds: []string{bin}, BindBinary: true}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(plan.Args, " "), "--ro-bind"); n != 1 {
		t.Errorf("binary should not be bound separately when its directory is mounted; got %d ro-binds", n)
	}
}

func TestBuildSyncFiles(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	self := filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	prof := config.Profile{
		Tmpfs:     []string{"$HOME"},
		SyncFiles: []string{"~/.claude.json", "?~/missing"},
	}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Args: []string{"--flag"}, Cwd: cwd, Home: home, Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || len(plan.Sync) != 1 {
		t.Fatalf("Files = %v, Sync = %v, want one each", plan.Files, plan.Sync)
	}
	var fd int
	for k := range plan.Files {
		fd = k
	}
	defer func() { _ = syscall.Close(fd) }()
	mountPoint := SyncDir + "/0"
	if plan.Sync[0] != (SyncFile{Path: file, Mount: mountPoint}) {
		t.Errorf("Sync[0] = %+v", plan.Sync[0])
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	for _, want := range []string{
		" --perms 0600 --file " + strconv.Itoa(fd) + " " + file + " ",
		" --bind " + file + " " + mountPoint + " ",
		" --ro-bind " + self + " " + self + " ",
		" -- " + self + " _forward -w " + file + "=" + mountPoint + " -- tool --flag ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, " -s ") || plan.Proxy != nil {
		t.Errorf("no proxy was requested: %s", cmd)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("unexpected warnings %v", plan.Warnings)
	}
}

func TestSyncerWriteBack(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host.json")
	copy := filepath.Join(dir, "copy.json")
	for _, p := range []string{host, copy} {
		if err := os.WriteFile(p, []byte(`{"a":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hostInode := func() uint64 {
		fi, err := os.Stat(host)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Ino
	}
	before := hostInode()
	s := NewSyncer([]SyncFile{{Path: copy, Mount: host}})

	// Identical content: nothing to do, the host file is not touched.
	if warnings, err := s.WriteBack(); err != nil || len(warnings) != 0 {
		t.Fatalf("WriteBack() = %v, %v", warnings, err)
	}

	// The program replaced its copy (new inode); the host gets the content
	// in place.
	tmp := copy + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, copy); err != nil {
		t.Fatal(err)
	}
	warnings, err := s.WriteBack()
	if err != nil || len(warnings) != 0 {
		t.Fatalf("WriteBack() = %v, %v", warnings, err)
	}
	if got, _ := os.ReadFile(host); string(got) != `{"a":2}` {
		t.Errorf("host = %s, want the copy's content", got)
	}
	if hostInode() != before {
		t.Error("host file must be rewritten in place")
	}
	if fi, _ := os.Stat(host); fi.Mode().Perm() != 0o600 {
		t.Errorf("host mode = %v, want 0600", fi.Mode().Perm())
	}

	// A host file edited meanwhile is overwritten with a warning.
	if err := os.WriteFile(host, []byte(`{"other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copy, []byte(`{"a":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings, err = s.WriteBack()
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "changed on the host") {
		t.Fatalf("WriteBack() = %v, %v, want one warning", warnings, err)
	}
	if got, _ := os.ReadFile(host); string(got) != `{"a":3}` {
		t.Errorf("host = %s, want the copy's content", got)
	}

	// A copy removed inside leaves the host alone.
	if err := os.Remove(copy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteBack(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(host); string(got) != `{"a":3}` {
		t.Errorf("host = %s after copy removal", got)
	}
}

func TestParseSyncFile(t *testing.T) {
	sf, err := ParseSyncFile("/home/u/.claude.json=/run/box/sync/0")
	if err != nil || sf.Path != "/home/u/.claude.json" || sf.Mount != "/run/box/sync/0" {
		t.Errorf("ParseSyncFile = %+v, %v", sf, err)
	}
	if sf.Arg() != "/home/u/.claude.json=/run/box/sync/0" {
		t.Errorf("Arg() = %q", sf.Arg())
	}
	for _, bad := range []string{"", "nope", "=/x", "/x="} {
		if _, err := ParseSyncFile(bad); err == nil {
			t.Errorf("ParseSyncFile(%q) should fail", bad)
		}
	}
}

func TestLookupRunner(t *testing.T) {
	r := LookupRunner()
	want := "bwrap"
	if runtime.GOOS == "darwin" {
		want = "sandbox-exec"
	}
	if r.Name != want || r.Mechanism == "" {
		t.Fatalf("LookupRunner() = %+v, want name %q with a mechanism", r, want)
	}
	if _, err := exec.LookPath(want); err != nil {
		if r.Exe != "" {
			t.Fatalf("Exe = %q, want empty when %s is not installed", r.Exe, want)
		}
		return
	}
	if r.Exe == "" {
		t.Fatalf("Exe is empty although %s is installed", want)
	}
	if want == "bwrap" && r.Version == "" {
		t.Fatalf("Version is empty for %s", r.Exe)
	}
}
