package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNotFoundDetail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/nonexistent")

	if d := notFoundDetail("missing"); d != "" {
		t.Errorf("no entry at all: want nothing, got %q", d)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if d := notFoundDetail("plain"); d != "" {
		t.Errorf("regular file: want nothing, got %q", d)
	}
	if err := os.Symlink(filepath.Join(dir, "plain"), filepath.Join(dir, "ok")); err != nil {
		t.Fatal(err)
	}
	if d := notFoundDetail("ok"); d != "" {
		t.Errorf("working symlink: want nothing, got %q", d)
	}
	if err := os.Symlink(filepath.Join(dir, "versions", "2.0"), filepath.Join(dir, "claude")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "claude") + " points at " + filepath.Join(dir, "versions", "2.0") + ", which does not exist inside the sandbox"
	if d := notFoundDetail("claude"); d != want {
		t.Errorf("dangling symlink via PATH:\n got %q\nwant %q", d, want)
	}
	if d := notFoundDetail(filepath.Join(dir, "claude")); d != want {
		t.Errorf("dangling symlink by path:\n got %q\nwant %q", d, want)
	}
}
