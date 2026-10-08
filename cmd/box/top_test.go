package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deric/box/internal/procs"
)

func TestTopUpdateRates(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := newTopState(time.Second, "", nil)
	s.totalMem = 0
	// First sample: average over the lifetime (2s of CPU in 10s).
	s.update([]procs.Sandbox{
		{PID: 10, Name: "claude", CPU: 2 * time.Second, Started: t0.Add(-10 * time.Second)},
	}, nil, t0)
	if got := s.rows[0].cpuPct; got < 19.9 || got > 20.1 {
		t.Errorf("lifetime cpu%% = %.2f, want 20", got)
	}
	// Second sample a second later: 500ms more CPU is 50%.
	s.update([]procs.Sandbox{
		{PID: 10, Name: "claude", CPU: 2500 * time.Millisecond, Started: t0.Add(-10 * time.Second)},
	}, nil, t0.Add(time.Second))
	if got := s.rows[0].cpuPct; got < 49.9 || got > 50.1 {
		t.Errorf("delta cpu%% = %.2f, want 50", got)
	}
	// Reused PID with a different start time falls back to the lifetime average.
	s.update([]procs.Sandbox{
		{PID: 10, Name: "sh", CPU: time.Second, Started: t0.Add(-2 * time.Second)},
	}, nil, t0.Add(2*time.Second))
	if got := s.rows[0].cpuPct; got < 24.9 || got > 25.1 {
		t.Errorf("reused pid cpu%% = %.2f, want 25", got)
	}
	// An error keeps the rows and is shown.
	s.update(nil, errors.New("boom"), t0.Add(3*time.Second))
	lines, _ := s.lines(80, 10)
	if len(s.rows) != 1 || !strings.Contains(lines[2], "error: boom") {
		t.Errorf("after error: rows=%d lines[2]=%q", len(s.rows), lines[2])
	}
}

func TestTopFilter(t *testing.T) {
	s := newTopState(time.Second, "claude", nil)
	now := time.Now()
	s.update([]procs.Sandbox{
		{PID: 1, Name: "claude", Started: now},
		{PID: 2, Name: "sh", Started: now},
	}, nil, now)
	if len(s.rows) != 1 || s.rows[0].Name != "claude" {
		t.Errorf("filter kept %v", s.rows)
	}
	lines, _ := s.lines(80, 10)
	if !strings.Contains(lines[0], "name: claude") {
		t.Errorf("summary = %q", lines[0])
	}
}

func TestTopSort(t *testing.T) {
	now := time.Now()
	s := newTopState(time.Second, "", nil)
	list := []procs.Sandbox{
		{PID: 1, Name: "b", Procs: 3, RSS: 100, CPU: 10 * time.Second, Started: now.Add(-100 * time.Second)},
		{PID: 2, Name: "a", Procs: 5, RSS: 300, CPU: 1 * time.Second, Started: now.Add(-10 * time.Second)},
		{PID: 3, Name: "c", Procs: 1, RSS: 200, CPU: 50 * time.Second, Started: now.Add(-100 * time.Second)},
	}
	s.update(list, nil, now)
	pids := func() []int {
		out := make([]int, len(s.rows))
		for i, r := range s.rows {
			out[i] = r.PID
		}
		return out
	}
	eq := func(a, b []int) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for _, tc := range []struct {
		key  byte
		want []int
	}{
		{'c', []int{3, 1, 2}},
		{'m', []int{2, 3, 1}},
		{'p', []int{2, 1, 3}},
		{'t', []int{1, 3, 2}},
		{'n', []int{2, 1, 3}},
	} {
		s.reverse = false
		if redraw, retick := s.key(tc.key); !redraw || retick {
			t.Errorf("key %q = redraw %v, retick %v", tc.key, redraw, retick)
		}
		if got := pids(); !eq(got, tc.want) {
			t.Errorf("sort %q = %v, want %v", tc.key, got, tc.want)
		}
		s.key('r')
		want := make([]int, len(tc.want))
		for i, p := range tc.want {
			want[len(want)-1-i] = p
		}
		if got := pids(); !eq(got, want) {
			t.Errorf("reversed sort %q = %v, want %v", tc.key, got, want)
		}
	}
	if redraw, retick := s.key('x'); redraw || retick {
		t.Errorf("unknown key = redraw %v, retick %v", redraw, retick)
	}
}

func TestTopInterval(t *testing.T) {
	s := newTopState(time.Second, "", nil)
	s.key('+')
	s.key('+')
	if s.interval != 4*time.Second {
		t.Errorf("interval after ++ = %s", s.interval)
	}
	for range 10 {
		s.key('-')
	}
	if s.interval != topMinInterval {
		t.Errorf("interval after many - = %s", s.interval)
	}
	for range 20 {
		s.key('+')
	}
	if s.interval != topMaxInterval {
		t.Errorf("interval after many + = %s", s.interval)
	}
}

func TestTopLines(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := newTopState(time.Second, "", nil)
	s.totalMem = 1 << 30
	var list []procs.Sandbox
	for i := 1; i <= 5; i++ {
		list = append(list, procs.Sandbox{
			PID: 1000 + i, Name: "claude", Procs: i, RSS: uint64(i) << 20,
			Dir:     "/home/deric/dev/some/very/long/path/to/a/project/directory",
			Command: []string{"claude", "--resume", strings.Repeat("x", 100)},
			Started: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	s.update(list, nil, now)
	width, height := 60, 12
	lines, headers := s.lines(width, height)
	if len(lines) != height {
		t.Fatalf("got %d lines, want %d", len(lines), height)
	}
	for i, l := range lines {
		if n := len([]rune(l)); n > width {
			t.Errorf("line %d is %d cells wide: %q", i, n, l)
		}
	}
	if len(headers) != 2 || headers[0].line != 1 || !strings.HasPrefix(lines[1], "    PID  NAME") {
		t.Errorf("headers = %v, lines[1] = %q", headers, lines[1])
	}
	if h := headers[0]; lines[1][h.from:h.to] != "CPU%" {
		t.Errorf("sort column = %q (%d:%d)", lines[1][h.from:h.to], h.from, h.to)
	}
	// Five rows fit; the denied pane starts right below them with its header.
	if h := headers[1]; h.line != 7 || h.from != 0 || h.to != 0 || !strings.HasPrefix(lines[7], "DENIED  LAST      SANDBOX  TARGET") {
		t.Errorf("denied header = %v, lines[7] = %q", h, lines[7])
	}
	if lines[8] != "no denied connections" || lines[9] != "" || lines[10] != "" {
		t.Errorf("denied pane = %q", lines[8:11])
	}
	if lines[height-1] != clip(topKeys, width) {
		t.Errorf("last line = %q", lines[height-1])
	}

	// A wide terminal shows the whole summary and the end of the directory.
	lines, _ = s.lines(200, height)
	if !strings.Contains(lines[0], "sandboxes: 5  procs: 15  cpu: 0.0%  mem: 15.0MiB (1.5%)  denied: 0  interval: 1s") {
		t.Errorf("summary = %q", lines[0])
	}
	if strings.Contains(lines[0], "showing") {
		t.Errorf("summary mentions showing with all rows visible: %q", lines[0])
	}
	if !strings.Contains(lines[2], "…ong/path/to/a/project/directory  claude --resume") {
		t.Errorf("dir not clipped from the left: %q", lines[2])
	}

	// A short terminal shows fewer rows and says so; the denied pane keeps
	// its header and one line.
	s.key('t')
	lines, headers = s.lines(200, 6)
	if len(lines) != 6 || !strings.Contains(lines[0], "(showing 1)") {
		t.Errorf("short terminal: %d lines, summary %q", len(lines), lines[0])
	}
	if !strings.Contains(lines[2], "1005  claude") || !strings.Contains(lines[2], "5m00s") {
		t.Errorf("first row = %q", lines[2])
	}
	if len(headers) != 2 || headers[1].line != 3 || lines[4] != "no denied connections" {
		t.Errorf("short terminal denied pane: headers %v, lines %q", headers, lines[3:])
	}

	s.update(nil, nil, now)
	lines, _ = s.lines(80, 5)
	if lines[2] != "no running sandboxes" {
		t.Errorf("empty = %q", lines[2])
	}
}

func TestSplitHeight(t *testing.T) {
	for _, tc := range []struct {
		avail, topNeed, botNeed int
		top, bot                int
	}{
		{23, 4, 3, 4, 19},    // both small: each gets what it needs, bottom the slack
		{23, 30, 3, 20, 3},   // many sandboxes, few denials
		{23, 4, 30, 4, 19},   // few sandboxes, many denials
		{23, 30, 30, 11, 12}, // both big: halves
		{4, 3, 2, 3, 1},      // tiny: the top keeps three lines
		{2, 3, 2, 2, 0},
		{0, 3, 2, 0, 0},
	} {
		top, bot := splitHeight(tc.avail, tc.topNeed, tc.botNeed)
		if top != tc.top || bot != tc.bot {
			t.Errorf("splitHeight(%d, %d, %d) = %d, %d; want %d, %d", tc.avail, tc.topNeed, tc.botNeed, top, bot, tc.top, tc.bot)
		}
	}
}

func TestTopDeniedPane(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("claude-1001-home-deric-dev-box.log", strings.Join([]string{
		"2026/10/07 11:59:00 denied CONNECT telemetry.example:443",
		"2026/10/07 11:59:30 denied GET http://telemetry.example/v1/ping",
		"2026/10/07 11:59:40 CONNECT github.com:443: dial tcp: timeout",
		"2026/10/07 11:59:50 denied CONNECT [2001:db8::1]:443",
		"",
	}, "\n"))
	write("sh-1002-tmp.log", "2026/10/07 11:58:00 denied CONNECT telemetry.example:443\n")
	write("sh-1003-tmp.log", "2026/10/07 11:58:00 denied CONNECT exited.example:443\n") // not running

	s := newTopState(time.Second, "", []string{dir})
	s.totalMem = 0
	s.update([]procs.Sandbox{
		{PID: 1001, Name: "claude", Started: now.Add(-time.Minute)},
		{PID: 1002, Name: "sh", Started: now.Add(-time.Minute)},
	}, nil, now)
	s.updateDenied()
	lines, headers := s.lines(100, 12)
	if !strings.Contains(lines[0], "sandboxes: 2") || !strings.Contains(lines[0], "denied: 4") {
		t.Errorf("summary = %q", lines[0])
	}
	if len(headers) != 2 || headers[1].line != 4 {
		t.Fatalf("headers = %v", headers)
	}
	if lines[4] != "DENIED  LAST      SANDBOX    TARGET" {
		t.Errorf("denied header = %q", lines[4])
	}
	want := []string{
		"     3  11:59:30  claude,sh  telemetry.example",
		"     1  11:59:50  claude     2001:db8::1",
	}
	for i, w := range want {
		if lines[5+i] != w {
			t.Errorf("denied row %d = %q, want %q", i, lines[5+i], w)
		}
	}
	if lines[7] != "" {
		t.Errorf("line after denied rows = %q", lines[7])
	}

	// More denials appended to a log are picked up; a sandbox that exits
	// takes its denials with it.
	f, err := os.OpenFile(filepath.Join(dir, "sh-1002-tmp.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := f.WriteString("2026/10/07 12:00:01 denied CONNECT new.example:443\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	s.updateDenied()
	// Equal counts are ordered by the most recent denial.
	if rows := s.denied.rows(); len(rows) != 3 || rows[0].target != "new.example" || rows[0].count != 3 || rows[1].target != "telemetry.example" {
		t.Errorf("after append: %+v", rows)
	}
	s.update([]procs.Sandbox{{PID: 1001, Name: "claude", Started: now.Add(-time.Minute)}}, nil, now)
	s.updateDenied()
	rows := s.denied.rows()
	if len(rows) != 2 || rows[0].count != 2 || rows[0].sandboxNames() != "claude" || s.denied.total() != 3 {
		t.Errorf("after sh exited: %+v total %d", rows, s.denied.total())
	}

	// With more targets than lines the rest is counted.
	s.update(nil, nil, now)
	s.updateDenied()
	tail := &logTail{name: "x"}
	for i := range 10 {
		for range 10 - i {
			tail.add(fmt.Sprintf("2026/10/07 12:00:00 denied CONNECT host%d.example:443", i))
		}
	}
	s.denied.tails["x"] = tail
	lines, _ = s.lines(100, 8)
	if lines[3] != "DENIED  LAST      SANDBOX  TARGET" || !strings.HasPrefix(lines[4], "    10  12:00:00  x        host0.example") || lines[6] != "… 8 more targets" {
		t.Errorf("clipped denied pane = %q", lines[3:7])
	}
}

func TestParseDenied(t *testing.T) {
	for _, tc := range []struct {
		line   string
		target string
		ok     bool
	}{
		{"2026/10/07 11:59:00 denied CONNECT telemetry.example:443", "telemetry.example", true},
		{"2026/10/07 11:59:00 denied GET http://telemetry.example:8080/v1/ping", "telemetry.example", true},
		{"2026/10/07 11:59:00 denied POST https://api.example/x", "api.example", true},
		{"2026/10/07 11:59:00 denied CONNECT [::1]:443", "::1", true},
		{"2026/10/07 11:59:00 denied CONNECT noport", "noport", true},
		{"2026/10/07 11:59:00 CONNECT github.com:443: dial tcp: timeout", "", false},
		{"2026/10/07 11:59:00 GET http://x/: EOF", "", false},
		{"garbage", "", false},
		{"", "", false},
	} {
		at, target, ok := parseDenied(tc.line)
		if target != tc.target || ok != tc.ok {
			t.Errorf("parseDenied(%q) = %q, %v; want %q, %v", tc.line, target, ok, tc.target, tc.ok)
		}
		if ok && !at.Equal(time.Date(2026, 10, 7, 11, 59, 0, 0, time.Local)) {
			t.Errorf("parseDenied(%q) at = %s", tc.line, at)
		}
	}
}

func TestLogTailTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-1-x.log")
	line := "2026/10/07 11:59:00 denied CONNECT a.example:443\n"
	if err := os.WriteFile(path, []byte(line+line+"2026/10/07 11:59:00 denied CONNECT partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := &logTail{name: "claude"}
	if err := tail.read(path); err != nil {
		t.Fatal(err)
	}
	if len(tail.counts) != 1 || tail.counts["a.example"].count != 2 {
		t.Errorf("counts = %+v (the unfinished line must wait)", tail.counts)
	}
	// Finishing the line completes it on the next read.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(".example:443\n")
	_ = f.Close()
	if err := tail.read(path); err != nil {
		t.Fatal(err)
	}
	if tail.counts["partial.example"] == nil || tail.counts["partial.example"].count != 1 {
		t.Errorf("counts = %+v", tail.counts)
	}
	// A shrunken file is counted afresh.
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tail.read(path); err != nil {
		t.Fatal(err)
	}
	if len(tail.counts) != 1 || tail.counts["a.example"].count != 1 {
		t.Errorf("after truncation counts = %+v", tail.counts)
	}
	// A vanished file is an error that is reported but not fatal.
	_ = os.Remove(path)
	var d deniedLogs
	d.update(map[string]string{path: "claude"})
	if d.err == nil || len(d.rows()) != 0 {
		t.Errorf("missing log: err %v rows %v", d.err, d.rows())
	}
}

func TestDeniedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"claude-1001-home-deric-dev-box.log", "claude-1002-home.log", "sh-1001-tmp.log",
		"claude-1001.sock", "notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "claude-1001-dir.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := deniedFiles([]string{dir, filepath.Join(dir, "missing")}, []procs.Sandbox{{PID: 1001, Name: "claude"}})
	if len(files) != 1 || files[filepath.Join(dir, "claude-1001-home-deric-dev-box.log")] != "claude" {
		t.Errorf("files = %v", files)
	}
}

func TestClip(t *testing.T) {
	for _, tc := range []struct{ in, want, wantLeft string }{
		{"abc", "abc", "abc"},
		{"abcdef", "abc…", "…def"},
		{"ěščřž", "ěšč…", "…čřž"},
	} {
		if got := clip(tc.in, 4); got != tc.want {
			t.Errorf("clip(%q, 4) = %q, want %q", tc.in, got, tc.want)
		}
		if got := clipLeft(tc.in, 4); got != tc.wantLeft {
			t.Errorf("clipLeft(%q, 4) = %q, want %q", tc.in, got, tc.wantLeft)
		}
	}
	if got := clip("abc", 1); got != "…" {
		t.Errorf("clip to 1 = %q", got)
	}
	if got := clip("abc", 0); got != "" {
		t.Errorf("clip to 0 = %q", got)
	}
}
