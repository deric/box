package procs

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func TestParsePS(t *testing.T) {
	now := time.Unix(100000, 0)
	out := "    1     0  12345   1:02.50 3-04:05:06\n" +
		"  200     1    100   0:00.01      00:07\n" +
		"  garbage line\n" +
		"  300     1     10 123:00.00   01:00:00\n"
	all := parsePS([]byte(out), now)
	if len(all) != 3 {
		t.Fatalf("got %d processes, want 3", len(all))
	}
	p := all[1]
	if p.ppid != 0 || p.rss != 12345*1024 || p.cpu != 62500*time.Millisecond {
		t.Errorf("unexpected pid 1: %+v", p)
	}
	if want := now.Add(-(3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second)); !p.started.Equal(want) {
		t.Errorf("started = %v, want %v", p.started, want)
	}
	if all[200].started != now.Add(-7*time.Second) || all[300].cpu != 123*time.Minute {
		t.Errorf("unexpected %+v %+v", all[200], all[300])
	}
}

func procArgs(argv, env []string) []byte {
	buf := binary.NativeEndian.AppendUint32(nil, uint32(len(argv)))
	buf = append(buf, "/bin/exe\x00\x00\x00\x00"...)
	for _, s := range append(append(argv, env...), "", "ptr_munge=") {
		buf = append(buf, s+"\x00"...)
	}
	return buf
}

func TestParseProcArgs(t *testing.T) {
	argv, env := parseProcArgs(procArgs([]string{"claude", "", "--resume"}, []string{"A=1", "BOX_NAME=claude"}))
	if !slices.Equal(argv, []string{"claude", "", "--resume"}) || !slices.Equal(env, []string{"A=1", "BOX_NAME=claude"}) {
		t.Errorf("argv=%q env=%q", argv, env)
	}
	if argv, env := parseProcArgs([]byte{1, 0}); argv != nil || env != nil {
		t.Error("short buffer should yield nothing")
	}
}

func TestGroupByEnv(t *testing.T) {
	start := time.Unix(1000, 0)
	tag := []string{"BOX_NAME=claude", "BOX_DIR=/work"}
	all := map[int]*proc{
		10: {pid: 10, ppid: 1, argv: []string{"zsh"}, started: start},
		11: {pid: 11, ppid: 10, argv: []string{"claude", "-c"}, env: tag, cpu: time.Second, rss: 100, started: start.Add(time.Second)},
		12: {pid: 12, ppid: 11, argv: []string{"node"}, env: tag, cpu: time.Second, rss: 50},
		13: {pid: 13, ppid: 12, cpu: time.Second}, // unreadable, still counted
	}
	got := group(all, envTag("BOX_NAME", "BOX_DIR"))
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	s := got[0]
	if s.PID != 11 || s.Name != "claude" || s.Dir != "/work" || s.Procs != 3 || s.CPU != 3*time.Second ||
		s.RSS != 150 || !slices.Equal(s.Command, []string{"claude", "-c"}) {
		t.Errorf("unexpected sandbox %+v", s)
	}
}

func TestRunning(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 100, 1, "bwrap", 0, 0, 0, 0, "box:claude", "--", "claude")
	writeProc(t, root, 200, 1, "bwrap", 0, 0, 0, 0, "bwrap", "--", "flatpak")
	for _, tc := range []struct {
		pid  int
		name string
		want bool
	}{
		{100, "claude", true},
		{100, "sh", false},
		{200, "flatpak", false},
		{300, "claude", false},
	} {
		if got := Running(root, "box:", tc.pid, tc.name); got != tc.want {
			t.Errorf("Running(%d, %q) = %v, want %v", tc.pid, tc.name, got, tc.want)
		}
	}
}
