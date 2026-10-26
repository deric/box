package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/deric/box/internal/config"
)

func TestBuildSeatbeltSyncFiles(t *testing.T) {
	root := realTempDir(t)
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	for _, d := range []string{cwd, filepath.Join(home, ".claude", "projects"), filepath.Join(root, "etc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(home, ".claude.json"), filepath.Join(root, "etc", "tool.conf")} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(root, "box")
	tmpRoot := filepath.Join(root, "tmp", "box")
	prof := config.Profile{
		RWBinds:   []string{"$PWD"},
		Tmpfs:     []string{"$HOME"},
		SyncFiles: []string{"~/.claude", "~/.claude.json", filepath.Join(root, "link", "tool.conf"), "?~/missing", "~/fifo", "~/absent"},
	}
	plan, err := buildSeatbelt(prof, Options{
		Binary: "claude", Args: []string{"--flag"}, Cwd: cwd, Home: home, Self: self,
		TmpRoot: tmpRoot, ID: "42", StateDir: filepath.Join(root, "state"),
		Lookup: func(string) (string, error) { return filepath.Join(root, "claude"), nil },
	}, "/usr/bin/sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	syncDir := filepath.Join(tmpRoot, "claude-42.sync")
	if plan.SyncDir != syncDir {
		t.Errorf("SyncDir = %q, want %q", plan.SyncDir, syncDir)
	}
	want := []SyncFile{
		{Path: syncDir + "/copy/.claude", Mount: home + "/.claude", Base: syncDir + "/base/.claude"},
		{Path: syncDir + "/copy/.claude/.claude.json", Mount: home + "/.claude.json", Base: syncDir + "/base/.claude/.claude.json"},
		{Path: syncDir + "/copy" + root + "/etc/tool.conf", Mount: root + "/etc/tool.conf", Base: syncDir + "/base" + root + "/etc/tool.conf"},
	}
	if !slices.Equal(plan.Sync, want) {
		t.Errorf("Sync = %+v\nwant %+v", plan.Sync, want)
	}
	saveDir := filepath.Join(root, "state", "sync")
	if plan.SyncSave != saveDir || !slices.Contains(plan.Dirs, saveDir) {
		t.Errorf("SyncSave = %q, Dirs = %v, want %s created", plan.SyncSave, plan.Dirs, saveDir)
	}
	if !slices.Contains(plan.Env, "BOX_SYNC_DIR="+syncDir+"/copy") {
		t.Errorf("BOX_SYNC_DIR not set to the copies: %q", plan.Env)
	}
	if !slices.Contains(plan.Env, "CLAUDE_CONFIG_DIR="+syncDir+"/copy/.claude") {
		t.Errorf("CLAUDE_CONFIG_DIR not set to the copy: %q", plan.Env)
	}

	// The forwarder runs on the host around sandbox-exec.
	if plan.Exe != self {
		t.Errorf("Exe = %q, want the box executable", plan.Exe)
	}
	head := []string{"_forward", "-x", "-W", saveDir}
	for _, sf := range want {
		head = append(head, "-w", sf.Arg())
	}
	head = append(head, "--", "/usr/bin/sandbox-exec", "-p")
	if len(plan.Args) < len(head)+4 || !slices.Equal(plan.Args[:len(head)], head) ||
		!slices.Equal(plan.Args[len(head)+1:], []string{"--", "claude", "--flag"}) {
		t.Fatalf("unexpected args %q", plan.Args)
	}
	profile := plan.Args[len(head)]
	for _, rule := range []string{
		`(deny file-read* file-write* network-outbound (subpath "` + home + `/.claude"))`,
		`(deny file-read* file-write* network-outbound (subpath "` + home + `/.claude.json"))`,
		`(allow file-read* file-write* (subpath "` + syncDir + `/copy/.claude"))`,
		`(allow file-read* file-write* (subpath "` + syncDir + `/copy/.claude/.claude.json"))`,
		`(allow file-read* file-write* (subpath "` + syncDir + `/copy` + root + `/etc/tool.conf"))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Errorf("missing %s in\n%s", rule, profile)
		}
	}
	if strings.Contains(profile, `(subpath "`+syncDir+`/base`) {
		t.Errorf("the base copies must stay out of reach:\n%s", profile)
	}
	if strings.Contains(profile, `(subpath "`+self+`"))`) {
		t.Errorf("box itself need not be exposed, the forwarder runs outside:\n%s", profile)
	}
	if len(plan.Warnings) != 2 ||
		!strings.Contains(plan.Warnings[0], "fifo skipped: not a regular file or a directory") ||
		!strings.Contains(plan.Warnings[1], "absent skipped") {
		t.Errorf("warnings = %q", plan.Warnings)
	}
	cmd := plan.Command()
	if !strings.Contains(cmd, " "+self+" _forward -x -W "+saveDir+" -w ") || !strings.Contains(cmd, " -- /usr/bin/sandbox-exec -p ") {
		t.Errorf("Command() = %s", cmd)
	}
}

func TestBuildSeatbeltSyncFilesEnvOverride(t *testing.T) {
	root := realTempDir(t)
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSeatbelt(config.Profile{
		SyncFiles: []string{"~/.claude"},
		Env:       map[string]string{"CLAUDE_CONFIG_DIR": "/elsewhere"},
	}, Options{
		Binary: "claude", Cwd: root, Home: home, Self: filepath.Join(root, "box"),
		TmpRoot: filepath.Join(root, "box-tmp"), ID: "7",
		Lookup: func(string) (string, error) { return filepath.Join(root, "claude"), nil },
	}, "/usr/bin/sandbox-exec")
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, kv := range plan.Env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			seen = append(seen, kv)
		}
	}
	if !slices.Equal(seen, []string{"CLAUDE_CONFIG_DIR=/elsewhere"}) {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the profile's value alone", seen)
	}
}

func TestSyncCopyName(t *testing.T) {
	home := "/Users/u"
	for _, tc := range []struct{ host, want string }{
		{"/Users/u/.claude.json", ".claude/.claude.json"},
		{"/Users/u/.claude", ".claude"},
		{"/Users/u/.config/gh", ".config/gh"},
		{"/etc/tool.conf", "etc/tool.conf"},
		{"/Users/other/.claude.json", "Users/other/.claude.json"},
	} {
		if got := syncCopyName(tc.host, home, []string{"/Users/u/.claude", "/Users/u/.claude.json"}); got != tc.want {
			t.Errorf("syncCopyName(%s) = %q, want %q", tc.host, got, tc.want)
		}
	}
	if got := syncCopyName("/Users/u/.claude.json", home, []string{"/Users/u/.claude.json"}); got != ".claude.json" {
		t.Errorf("without ~/.claude synced, .claude.json = %q", got)
	}
}
