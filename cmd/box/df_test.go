package main

import (
	"bytes"
	"os"
	"path/filepath"
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
	write(filepath.Join(overlays, "sh", "layer", "upper", "f"), 1<<20)
	write(filepath.Join(tmp, "claude-100", "scratch"), 2<<20)
	write(filepath.Join(tmp, "claude-101", "scratch"), 1<<20)
	write(filepath.Join(tmp, "claude-100.sock"), 0)
	write(filepath.Join(tmp, "claude-100-home-u-proj.log"), 1<<20)
	write(filepath.Join(tmp, "unrelated"), 1<<20)
	write(filepath.Join(tmp, "notes.txt"), 1<<20)
	write(filepath.Join(logs, "sh-7-home-u.log"), 1<<20)
	write(filepath.Join(logs, "sh-7"), 1<<20) // only logs count outside the tmp root

	// overlayfs keeps an unreadable work/work directory in each layer; it
	// is counted but not descended into, and must not produce a warning.
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
	if n, err := dirSize(work); err != nil || n == 0 {
		t.Errorf("dirSize(unreadable work dir) = %d, %v; want its own size, nil", n, err)
	}
	want := map[string]diskUsage{
		"claude": {
			overlays: du(filepath.Join(overlays, "claude")),
			tmp:      du(filepath.Join(tmp, "claude-100")) + du(filepath.Join(tmp, "claude-101")),
			logs:     du(filepath.Join(tmp, "claude-100-home-u-proj.log")),
		},
		"sh": {
			overlays: du(filepath.Join(overlays, "sh")),
			logs:     du(filepath.Join(logs, "sh-7-home-u.log")),
		},
	}
	if len(got) != len(want) {
		t.Fatalf("gatherUsage = %v; want %v", got, want)
	}
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("gatherUsage[%q] = %+v; want %+v", name, g, w)
		}
		if w.total() < 1<<20 {
			t.Errorf("%s: implausibly small usage %+v", name, w)
		}
	}
	if got["claude"].overlays < 4<<20 {
		t.Errorf("claude overlays = %d; want at least 4MiB", got["claude"].overlays)
	}

	got, err = gatherUsage(overlays, tmp, []string{logs}, "sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["sh"] != want["sh"] {
		t.Errorf("gatherUsage(filter sh) = %v; want only %+v", got, want["sh"])
	}

	got, err = gatherUsage(filepath.Join(root, "missing"), filepath.Join(root, "missing"), nil, "")
	if err != nil || len(got) != 0 {
		t.Errorf("gatherUsage on missing dirs = %v, %v; want empty, nil", got, err)
	}
}
