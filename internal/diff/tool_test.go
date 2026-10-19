package diff

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWriteEqual(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, "a", "b", []byte("x\n"), []byte("x\n")); err != nil || out.Len() != 0 {
		t.Errorf("Write(equal) = %q, %v", out.String(), err)
	}
}

func TestWriteWithTool(t *testing.T) {
	if _, err := exec.LookPath("diff"); err != nil {
		t.Skip("diff not installed")
	}
	// Only diff: colordiff would add escape sequences.
	dir := t.TempDir()
	diff, _ := exec.LookPath("diff")
	if err := os.Symlink(diff, filepath.Join(dir, "diff")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	a, b := []byte("one\ntwo\n"), []byte("one\nTWO\n")
	var out bytes.Buffer
	if err := Write(&out, "host", "sandbox", a, b); err != nil {
		t.Fatal(err)
	}
	if want := Unified("host", "sandbox", a, b); out.String() != want {
		t.Errorf("Write via diff:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestWriteWithoutTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Tool(); err == nil {
		t.Fatal("Tool() should fail with an empty PATH")
	}
	a, b := []byte("one\ntwo\n"), []byte("one\nTWO\n")
	var out bytes.Buffer
	if err := Write(&out, "host", "sandbox", a, b); err != nil {
		t.Fatal(err)
	}
	if want := Unified("host", "sandbox", a, b); out.String() != want {
		t.Errorf("Write fallback:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestWriteToolFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "diff"), []byte("#!/bin/sh\necho broken >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	a, b := []byte("one\ntwo\n"), []byte("one\nTWO\n")
	var out bytes.Buffer
	if err := Write(&out, "host", "sandbox", a, b); err != nil {
		t.Fatal(err)
	}
	if want := Unified("host", "sandbox", a, b); out.String() != want {
		t.Errorf("Write after a failing tool:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestToolPrefersColordiff(t *testing.T) {
	diff, err := exec.LookPath("diff")
	if err != nil {
		t.Skip("diff not installed")
	}
	bin := t.TempDir()
	if err := os.Symlink(diff, filepath.Join(bin, "diff")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// Only diff on PATH: fall back silently.
	got, err := Tool()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(bin, "diff") {
		t.Fatalf("expected fallback to diff, got %s", got)
	}

	// colordiff present: prefer it.
	if err := os.Symlink(diff, filepath.Join(bin, "colordiff")); err != nil {
		t.Fatal(err)
	}
	got, err = Tool()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(bin, "colordiff") {
		t.Fatalf("expected colordiff, got %s", got)
	}

	// Neither present: error.
	t.Setenv("PATH", t.TempDir())
	if _, err := Tool(); err == nil {
		t.Fatal("expected error when no diff tool is available")
	}
}
