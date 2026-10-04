package sandbox

import (
	"os"
	"path/filepath"
	"strings"
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
