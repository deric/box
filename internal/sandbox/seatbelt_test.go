package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/deric/box/internal/config"
)

// realTempDir returns a temporary directory with symlinks resolved (on macOS
// the temporary directory lives under the /var -> private/var symlink).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuildSeatbelt(t *testing.T) {
	root := realTempDir(t)
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	tools := filepath.Join(root, "tools")
	real := filepath.Join(root, "real")
	for _, d := range []string{cwd, tools, real} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	prof := config.Profile{
		ROBinds:       []string{filepath.Join(root, "link"), "?/does/not/exist"},
		RWBinds:       []string{"$PWD", "?$BOX_TEST_UNSET_VARIABLE"},
		Tmpfs:         []string{"$HOME"},
		Overlays:      []config.Overlay{{Path: tools, Persist: true}},
		Network:       true,
		Hostname:      "box",
		Env:           map[string]string{"B": "2", "A": "1"},
		UnsetEnv:      []string{"GONE"},
		ClearEnv:      true,
		PassEnv:       []string{"BOX_T_PASS"},
		ExtraArgs:     []string{"-D", "K=V"},
		SeatbeltRules: []string{`(allow mach-lookup (global-name "x"))`},
	}
	t.Setenv("BOX_T_PASS", "yes")
	plan, err := buildSeatbelt(prof, Options{
		Binary: "tool",
		Args:   []string{"--flag"},
		Cwd:    cwd,
		Home:   home,
		Lookup: func(string) (string, error) { return filepath.Join(tools, "tool"), nil },
	}, "/usr/bin/sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Args) != 7 || plan.Args[0] != "-D" || plan.Args[2] != "-p" ||
		!slices.Equal(plan.Args[4:], []string{"--", "tool", "--flag"}) {
		t.Fatalf("unexpected args %q", plan.Args)
	}
	profile := plan.Args[3]
	for _, want := range []string{
		"(deny default)",
		"(allow network* system-socket)",
		`(allow file-read* (subpath "` + real + `"))`,
		`(deny file-read* file-write* network-outbound (subpath "` + home + `"))`,
		`(allow file-read* file-write* (subpath "` + cwd + `"))`,
		`(allow file-read* (subpath "` + tools + `"))`,
		`(allow mach-lookup (global-name "x"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile is missing %s:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "link") {
		t.Error("symlinks must be resolved: Seatbelt matches real paths")
	}
	if strings.Index(profile, `(deny file-read* file-write* network-outbound (subpath "`+home) >
		strings.Index(profile, `(allow file-read* file-write* (subpath "`+cwd) {
		t.Error("the $HOME rule must precede the rule for a path under it")
	}
	if !strings.HasSuffix(profile, "(allow mach-lookup (global-name \"x\"))\n") {
		t.Error("seatbelt_rules must come last so they take precedence")
	}

	if want := []string{"BOX_T_PASS=yes", "A=1", "B=2", "BOX_NAME=" + filepath.Base(cwd), "BOX_BINARY=tool", "BOX_DIR=" + cwd}; !slices.Equal(plan.Env, want) {
		t.Errorf("env = %q, want %q", plan.Env, want)
	}
	if !plan.ClearEnv || !slices.Equal(plan.UnsetEnv, []string{"GONE"}) || plan.Dir != cwd {
		t.Errorf("unexpected environment setup %+v", plan)
	}
	if len(plan.Dirs) != 0 {
		t.Errorf("overlay layers must not be created, got %v", plan.Dirs)
	}
	var overlayWarned, hostnameWarned bool
	for _, w := range plan.Warnings {
		overlayWarned = overlayWarned || strings.Contains(w, "overlay "+tools)
		hostnameWarned = hostnameWarned || strings.Contains(w, "hostname")
	}
	if !overlayWarned || !hostnameWarned || len(plan.Warnings) != 2 {
		t.Errorf("want overlay and hostname warnings, got %q", plan.Warnings)
	}
	if cmd := plan.Command(); !strings.HasPrefix(cmd, "env -i -u GONE BOX_T_PASS=yes A=1 B=2 BOX_NAME="+filepath.Base(cwd)+" BOX_BINARY=tool ") {
		t.Errorf("command = %s", cmd)
	}
}

func TestBuildSeatbeltNoNetwork(t *testing.T) {
	cwd := realTempDir(t)
	plan, err := buildSeatbelt(config.Profile{RWBinds: []string{"$PWD"}}, Options{
		Binary: "tool", Cwd: cwd, Home: cwd,
		Lookup: func(string) (string, error) { return filepath.Join(cwd, "tool"), nil },
	}, "sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	profile := plan.Args[1]
	if strings.Contains(profile, "network*") || strings.Contains(profile, "mDNSResponder") {
		t.Errorf("network should be disabled:\n%s", profile)
	}
	if !strings.Contains(profile, `(allow network-outbound (subpath "`+cwd+`"))`) {
		t.Error("Unix sockets under exposed paths should stay reachable")
	}
}

func TestBuildSeatbeltPrivateTmp(t *testing.T) {
	root := realTempDir(t)
	cwd := filepath.Join(root, "project")
	hostTmp := filepath.Join(root, "T")
	for _, d := range []string{cwd, hostTmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", hostTmp+"/")
	tmpRoot := filepath.Join(root, "box") // not created yet
	plan, err := buildSeatbelt(config.Profile{RWBinds: []string{"$PWD", "/tmp", "$TMPDIR"}, PrivateTmp: true}, Options{
		Binary: "tool", Cwd: cwd, Home: root, TmpRoot: tmpRoot, ID: "42",
		Lookup: func(string) (string, error) { return filepath.Join(cwd, "tool"), nil },
	}, "sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(tmpRoot, "tool-42")
	if plan.TmpDir != private {
		t.Errorf("TmpDir = %q, want %q", plan.TmpDir, private)
	}
	if !slices.Contains(plan.Env, "TMPDIR="+private+"/") {
		t.Errorf("TMPDIR not set to the private directory: %q", plan.Env)
	}
	if !slices.Contains(plan.Env, "CLAUDE_CODE_TMPDIR="+private) {
		t.Errorf("CLAUDE_CODE_TMPDIR not set to the private directory: %q", plan.Env)
	}
	profile := plan.Args[1]
	hide := func(p string) int {
		return strings.LastIndex(profile, `(deny file-read* file-write* network-outbound (subpath "`+p+`"))`)
	}
	allowTmp := strings.LastIndex(profile, `(allow file-read* file-write* (subpath "/private/tmp"))`)
	allowHostTmp := strings.LastIndex(profile, `(allow file-read* file-write* (subpath "`+hostTmp+`"))`)
	if hide("/private/tmp") < allowTmp || hide(hostTmp) < allowHostTmp {
		t.Errorf("/tmp and $TMPDIR must be hidden even when bound:\n%s", profile)
	}
	if !strings.Contains(profile, `(allow file-read* file-write* (subpath "`+private+`"))`) ||
		strings.Index(profile, `(subpath "`+private+`")`) < hide(hostTmp) {
		t.Errorf("the private directory must be writable after the hidden ones:\n%s", profile)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("unexpected warnings %q", plan.Warnings)
	}
}

func TestBuildSeatbeltPrivateTmpKeepsCwd(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("TMPDIR", root)
	plan, err := buildSeatbelt(config.Profile{RWBinds: []string{"$PWD"}, PrivateTmp: true}, Options{
		Binary: "tool", Cwd: root, Home: root, TmpRoot: filepath.Join(root, "box"), ID: "1",
		Lookup: func(string) (string, error) { return filepath.Join(root, "tool"), nil },
	}, "sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Args[1], `(deny file-read* file-write* network-outbound (subpath "`+root+`"))`) {
		t.Errorf("a $TMPDIR that contains the working directory must not be hidden:\n%s", plan.Args[1])
	}
}

func TestRealPathPrefix(t *testing.T) {
	root := realTempDir(t)
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if got, want := realPathPrefix(filepath.Join(root, "link", "a", "b")), filepath.Join(root, "a", "b"); got != want {
		t.Errorf("realPathPrefix = %q, want %q", got, want)
	}
}

func TestPlanEnviron(t *testing.T) {
	t.Setenv("BOX_TEST_KEEP", "1")
	t.Setenv("BOX_TEST_DROP", "1")
	t.Setenv("BOX_TEST_SET", "old")
	p := &Plan{UnsetEnv: []string{"BOX_TEST_DROP"}, Env: []string{"BOX_TEST_SET=new"}}
	env := p.environ()
	if !slices.Contains(env, "BOX_TEST_KEEP=1") || slices.Contains(env, "BOX_TEST_DROP=1") ||
		slices.Contains(env, "BOX_TEST_SET=old") || !slices.Contains(env, "BOX_TEST_SET=new") {
		t.Errorf("unexpected environment %q", env)
	}
	p.ClearEnv = true
	if env := p.environ(); !slices.Equal(env, []string{"BOX_TEST_SET=new"}) {
		t.Errorf("clear_env: got %q", env)
	}
}

func TestSBPLString(t *testing.T) {
	if got, want := sbplString(`/a "b" \c`), `"/a \"b\" \\c"`; got != want {
		t.Errorf("sbplString = %s, want %s", got, want)
	}
}

// TestSeatbeltSandbox runs a shell under the default macOS profile and checks
// what it can reach.
func TestSeatbeltSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is macOS only")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not found")
	}
	home := realTempDir(t)
	cwd := filepath.Join(home, "project")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "secret"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(config.DefaultDarwinTOML)
	if err != nil {
		t.Fatal(err)
	}
	const script = `
fail() { echo "FAIL: $*"; exit 1; }
echo hello > out.txt || fail "cannot write the working directory"
ls /usr/bin > /dev/null || fail "cannot list /usr/bin"
cat ../secret 2> /dev/null && fail "file under the hidden \$HOME is readable"
touch ../new 2> /dev/null && fail "hidden \$HOME is writable"
touch /usr/bin/box-test 2> /dev/null && fail "/usr is writable"
ls /Users > /dev/null 2>&1 && fail "/Users is listable"
cat /tmp/box-test-shared 2> /dev/null && fail "the host's /tmp is readable"
touch /tmp/box-test-new 2> /dev/null && fail "the host's /tmp is writable"
echo tmp > "$TMPDIR/file" || fail "cannot write the private TMPDIR"
case "$TMPDIR" in */box-test/sh-*) ;; *) fail "TMPDIR is $TMPDIR" ;; esac
echo "ok $BOX_NAME $BOX_BINARY $BOX_SANDBOX"
`
	shared := "/tmp/box-test-shared"
	if err := os.WriteFile(shared, []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(shared); _ = os.Remove("/tmp/box-test-new") })
	plan, err := Build(cfg.Resolve("sh"), Options{
		Binary: "/bin/sh", Args: []string{"-c", script}, Cwd: cwd, Home: home,
		TmpRoot: filepath.Join(home, "box-test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := makeTmpDir(plan.TmpDir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(plan.Exe, plan.Args...)
	cmd.Dir, cmd.Env = plan.Dir, plan.environ()
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok "+filepath.Base(cwd)+" sh 1" {
		t.Fatalf("sandboxed shell failed: %v\n%s\nprofile:\n%s", err, out, plan.Args[1])
	}
	if data, err := os.ReadFile(filepath.Join(cwd, "out.txt")); err != nil || string(data) != "hello\n" {
		t.Errorf("write in the working directory did not reach the host: %q %v", data, err)
	}
}
