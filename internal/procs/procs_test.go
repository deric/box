package procs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeProc creates a fake /proc/<pid> with the stat fields List reads.
func writeProc(t *testing.T, root string, pid, ppid int, comm string, utime, stime, start, rss uint64, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fields 3..24 of proc(5); unused ones are zero.
	f := make([]string, 22)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[4-3] = strconv.Itoa(ppid)
	f[14-3] = strconv.FormatUint(utime, 10)
	f[15-3] = strconv.FormatUint(stime, 10)
	f[22-3] = strconv.FormatUint(start, 10)
	f[24-3] = strconv.FormatUint(rss, 10)
	stat := fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(f, " "))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdline := strings.Join(argv, "\x00") + "\x00"
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3\nbtime 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bwrapArgs := []string{"--unshare-all", "--chdir", "/work", "--", "claude", "--resume"}
	// Sandbox: outer bwrap -> inner bwrap -> claude -> node (comm with spaces).
	writeProc(t, root, 100, 1, "bwrap", 1, 1, 500, 10, append([]string{"box:claude"}, bwrapArgs...)...)
	writeProc(t, root, 101, 100, "bwrap", 0, 0, 501, 5, append([]string{"box:claude"}, bwrapArgs...)...)
	writeProc(t, root, 102, 101, "claude", 100, 50, 502, 1000, "claude", "--resume")
	writeProc(t, root, 103, 102, "a (b) c", 48, 0, 503, 200, "node")
	// A later sandbox started first in PID order.
	writeProc(t, root, 50, 1, "bwrap", 0, 0, 900, 1, "box:sh", "--", "sh")
	// Unrelated processes, including a bwrap not started by box.
	writeProc(t, root, 200, 1, "bwrap", 0, 0, 100, 1, "bwrap", "--", "flatpak")
	writeProc(t, root, 201, 1, "bash", 0, 0, 100, 1, "bash")
	// Kernel thread: empty cmdline.
	writeProc(t, root, 2, 0, "kthreadd", 0, 0, 0, 0)

	got, err := List(root, "box:")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sandboxes, want 2: %+v", len(got), got)
	}
	s := got[0]
	if s.PID != 100 || s.Name != "claude" || s.Dir != "/work" || s.Procs != 4 {
		t.Errorf("unexpected sandbox %+v", s)
	}
	if strings.Join(s.Command, " ") != "claude --resume" {
		t.Errorf("command = %q", s.Command)
	}
	if s.CPU != 2*time.Second {
		t.Errorf("cpu = %v, want 2s", s.CPU)
	}
	if want := uint64(1215 * os.Getpagesize()); s.RSS != want {
		t.Errorf("rss = %d, want %d", s.RSS, want)
	}
	if want := time.Unix(1005, 0); !s.Started.Equal(want) {
		t.Errorf("started = %v, want %v", s.Started, want)
	}
	if got[1].PID != 50 || got[1].Name != "sh" || got[1].Procs != 1 {
		t.Errorf("unexpected second sandbox %+v", got[1])
	}
}

func TestListReal(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no procfs")
	}
	if _, err := List("/proc", "box-test-no-such-prefix:"); err != nil {
		t.Fatal(err)
	}
}
