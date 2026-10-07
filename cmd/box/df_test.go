package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatherUsage(t *testing.T) {
	root := t.TempDir()
	overlays := filepath.Join(root, "overlays")
	tmp := filepath.Join(root, "tmp")
	logs := filepath.Join(root, "logs")
	write := func(path string, size int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(overlays, "claude", "home-u-.local-bin", "upper", "tool"), 3<<20)
	write(filepath.Join(overlays, "claude", "home-u-.local-bin", "upper", "sub", "more"), 1<<20)
	write(filepath.Join(overlays, "claude", "home-u-.local-share-mise", "upper", "f"), 1<<20)
	write(filepath.Join(overlays, "sh", "layer", "upper", "f"), 1<<20)
	write(filepath.Join(tmp, "claude-100", "scratch"), 2<<20)
	write(filepath.Join(tmp, "claude-101", "scratch"), 1<<20)
	write(filepath.Join(tmp, "claude-100.sock"), 0)
	write(filepath.Join(tmp, "claude-100-home-u-proj.log"), 1<<20)
	write(filepath.Join(tmp, "unrelated"), 1<<20)
	write(filepath.Join(tmp, "notes.txt"), 1<<20)
	write(filepath.Join(logs, "sh-7-home-u.log"), 1<<20)
	write(filepath.Join(logs, "sh-7"), 1<<20) // only logs count outside the tmp root

	// overlayfs keeps an unreadable work/work directory in each layer; the
	// walk must get past it without an error.
	work := filepath.Join(overlays, "claude", "home-u-.local-bin", "work", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(work, 0o755) })

	du := func(path string) int64 {
		t.Helper()
		n, err := dirSize(path)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Listing the tmp root among the log dirs must not count it twice.
	got, err := gatherUsage(overlays, tmp, []string{tmp, logs}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []diskEntry{
		{"claude", "overlay", filepath.Join(overlays, "claude", "home-u-.local-bin"), du(filepath.Join(overlays, "claude", "home-u-.local-bin"))},
		{"claude", "overlay", filepath.Join(overlays, "claude", "home-u-.local-share-mise"), du(filepath.Join(overlays, "claude", "home-u-.local-share-mise"))},
		{"sh", "overlay", filepath.Join(overlays, "sh", "layer"), du(filepath.Join(overlays, "sh", "layer"))},
		{"claude", "tmp", filepath.Join(tmp, "claude-100"), du(filepath.Join(tmp, "claude-100"))},
		{"claude", "log", filepath.Join(tmp, "claude-100-home-u-proj.log"), du(filepath.Join(tmp, "claude-100-home-u-proj.log"))},
		{"claude", "tmp", filepath.Join(tmp, "claude-101"), du(filepath.Join(tmp, "claude-101"))},
		{"sh", "log", filepath.Join(logs, "sh-7-home-u.log"), du(filepath.Join(logs, "sh-7-home-u.log"))},
	}
	if len(got) != len(want) {
		t.Fatalf("gatherUsage = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v; want %+v", i, got[i], want[i])
		}
		if want[i].size < 1<<20 {
			t.Errorf("entry %d: implausibly small size %+v", i, want[i])
		}
	}

	got, err = gatherUsage(overlays, tmp, []string{logs}, "sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[2] || got[1] != want[6] {
		t.Errorf("gatherUsage(filter sh) = %+v; want only sh entries", got)
	}

	got, err = gatherUsage(filepath.Join(root, "missing"), filepath.Join(root, "missing"), nil, "")
	if err != nil || len(got) != 0 {
		t.Errorf("gatherUsage on missing dirs = %v, %v; want empty, nil", got, err)
	}
}

func TestPrintDf(t *testing.T) {
	entries := []diskEntry{
		{"sh", "log", "/tmp/box/sh-7-home-u.log", 1 << 10},
		{"claude", "tmp", "/tmp/box/claude-100", 2 << 20},
		{"claude", "overlay", "/state/overlays/claude/layer", 5 << 20},
		{"claude", "log", "/tmp/box/claude-100-home-u.log", 3 << 10},
	}
	var b strings.Builder
	if err := printSummary(&b, entries); err != nil {
		t.Fatal(err)
	}
	wantSummary := "NAME\tOVERLAYS\tTMP\tLOGS\tTOTAL\n" +
		"claude\t5.0MiB\t2.0MiB\t3.0KiB\t7.0MiB\n" +
		"sh\t0B\t0B\t1.0KiB\t1.0KiB\n" +
		"total\t5.0MiB\t2.0MiB\t4.0KiB\t7.0MiB\n"
	if b.String() != wantSummary {
		t.Errorf("printSummary:\n%s\nwant:\n%s", b.String(), wantSummary)
	}

	b.Reset()
	if err := printEntries(&b, entries); err != nil {
		t.Fatal(err)
	}
	wantAll := "NAME\tKIND\tPATH\tSIZE\n" +
		"claude\toverlay\t/state/overlays/claude/layer\t5.0MiB\n" +
		"claude\ttmp\t/tmp/box/claude-100\t2.0MiB\n" +
		"claude\tlog\t/tmp/box/claude-100-home-u.log\t3.0KiB\n" +
		"\t\ttotal\t7.0MiB\n" +
		"sh\tlog\t/tmp/box/sh-7-home-u.log\t1.0KiB\n" +
		"total\t\t\t7.0MiB\n"
	if b.String() != wantAll {
		t.Errorf("printEntries:\n%s\nwant:\n%s", b.String(), wantAll)
	}
}
