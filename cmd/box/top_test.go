package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"box/internal/procs"
)

func TestTopUpdateRates(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := newTopState(time.Second, "")
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
	lines, _, _, _ := s.lines(80, 10)
	if len(s.rows) != 1 || !strings.Contains(lines[2], "error: boom") {
		t.Errorf("after error: rows=%d lines[2]=%q", len(s.rows), lines[2])
	}
}

func TestTopFilter(t *testing.T) {
	s := newTopState(time.Second, "claude")
	now := time.Now()
	s.update([]procs.Sandbox{
		{PID: 1, Name: "claude", Started: now},
		{PID: 2, Name: "sh", Started: now},
	}, nil, now)
	if len(s.rows) != 1 || s.rows[0].Name != "claude" {
		t.Errorf("filter kept %v", s.rows)
	}
	lines, _, _, _ := s.lines(80, 10)
	if !strings.Contains(lines[0], "name: claude") {
		t.Errorf("summary = %q", lines[0])
	}
}

func TestTopSort(t *testing.T) {
	now := time.Now()
	s := newTopState(time.Second, "")
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
	s := newTopState(time.Second, "")
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
	s := newTopState(time.Second, "")
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
	width, height := 60, 8
	lines, header, from, to := s.lines(width, height)
	if len(lines) != height {
		t.Fatalf("got %d lines, want %d", len(lines), height)
	}
	for i, l := range lines {
		if n := len([]rune(l)); n > width {
			t.Errorf("line %d is %d cells wide: %q", i, n, l)
		}
	}
	if header != 1 || !strings.HasPrefix(lines[1], "    PID  NAME") {
		t.Errorf("header at %d: %q", header, lines[1])
	}
	if got := lines[1][from:to]; got != "CPU%" {
		t.Errorf("sort column = %q (%d:%d)", got, from, to)
	}
	if lines[height-1] != clip(topKeys, width) {
		t.Errorf("last line = %q", lines[height-1])
	}

	// A wide terminal shows the whole summary and the end of the directory.
	lines, _, _, _ = s.lines(200, height)
	if !strings.Contains(lines[0], "sandboxes: 5  procs: 15  cpu: 0.0%  mem: 15.0MiB (1.5%)  interval: 1s") {
		t.Errorf("summary = %q", lines[0])
	}
	if strings.Contains(lines[0], "showing") {
		t.Errorf("summary mentions showing with all rows visible: %q", lines[0])
	}
	if !strings.Contains(lines[2], "…ong/path/to/a/project/directory  claude --resume") {
		t.Errorf("dir not clipped from the left: %q", lines[2])
	}

	// A short terminal shows fewer rows and says so.
	s.key('t')
	lines, _, _, _ = s.lines(200, 6)
	if len(lines) != 6 || !strings.Contains(lines[0], "(showing 3)") {
		t.Errorf("short terminal: %d lines, summary %q", len(lines), lines[0])
	}
	if !strings.Contains(lines[2], "1005  claude") || !strings.Contains(lines[2], "5m00s") {
		t.Errorf("first row = %q", lines[2])
	}

	s.update(nil, nil, now)
	lines, _, _, _ = s.lines(80, 5)
	if lines[2] != "no running sandboxes" {
		t.Errorf("empty = %q", lines[2])
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
