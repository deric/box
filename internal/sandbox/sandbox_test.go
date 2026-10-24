package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deric/box/internal/config"
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

// A persistent overlay inside a temporary one (the claude profile's
// ~/.claude/skills under ~/.claude) must be mounted after its parent so the
// tmp overlay does not hide it, and must get its own layer directories.
func TestBuildNestedOverlays(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	state := filepath.Join(filepath.Dir(home), "state")
	parent := filepath.Join(home, ".claude")
	child := filepath.Join(parent, "skills")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	prof := config.Profile{
		Tmpfs: []string{"$HOME"},
		Overlays: []config.Overlay{
			{Path: "?~/.claude/skills", Persist: true}, // listed first on purpose
			{Path: "?~/.claude", Persist: false},
			{Path: "?~/.agents/skills", Persist: true}, // missing: skipped silently
		},
	}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home, StateDir: state,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(plan.Args, " ")
	layer := filepath.Join(state, "overlays", "tool", layerName(child))
	tmp := "--overlay-src " + parent + " --tmp-overlay " + parent
	persist := "--overlay-src " + child + " --overlay " + filepath.Join(layer, "upper") + " " + filepath.Join(layer, "work") + " " + child
	ti, pi := strings.Index(cmd, tmp), strings.Index(cmd, persist)
	if ti < 0 || pi < 0 {
		t.Fatalf("expected both overlays in %s", cmd)
	}
	if hi := strings.Index(cmd, "--tmpfs "+home); hi >= ti || ti >= pi {
		t.Errorf("want $HOME tmpfs, then %s, then %s in %s", parent, child, cmd)
	}
	if strings.Contains(cmd, ".agents") {
		t.Errorf("missing optional overlay should be skipped: %s", cmd)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", plan.Warnings)
	}
	for _, d := range []string{filepath.Join(layer, "upper"), filepath.Join(layer, "work")} {
		if !slices.Contains(plan.Dirs, d) {
			t.Errorf("plan.Dirs = %v, missing %s", plan.Dirs, d)
		}
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
	logFile := filepath.Join(root, "tool-7-"+strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")+".log")
	if plan.Proxy == nil || plan.Proxy.Socket != sock || plan.Proxy.Log != logFile ||
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
	want := self + " _proxy -s " + sock + " -l " + logFile + " -a api.anthropic.com -a '*.github.com'"
	if got := plan.ProxyCommand(); got != want {
		t.Errorf("ProxyCommand() = %s, want %s", got, want)
	}

	// log_dir moves the log, not the socket; ~ and variables are expanded.
	prof.LogDir = "~/logs"
	plan, err = buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "logs", filepath.Base(logFile)); plan.Proxy.Log != want || plan.Proxy.Socket != sock {
		t.Errorf("with log_dir: Log = %s, Socket = %s; want %s, %s", plan.Proxy.Log, plan.Proxy.Socket, want, sock)
	}
	prof.LogDir = ""

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
	saveDir := filepath.Join(DefaultTmpRoot, "sync")
	if plan.SyncSave != saveDir || !slices.Contains(plan.Dirs, saveDir) {
		t.Errorf("SyncSave = %q, Dirs = %v, want %s created", plan.SyncSave, plan.Dirs, saveDir)
	}
	for _, want := range []string{
		" --perms 0600 --file " + strconv.Itoa(fd) + " " + file + " ",
		" --bind " + file + " " + mountPoint + " ",
		" --bind " + saveDir + " " + SaveDir + " ",
		" --ro-bind " + self + " " + self + " ",
		" -- " + self + " _forward -W " + saveDir + " -w " + file + "=" + mountPoint + " -- tool --flag ",
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
	if conflicts, err := s.WriteBack(); err != nil || len(conflicts) != 0 {
		t.Fatalf("WriteBack() = %v, %v", conflicts, err)
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
	conflicts, err := s.WriteBack()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("WriteBack() = %v, %v", conflicts, err)
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

	// A host file edited meanwhile is left alone and reported as a
	// conflict carrying both versions.
	if err := os.WriteFile(host, []byte(`{"other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copy, []byte(`{"a":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	conflicts, err = s.WriteBack()
	if err != nil || len(conflicts) != 1 {
		t.Fatalf("WriteBack() = %v, %v, want one conflict", conflicts, err)
	}
	c := conflicts[0]
	if c.File.Path != copy || string(c.Host) != `{"other":true}` || string(c.Copy) != `{"a":3}` {
		t.Errorf("conflict = %+v", c)
	}
	if got, _ := os.ReadFile(host); string(got) != `{"other":true}` {
		t.Errorf("host = %s, must be left alone on conflict", got)
	}

	// Saving keeps the host file and puts the copy aside under a dated name.
	saveDir := filepath.Join(dir, "saved")
	if err := os.Mkdir(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 15, 30, 0, 0, time.UTC)
	name, err := c.Save(saveDir, at)
	if err != nil || name != "copy.json.20261019-153000" {
		t.Fatalf("Save() = %q, %v", name, err)
	}
	if got, _ := os.ReadFile(filepath.Join(saveDir, name)); string(got) != `{"a":3}` {
		t.Errorf("saved = %s", got)
	}
	if got, _ := os.ReadFile(host); string(got) != `{"other":true}` {
		t.Errorf("host = %s after Save", got)
	}
	if _, err := c.Save(filepath.Join(dir, "missing"), at); err == nil {
		t.Error("Save into a missing directory should fail")
	}

	// Overwriting writes the copy over the host file, in place.
	if err := c.Overwrite(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(host); string(got) != `{"a":3}` {
		t.Errorf("host = %s after Overwrite", got)
	}
	if hostInode() != before {
		t.Error("Overwrite must rewrite in place")
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
	lookup := want
	if want == "sandbox-exec" {
		lookup = SandboxExecPath
	}
	if _, err := exec.LookPath(lookup); err != nil {
		if r.Exe != "" {
			t.Fatalf("Exe = %q, want empty when %s is not installed", r.Exe, want)
		}
		return
	}
	if r.Exe == "" {
		t.Fatalf("Exe is empty although %s is installed", want)
	}
	if want == "sandbox-exec" && r.Exe != SandboxExecPath {
		t.Fatalf("Exe = %q, want the fixed path %s", r.Exe, SandboxExecPath)
	}
	if want == "bwrap" && r.Version == "" {
		t.Fatalf("Version is empty for %s", r.Exe)
	}
}

func TestBuildPodman(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap plans are built on linux only")
	}
	home, cwd, _, bin := fixture(t)
	self := filepath.Join(filepath.Dir(bin), "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(filepath.Dir(home), "state")
	prof := config.Profile{
		ROBinds: []string{"/etc"},
		Tmpfs:   []string{"$HOME"},
		Podman:  true,
		Env:     map[string]string{"CONTAINERS_STORAGE_CONF": "/custom/storage.conf"},
	}
	plan, err := buildBwrap(prof, Options{
		Binary: "tool", Args: []string{"--flag"}, Cwd: cwd, Home: home, StateDir: state, Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "podman", "tool")
	if !slices.Contains(plan.Dirs, dir) {
		t.Errorf("Dirs = %v, want the podman state dir %s", plan.Dirs, dir)
	}
	runtimeDir := RuntimeDir(os.Getuid())
	pause := PausePidFile(os.Getuid())
	if plan.Pause != pause || !strings.HasPrefix(pause, runtimeDir+"/") {
		t.Errorf("Pause = %q, want %q under %s", plan.Pause, pause, runtimeDir)
	}
	cmd := " " + strings.Join(plan.Args, " ") + " "
	for _, want := range []string{
		" --bind " + dir + " " + dir + " ",
		" --ro-bind /sys /sys ",
		" --perms 0700 --tmpfs " + runtimeDir + " ",
		" --setenv XDG_RUNTIME_DIR " + runtimeDir + " ",
		" --setenv CONTAINERS_CONF_OVERRIDE " + dir + "/containers.conf ",
		" --setenv CONTAINERS_STORAGE_CONF /custom/storage.conf ",
		// The forwarder creates podman's pause process inside.
		" --ro-bind " + self + " " + self + " ",
		" -- " + self + " _forward -p " + pause + " -- tool --flag ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--disable-userns") {
		t.Error("podman needs user namespaces; --disable-userns must not be passed")
	}
	if strings.Count(cmd, " --setenv CONTAINERS_STORAGE_CONF ") != 1 {
		t.Errorf("an explicit env entry must replace the generated one:\n%s", cmd)
	}
	if strings.Index(cmd, " --tmpfs "+home+" ") > strings.Index(cmd, " --bind "+dir+" ") {
		t.Error("the state dir must be bound after the tmpfs on $HOME is mounted")
	}
	if strings.Contains(cmd, " --file ") || len(plan.Files) != 0 {
		t.Errorf("generated files must be bound from the host, not copied in:\n%s", cmd)
	}

	// The subordinate ID files are hidden behind an empty host file, which
	// must be bound after /etc.
	subid := filepath.Join(dir, "subid")
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		bind := " --ro-bind " + subid + " " + f + " "
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			if !strings.Contains(cmd, bind) {
				t.Errorf("missing %q in\n%s", bind, cmd)
			} else if strings.Index(cmd, " --ro-bind /etc /etc ") > strings.Index(cmd, bind) {
				t.Errorf("%s must be bound after /etc", f)
			}
		} else if strings.Contains(cmd, bind) {
			t.Errorf("%s does not exist on the host and cannot be bound over", f)
		}
	}

	// The generated files are written before running.
	want := []string{filepath.Join(dir, "containers.conf"), filepath.Join(dir, "storage.conf"), subid}
	if got := plan.WrittenFiles(); !slices.Equal(got, want) {
		t.Errorf("WrittenFiles() = %v, want %v", got, want)
	}
	if plan.Writes[subid] != "" {
		t.Errorf("subid must be empty, got %q", plan.Writes[subid])
	}
	store := plan.Writes[filepath.Join(dir, "storage.conf")]
	if !strings.Contains(store, "graphroot = "+strconv.Quote(filepath.Join(dir, "storage"))) ||
		!strings.Contains(store, `ignore_chown_errors = "true"`) {
		t.Errorf("storage.conf:\n%s", store)
	}
	conf := plan.Writes[filepath.Join(dir, "containers.conf")]
	for _, want := range []string{`netns = "host"`, `cgroups = "disabled"`, `cgroup_manager = "cgroupfs"`} {
		if !strings.Contains(conf, want) {
			t.Errorf("containers.conf is missing %q:\n%s", want, conf)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subid, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := plan.writeFiles(); err != nil {
		t.Fatal(err)
	}
	for path, content := range plan.Writes {
		if data, err := os.ReadFile(path); err != nil || string(data) != content {
			t.Errorf("%s: %q, %v; want %q", path, data, err, content)
		}
	}

	if _, err := buildBwrap(prof, Options{
		Binary: "tool", Cwd: cwd, Home: home, Self: self,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "bwrap"); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Errorf("without a state directory, want an error, got %v", err)
	}
}

func TestStartPause(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the pause process is started on linux only")
	}
	root := t.TempDir()
	// A stand-in for the box binary; `_pause` is ignored and it sleeps.
	self := filepath.Join(root, "box")
	if err := os.WriteFile(self, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "run", "libpod", "tmp", "pause.pid")
	cmd, err := StartPause(self, pidFile)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EINVAL) {
			t.Skipf("user namespaces are not available here: %v", err)
		}
		t.Fatal(err)
	}
	defer StopPause(cmd)
	pid := cmd.Process.Pid
	data, err := os.ReadFile(pidFile)
	if err != nil || string(data) != strconv.Itoa(pid) {
		t.Errorf("pid file: %q, %v; want %d", data, err, pid)
	}
	proc := "/proc/" + strconv.Itoa(pid)
	// The user namespace maps the current user and group to root, with
	// setgroups denied, as podman expects of its pause process.
	for _, c := range []struct{ file, want string }{
		{"uid_map", fmt.Sprintf("         0 %10d          1\n", os.Getuid())},
		{"gid_map", fmt.Sprintf("         0 %10d          1\n", os.Getgid())},
		{"setgroups", "deny\n"},
	} {
		if data, err := os.ReadFile(filepath.Join(proc, c.file)); err != nil || string(data) != c.want {
			t.Errorf("%s: %q, %v; want %q", c.file, data, err, c.want)
		}
	}
	for _, ns := range []string{"user", "mnt"} {
		own, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.Readlink(filepath.Join(proc, "ns", ns)); err != nil || got == own {
			t.Errorf("%s namespace: %q, %v; want one of its own (not %q)", ns, got, err, own)
		}
	}
	// A second start must not overwrite the file.
	if _, err := StartPause(self, pidFile); err == nil || !errors.Is(err, os.ErrExist) {
		t.Errorf("second start: %v, want an exists error", err)
	}
	StopPause(cmd)
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Error("the pause process is still running after StopPause")
	}
}

func TestBuildPodmanSeatbelt(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	plan, err := buildSeatbelt(config.Profile{Podman: true, RWBinds: []string{"$PWD"}}, Options{
		Binary: "tool", Cwd: cwd, Home: home,
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}, "sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Writes) != 0 || len(plan.Dirs) != 0 {
		t.Errorf("podman must be ignored on macOS: Writes %v Dirs %v", plan.Writes, plan.Dirs)
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.Contains(w, "podman") }) {
		t.Errorf("want a warning about podman, got %v", plan.Warnings)
	}
}

func TestBuildLimits(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	opts := Options{
		Binary: "tool", Cwd: cwd, Home: home, ID: "42",
		Lookup: func(string) (string, error) { return filepath.Join(bin, "tool"), nil },
	}
	prof := config.Profile{Tmpfs: []string{"$HOME", "/var/tmp"}, CPUs: 1.5, Memory: "2G", TmpfsSize: "512M"}
	plan, err := buildBwrap(prof, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Limits == nil || plan.Limits.CPUs != 1.5 || plan.Limits.Memory != 2<<30 {
		t.Fatalf("Limits = %+v", plan.Limits)
	}
	if plan.Scope != "box-tool-42.scope" {
		t.Errorf("Scope = %q", plan.Scope)
	}
	args := " " + strings.Join(plan.Args, " ") + " "
	for _, want := range []string{" --size 536870912 --tmpfs " + home + " ", " --size 536870912 --tmpfs /var/tmp "} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in\n%s", want, args)
		}
	}
	if strings.Contains(args, "systemd-run") {
		t.Error("the scope is not part of the bwrap arguments")
	}
	cmd := plan.Command()
	if !strings.HasPrefix(cmd, "systemd-run --user --scope --quiet --unit box-tool-42.scope -p MemoryMax=2147483648 -p MemorySwapMax=0 -p CPUQuota=150% -- bwrap ") {
		t.Errorf("Command() = %s", cmd)
	}

	// Either limit alone; none at all.
	plan, err = buildBwrap(config.Profile{Tmpfs: []string{"$HOME"}, Memory: "1G"}, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if props := plan.Limits.Properties(); strings.Join(props, " ") != "MemoryMax=1073741824 MemorySwapMax=0" {
		t.Errorf("Properties() = %v", props)
	}
	if args := strings.Join(plan.Args, " "); strings.Contains(args, "--size") {
		t.Errorf("unexpected --size without tmpfs_size in %s", args)
	}
	plan, err = buildBwrap(config.Profile{CPUs: 0.5}, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if props := plan.Limits.Properties(); strings.Join(props, " ") != "CPUQuota=50%" {
		t.Errorf("Properties() = %v", props)
	}
	plan, err = buildBwrap(config.Profile{}, opts, "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Limits != nil || plan.Scope != "" || strings.HasPrefix(plan.Command(), "systemd-run") {
		t.Errorf("unexpected limits: %+v %q %s", plan.Limits, plan.Scope, plan.Command())
	}

	// Bad values are errors, not silently dropped limits.
	for _, bad := range []config.Profile{{Memory: "lots"}, {CPUs: -1}, {TmpfsSize: "1X"}} {
		if _, err := buildBwrap(bad, opts, "bwrap"); err == nil {
			t.Errorf("profile %+v: want an error", bad)
		}
	}
}

func TestScopeName(t *testing.T) {
	for name, want := range map[string]string{
		"claude":       "box-claude-7.scope",
		"my_tool.v2":   "box-my_tool.v2-7.scope",
		"weird name/é": `box-weird\x20name\x2f\xc3\xa9-7.scope`,
		".hidden":      `box-\x2ehidden-7.scope`,
		"a:b-c":        "box-a:b-c-7.scope",
	} {
		if got := ScopeName(name, "7"); got != want {
			t.Errorf("ScopeName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A persistent overlay layer that carries a symlink of the command pointing
// at a path the host lacks (claude updated itself inside: the symlink in
// ~/.local/bin persisted, the version in the tmpfs $HOME did not) is
// reported before the run, naming both versions and the way out.
func TestBuildRejectsStaleOverlaySymlink(t *testing.T) {
	home, cwd, tools, _ := fixture(t)
	state := filepath.Join(filepath.Dir(home), "state")
	versions := filepath.Join(tools, "versions")
	if err := os.MkdirAll(filepath.Join(versions, "1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(versions, "1.0"), filepath.Join(tools, "claude")); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(state, "overlays", "claude", layerName(tools), "upper")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	prof := config.Profile{Overlays: []config.Overlay{{Path: tools, Persist: true}}}
	opts := Options{
		Binary: "claude", Cwd: cwd, Home: home, StateDir: state,
		Lookup: func(string) (string, error) { return filepath.Join(tools, "claude"), nil },
	}

	// Untouched inside: the host's entry shows through.
	if _, err := buildBwrap(prof, opts, "bwrap"); err != nil {
		t.Fatalf("no layer entry: %v", err)
	}

	// Updated inside to a version the host has as well: fine.
	if err := os.MkdirAll(filepath.Join(versions, "1.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(versions, "1.1"), filepath.Join(upper, "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildBwrap(prof, opts, "bwrap"); err != nil {
		t.Fatalf("layer entry with existing target: %v", err)
	}

	// Updated inside to a version the host lacks: refused, with the facts.
	if err := os.RemoveAll(filepath.Join(versions, "1.1")); err != nil {
		t.Fatal(err)
	}
	_, err := buildBwrap(prof, opts, "bwrap")
	if err == nil {
		t.Fatal("dangling layer symlink should be rejected")
	}
	for _, want := range []string{
		filepath.Join(tools, "claude") + ": inside the sandbox it points at " + filepath.Join(versions, "1.1"),
		"the host's points at " + filepath.Join(versions, "1.0"),
		"`claude update`",
		"`box clean claude`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %q", want, err)
		}
	}

	// A relative symlink is resolved against the command's directory.
	if err := os.Remove(filepath.Join(upper, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("versions/1.0", filepath.Join(upper, "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildBwrap(prof, opts, "bwrap"); err != nil {
		t.Fatalf("relative layer symlink with existing target: %v", err)
	}

	// Other programs get no update command, only the clean-up.
	if err := os.Remove(filepath.Join(upper, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("versions/2.0", filepath.Join(upper, "claude")); err != nil {
		t.Fatal(err)
	}
	opts.Binary = "tool"
	toolUpper := filepath.Join(state, "overlays", "tool", layerName(tools), "upper")
	if err := os.MkdirAll(toolUpper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("versions/2.0", filepath.Join(toolUpper, "claude")); err != nil {
		t.Fatal(err)
	}
	_, err = buildBwrap(prof, opts, "bwrap")
	if err == nil || !strings.Contains(err.Error(), "install that version on the host, or `box clean tool`") {
		t.Errorf("want generic hint for tool, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "update`") {
		t.Errorf("no update command is known for tool: %v", err)
	}
}

func TestBuildRWParent(t *testing.T) {
	home, cwd, _, bin := fixture(t)
	lookup := func(string) (string, error) { return filepath.Join(bin, "tool"), nil }
	build := func(dir string) *Plan {
		t.Helper()
		plan, err := buildBwrap(config.Profile{RWBinds: []string{"$PWD"}, RWParent: true, Tmpfs: []string{"$HOME"}, Network: true},
			Options{Binary: "tool", Cwd: dir, Home: home, Lookup: lookup}, "bwrap")
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	// A nested project: its parent is bound read-write, after the tmpfs on $HOME.
	nested := filepath.Join(cwd, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := build(nested)
	cmd := " " + strings.Join(plan.Args, " ") + " "
	tmpfs := strings.Index(cmd, " --tmpfs "+home+" ")
	parent := strings.Index(cmd, " --bind "+cwd+" "+cwd+" ")
	self := strings.Index(cmd, " --bind "+nested+" "+nested+" ")
	if tmpfs < 0 || parent < 0 || self < 0 || tmpfs > parent || parent > self {
		t.Errorf("want tmpfs $HOME, then the parent, then $PWD bound read-write, got:\n%s", cmd)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", plan.Warnings)
	}

	// Directly under $HOME: the parent is the home directory, so it is skipped.
	plan = build(cwd)
	if strings.Contains(" "+strings.Join(plan.Args, " ")+" ", " --bind "+home+" "+home+" ") {
		t.Error("the home directory must never be bound read-write by rw_parent")
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "home directory") {
		t.Errorf("want a warning about the home directory, got %v", plan.Warnings)
	}

	// Directly under /: the parent is the root directory, so it is skipped.
	plan = build("/usr")
	if strings.Contains(" "+strings.Join(plan.Args, " ")+" ", " --bind / / ") {
		t.Error("the root directory must never be bound read-write by rw_parent")
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "root directory") {
		t.Errorf("want a warning about the root directory, got %v", plan.Warnings)
	}
}

func TestDefaultName(t *testing.T) {
	for _, tc := range []struct{ cwd, binary, want string }{
		{"/home/u/dev/box", "/usr/bin/claude", "box"},
		{"/home/u/dev/box/", "claude", "box"},
		{"/", "claude", "claude"},
		{"", "/usr/bin/claude", "claude"},
	} {
		if got := DefaultName(tc.cwd, tc.binary); got != tc.want {
			t.Errorf("DefaultName(%q, %q) = %q, want %q", tc.cwd, tc.binary, got, tc.want)
		}
	}
}
