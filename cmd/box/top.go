package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"box/internal/procs"
)

// Sort orders of the top view, chosen with the keys listed in topKeys.
type sortKey int

const (
	sortCPU sortKey = iota
	sortMem
	sortProcs
	sortUptime
	sortName
)

const (
	topMinInterval = 250 * time.Millisecond
	topMaxInterval = time.Minute
	topKeys        = "q quit  c cpu  m mem  p procs  t uptime  n name  r reverse  +/- interval"
)

// topRow is one sandbox with the CPU usage rate derived from two samples.
type topRow struct {
	procs.Sandbox
	cpuPct float64 // CPU time per wall-clock time, in percent of one core
}

// topSample remembers the CPU time of a sandbox at the previous refresh.
type topSample struct {
	cpu     time.Duration
	started time.Time // detects a PID reused by a new sandbox
	at      time.Time
}

// topState holds everything the top view renders and the previous sample
// it computes rates from.
type topState struct {
	rows     []topRow
	prev     map[int]topSample
	sort     sortKey
	reverse  bool
	interval time.Duration
	totalMem uint64
	filter   string
	now      time.Time
	err      error
}

func newTopState(interval time.Duration, filter string) *topState {
	return &topState{
		prev:     map[int]topSample{},
		interval: interval,
		totalMem: procs.TotalMemory(),
		filter:   filter,
	}
}

// update replaces the rows with a fresh listing taken at now. A sandbox seen
// before gets the rate over the interval since; a new one the average over
// its lifetime.
func (s *topState) update(list []procs.Sandbox, err error, now time.Time) {
	s.now, s.err = now, err
	if err != nil {
		return
	}
	next := make(map[int]topSample, len(list))
	s.rows = s.rows[:0]
	for _, sb := range list {
		if s.filter != "" && sb.Name != s.filter {
			continue
		}
		var pct float64
		if p, ok := s.prev[sb.PID]; ok && p.started.Equal(sb.Started) {
			pct = cpuPercent(sb.CPU-p.cpu, now.Sub(p.at))
		} else {
			pct = cpuPercent(sb.CPU, now.Sub(sb.Started))
		}
		next[sb.PID] = topSample{cpu: sb.CPU, started: sb.Started, at: now}
		s.rows = append(s.rows, topRow{Sandbox: sb, cpuPct: pct})
	}
	s.prev = next
	s.sortRows()
}

// cpuPercent is CPU time per elapsed wall-clock time as a percentage of one
// core; negative inputs (a sandbox that was replaced mid-sample) yield 0.
func cpuPercent(cpu, elapsed time.Duration) float64 {
	if cpu <= 0 || elapsed <= 0 {
		return 0
	}
	return 100 * float64(cpu) / float64(elapsed)
}

// sortRows orders rows by the current key, biggest first for numeric keys
// and alphabetically for names, with PID as the tie-breaker.
func (s *topState) sortRows() {
	less := func(a, b topRow) bool {
		switch s.sort {
		case sortMem:
			if a.RSS != b.RSS {
				return a.RSS > b.RSS
			}
		case sortProcs:
			if a.Procs != b.Procs {
				return a.Procs > b.Procs
			}
		case sortUptime:
			if !a.Started.Equal(b.Started) {
				return a.Started.Before(b.Started)
			}
		case sortName:
			if a.Name != b.Name {
				return a.Name < b.Name
			}
		default:
			if a.cpuPct != b.cpuPct {
				return a.cpuPct > b.cpuPct
			}
		}
		return a.PID < b.PID
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		if s.reverse {
			return less(s.rows[j], s.rows[i])
		}
		return less(s.rows[i], s.rows[j])
	})
}

// key applies a key press; it reports whether the view should be redrawn
// and whether the refresh interval changed.
func (s *topState) key(k byte) (redraw, retick bool) {
	switch k {
	case 'c':
		s.sort = sortCPU
	case 'm':
		s.sort = sortMem
	case 'p':
		s.sort = sortProcs
	case 't':
		s.sort = sortUptime
	case 'n':
		s.sort = sortName
	case 'r':
		s.reverse = !s.reverse
	case '+':
		s.interval = min(s.interval*2, topMaxInterval)
		return true, true
	case '-':
		s.interval = max(s.interval/2, topMinInterval)
		return true, true
	default:
		return false, false
	}
	s.sortRows()
	return true, false
}

// topColumn describes one column of the table: a header, its width and
// whether it is right-aligned. The last column takes the remaining width.
type topColumn struct {
	name  string
	width int
	right bool
	key   sortKey
	sorts bool
}

// lines renders the view as exactly height plain-text lines of at most width
// cells: a summary, the column header, the rows, padding and the key help.
// The index of the header line is returned so the caller can highlight it,
// along with the cell range of the sort column within it.
func (s *topState) lines(width, height int) (lines []string, header int, sortFrom, sortTo int) {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	body := max(height-3, 1)
	shown := min(len(s.rows), body)

	lines = append(lines, clip(s.summary(shown), width))
	cols := s.columns()
	hdr, from, to := s.header(cols)
	header = len(lines)
	lines = append(lines, clip(hdr, width))
	sortFrom, sortTo = min(from, width), min(to, width)

	switch {
	case s.err != nil:
		lines = append(lines, clip("error: "+s.err.Error(), width))
	case len(s.rows) == 0:
		msg := "no running sandboxes"
		if s.filter != "" {
			msg = "no running " + s.filter + " sandboxes"
		}
		lines = append(lines, msg)
	default:
		for _, r := range s.rows[:shown] {
			lines = append(lines, clip(s.row(cols, r, width), width))
		}
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	if len(lines) < height {
		lines = append(lines, clip(topKeys, width))
	}
	return lines[:min(len(lines), height)], header, sortFrom, sortTo
}

// summary is the first line: the time and totals over the listed sandboxes.
func (s *topState) summary(shown int) string {
	var procsN int
	var cpu float64
	var rss uint64
	for _, r := range s.rows {
		procsN += r.Procs
		cpu += r.cpuPct
		rss += r.RSS
	}
	b := new(strings.Builder)
	fmt.Fprintf(b, "box top - %s", s.now.Format("15:04:05"))
	if s.filter != "" {
		fmt.Fprintf(b, "  name: %s", s.filter)
	}
	fmt.Fprintf(b, "  sandboxes: %d", len(s.rows))
	if shown < len(s.rows) {
		fmt.Fprintf(b, " (showing %d)", shown)
	}
	fmt.Fprintf(b, "  procs: %d  cpu: %.1f%%  mem: %s", procsN, cpu, formatBytes(rss))
	if s.totalMem > 0 {
		fmt.Fprintf(b, " (%.1f%%)", 100*float64(rss)/float64(s.totalMem))
	}
	fmt.Fprintf(b, "  interval: %s", s.interval)
	return b.String()
}

func (s *topState) columns() []topColumn {
	nameW := 4
	for _, r := range s.rows {
		nameW = max(nameW, utf8.RuneCountInString(r.Name))
	}
	cols := []topColumn{
		{name: "PID", width: 7, right: true},
		{name: "NAME", width: min(nameW, 20), key: sortName, sorts: true},
		{name: "PROCS", width: 5, right: true, key: sortProcs, sorts: true},
		{name: "CPU%", width: 6, right: true, key: sortCPU, sorts: true},
		{name: "MEM", width: 8, right: true, key: sortMem, sorts: true},
	}
	if s.totalMem > 0 {
		cols = append(cols, topColumn{name: "MEM%", width: 5, right: true})
	}
	return append(cols,
		topColumn{name: "UPTIME", width: 7, right: true, key: sortUptime, sorts: true},
		topColumn{name: "DIR", width: 32},
		topColumn{name: "COMMAND"},
	)
}

// header renders the column titles and returns the cell range occupied by
// the sort column's title.
func (s *topState) header(cols []topColumn) (line string, from, to int) {
	cells := make([]string, len(cols))
	for i, c := range cols {
		cells[i] = c.name
	}
	line, starts := joinCells(cols, cells)
	for i, c := range cols {
		if c.sorts && c.key == s.sort {
			from = starts[i]
			if c.right {
				from += c.width - len(c.name)
			}
			return line, from, from + len(c.name)
		}
	}
	return line, 0, 0
}

func (s *topState) row(cols []topColumn, r topRow, width int) string {
	cells := []string{
		fmt.Sprint(r.PID),
		r.Name,
		fmt.Sprint(r.Procs),
		fmt.Sprintf("%.1f", r.cpuPct),
		formatBytes(r.RSS),
	}
	if s.totalMem > 0 {
		cells = append(cells, fmt.Sprintf("%.1f", 100*float64(r.RSS)/float64(s.totalMem)))
	}
	cells = append(cells,
		formatDuration(s.now.Sub(r.Started)),
		clipLeft(r.Dir, cols[len(cols)-2].width),
		strings.Join(r.Command, " "),
	)
	line, _ := joinCells(cols, cells)
	return line
}

// joinCells lays cells out in their columns, two spaces apart, and returns
// the line and the start cell of every column. The last column is not
// padded.
func joinCells(cols []topColumn, cells []string) (string, []int) {
	b := new(strings.Builder)
	starts := make([]int, len(cols))
	pos := 0
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
			pos += 2
		}
		starts[i] = pos
		cell := cells[i]
		if i == len(cols)-1 {
			b.WriteString(cell)
			break
		}
		cell = clip(cell, c.width)
		if c.right {
			fmt.Fprintf(b, "%*s", c.width, cell)
		} else {
			fmt.Fprintf(b, "%-*s", c.width, cell)
		}
		pos += c.width
	}
	return b.String(), starts
}

// clip shortens s to at most n cells, marking a cut with "…".
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return strings.Repeat("…", n)
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// clipLeft shortens s to at most n cells keeping its end, as paths are told
// apart by their last components.
func clipLeft(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return strings.Repeat("…", n)
	}
	r := []rune(s)
	return "…" + string(r[len(r)-n+1:])
}

// draw writes one frame: the cursor goes home, every line is written and
// cleared to its end, the header line in reverse video with the sort column
// in bold, the key help dimmed, and whatever is left below is cleared. No
// newline follows the last line so the screen never scrolls.
func (s *topState) draw(w *bufio.Writer, width, height int) error {
	lines, header, from, to := s.lines(width, height)
	b := new(strings.Builder)
	b.WriteString("\x1b[H")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		switch {
		case i == header:
			r := []rune(line)
			b.WriteString("\x1b[7m")
			b.WriteString(string(r[:from]))
			b.WriteString("\x1b[27m\x1b[1m")
			b.WriteString(string(r[from:to]))
			b.WriteString("\x1b[22m\x1b[7m")
			b.WriteString(string(r[to:]))
			b.WriteString("\x1b[K\x1b[0m")
		case i == len(lines)-1 && line == topKeys:
			b.WriteString("\x1b[2m" + line + "\x1b[K\x1b[0m")
		default:
			b.WriteString(line + "\x1b[K")
		}
	}
	b.WriteString("\x1b[J")
	if _, err := w.WriteString(b.String()); err != nil {
		return err
	}
	return w.Flush()
}

// cmdTop shows the running sandboxes in a full-screen view that refreshes
// every interval, like top(1): one row per sandbox with its process count,
// CPU usage rate, resident memory and uptime.
func cmdTop(argv []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	var interval time.Duration
	fs.DurationVar(&interval, "i", time.Second, "refresh interval")
	fs.DurationVar(&interval, "interval", time.Second, "refresh interval")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if interval < topMinInterval || interval > topMaxInterval {
		return fmt.Errorf("top: interval must be between %s and %s", topMinInterval, topMaxInterval)
	}
	var filter string
	if fs.NArg() > 0 {
		filter = filepath.Base(fs.Arg(0))
	}
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !isTerminal(in) || !isTerminal(out) {
		return fmt.Errorf("top: a terminal is required (use `box ps` for a one-off listing)")
	}
	restore, err := rawMode(in)
	if err != nil {
		return fmt.Errorf("top: %w", err)
	}
	defer restore()
	w := bufio.NewWriter(os.Stdout)
	// Alternate screen, cursor hidden; undone on exit so the shell's
	// scrollback is left as it was.
	if _, err := w.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J"); err != nil {
		return err
	}
	defer func() {
		_, _ = w.WriteString("\x1b[?25h\x1b[?1049l")
		_ = w.Flush()
	}()

	keys := make(chan byte)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			if n == 1 {
				keys <- buf[0]
			}
		}
	}()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(sigs)
	defer signal.Stop(winch)

	st := newTopState(interval, filter)
	draw := func() error {
		width, height, err := termSize(out)
		if err != nil {
			width, height = 0, 0
		}
		return st.draw(w, width, height)
	}
	refresh := func() error {
		list, err := procs.Sandboxes()
		st.update(list, err, time.Now())
		return draw()
	}
	if err := refresh(); err != nil {
		return err
	}
	ticker := time.NewTicker(st.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := refresh(); err != nil {
				return err
			}
		case k, ok := <-keys:
			if !ok || k == 'q' || k == 'Q' || k == 3 /* ^C */ || k == 4 /* ^D */ {
				return nil
			}
			redraw, retick := st.key(k)
			if retick {
				ticker.Reset(st.interval)
			}
			if redraw {
				if err := draw(); err != nil {
					return err
				}
			}
		case <-winch:
			if err := draw(); err != nil {
				return err
			}
		case <-sigs:
			return nil
		}
	}
}
