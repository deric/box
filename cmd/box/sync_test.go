package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deric/box/internal/sandbox"
)

func conflictFixture(t *testing.T) (sandbox.Conflict, string, string) {
	t.Helper()
	// Without diff(1) on PATH the built-in diff renders, so the output is
	// the same everywhere (colordiff would add escape sequences).
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	host := filepath.Join(dir, "host.json")
	if err := os.WriteFile(host, []byte("{\n  \"host\": true\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveDir := filepath.Join(dir, "saved")
	if err := os.Mkdir(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := sandbox.Conflict{
		File: sandbox.SyncFile{Path: "/home/u/.claude.json", Mount: host},
		Host: []byte("{\n  \"host\": true\n}\n"),
		Copy: []byte("{\n  \"sandbox\": true\n}\n"),
	}
	return c, host, saveDir
}

func TestResolveConflictsOverwrite(t *testing.T) {
	c, host, saveDir := conflictFixture(t)
	var out bytes.Buffer
	resolveConflicts([]sandbox.Conflict{c}, true, strings.NewReader("what\no\n"), &out, saveDir, "/host/saved")
	got := out.String()
	for _, want := range []string{
		"box: /home/u/.claude.json changed on the host while the sandbox ran",
		"--- /home/u/.claude.json (host)\n+++ /home/u/.claude.json (sandbox)\n",
		"-  \"host\": true\n+  \"sandbox\": true\n",
		"[o/K] ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "[o/K] ") != 2 {
		t.Errorf("an unknown answer should repeat the question:\n%s", got)
	}
	if data, _ := os.ReadFile(host); string(data) != string(c.Copy) {
		t.Errorf("host = %q, want the sandbox copy", data)
	}
	if entries, _ := os.ReadDir(saveDir); len(entries) != 0 {
		t.Errorf("nothing should be saved on overwrite, got %v", entries)
	}
}

func TestResolveConflictsKeep(t *testing.T) {
	for _, answer := range []string{"k\n", "\n", "merge\n", ""} {
		c, host, saveDir := conflictFixture(t)
		var out bytes.Buffer
		resolveConflicts([]sandbox.Conflict{c}, true, strings.NewReader(answer), &out, saveDir, "/host/saved")
		if data, _ := os.ReadFile(host); string(data) != string(c.Host) {
			t.Errorf("answer %q: host = %q, want it kept", answer, data)
		}
		entries, _ := os.ReadDir(saveDir)
		if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".claude.json.") {
			t.Fatalf("answer %q: saved = %v, want one dated copy", answer, entries)
		}
		if data, _ := os.ReadFile(filepath.Join(saveDir, entries[0].Name())); string(data) != string(c.Copy) {
			t.Errorf("answer %q: saved copy = %q", answer, data)
		}
		want := "box: kept /home/u/.claude.json; the sandbox copy is saved as /host/saved/" + entries[0].Name() + " to merge by hand\n"
		if !strings.Contains(out.String(), want) {
			t.Errorf("answer %q: missing %q in:\n%s", answer, want, out.String())
		}
	}
}

func TestResolveConflictsNoTerminal(t *testing.T) {
	c, host, saveDir := conflictFixture(t)
	var out bytes.Buffer
	resolveConflicts([]sandbox.Conflict{c}, false, strings.NewReader("o\n"), &out, saveDir, "/host/saved")
	if strings.Contains(out.String(), "[o/K]") {
		t.Errorf("no question without a terminal:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no terminal to ask; keeping the host file") {
		t.Errorf("missing warning:\n%s", out.String())
	}
	if data, _ := os.ReadFile(host); string(data) != string(c.Host) {
		t.Errorf("host = %q, want it kept", data)
	}
	if entries, _ := os.ReadDir(saveDir); len(entries) != 1 {
		t.Errorf("saved = %v, want one copy", entries)
	}
}

func TestResolveConflictsSaveFails(t *testing.T) {
	c, host, _ := conflictFixture(t)
	var out bytes.Buffer
	resolveConflicts([]sandbox.Conflict{c}, false, strings.NewReader(""), &out, filepath.Join(t.TempDir(), "missing"), "/host/saved")
	if !strings.Contains(out.String(), "saving the sandbox copy of /home/u/.claude.json") {
		t.Errorf("missing save error:\n%s", out.String())
	}
	if data, _ := os.ReadFile(host); string(data) != string(c.Host) {
		t.Errorf("host = %q, want it kept even when saving fails", data)
	}
}
