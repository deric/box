package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/deric/box/internal/procs"
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

// topState holds everything the top view renders: the sandboxes in the
// upper pane with the previous sample their rates are computed from, and
// the requests the proxy denied them in the lower pane.
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
	denied   deniedLogs
	logDirs  []string // where the proxy logs of the rows are looked for
}

func newTopState(interval time.Duration, filter string, logDirs []string) *topState {
	return &topState{
		prev:     map[int]topSample{},
		interval: interval,
		totalMem: procs.TotalMemory(),
		filter:   filter,
		logDirs:  logDirs,
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

// updateDenied reads on in the proxy logs of the listed sandboxes.
func (s *topState) updateDenied() {
	list := make([]procs.Sandbox, len(s.rows))
	for i, r := range s.rows {
		list[i] = r.Sandbox
	}
	s.denied.update(deniedFiles(s.logDirs, list))
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

// topHeader marks a header line of the view and the cell range within it
// that is emphasized (the sort column of the sandbox table).
type topHeader struct {
	line, from, to int
}

// lines renders the view as exactly height plain-text lines of at most width
// cells, split into two panes: the sandboxes (a summary, the column header
// and the rows) above, the requests the proxy denied them grouped by target
// (a column header and the rows) below, and the key help on the last line.
// Each pane gets the lines it needs up to half the screen, or more when the
// other needs less. The header lines are returned for highlighting.
func (s *topState) lines(width, height int) (lines []string, headers []topHeader) {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	denied := s.denied.rows()
	topH, botH := splitHeight(height-1, 2+max(len(s.rows), 1), 1+max(len(denied), 1))

	shown := min(len(s.rows), max(topH-2, 0))
	lines = append(lines, clip(s.summary(shown), width))
	cols := s.columns()
	hdr, from, to := s.header(cols)
	headers = append(headers, topHeader{len(lines), min(from, width), min(to, width)})
	lines = append(lines, clip(hdr, width))
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
	for len(lines) < topH {
		lines = append(lines, "")
	}
	lines = lines[:min(len(lines), topH)]

	if botH > 0 {
		dcols := s.deniedColumns(denied)
		headers = append(headers, topHeader{line: len(lines)})
		lines = append(lines, clip(s.deniedHeader(dcols), width))
		shown := min(len(denied), botH-1)
		if shown < len(denied) {
			shown = max(botH-2, 0) // leave a line for the count of the rest
		}
		switch {
		case s.denied.err != nil:
			lines = append(lines, clip("error: "+s.denied.err.Error(), width))
		case len(denied) == 0:
			lines = append(lines, "no denied connections")
		default:
			for _, d := range denied[:shown] {
				lines = append(lines, clip(s.deniedRow(dcols, d), width))
			}
			if shown < len(denied) {
				lines = append(lines, clip(fmt.Sprintf("… %d more targets", len(denied)-shown), width))
			}
		}
		for len(lines) < topH+botH {
			lines = append(lines, "")
		}
		lines = lines[:min(len(lines), topH+botH)]
	}
	if len(lines) < height {
		lines = append(lines, clip(topKeys, width))
	}
	return lines, headers
}

// splitHeight divides avail lines between the two panes that would like
// topNeed and botNeed lines: the upper one gets what it needs up to half,
// or more when the lower one does not need its half, and at least three
// lines (summary, header, one row); the lower one gets the rest.
func splitHeight(avail, topNeed, botNeed int) (topH, botH int) {
	if avail <= 0 {
		return 0, 0
	}
	topH = min(topNeed, max(avail/2, avail-botNeed))
	topH = max(topH, min(3, avail))
	return topH, avail - topH
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
	fmt.Fprintf(b, "  denied: %d  interval: %s", s.denied.total(), s.interval)
	return b.String()
}

// deniedColumns describes the lower pane: the number of denied requests,
// when the last one happened, which sandboxes made them and the target host
// they were for, which takes the remaining width.
func (s *topState) deniedColumns(rows []denial) []topColumn {
	nameW := 7
	for _, d := range rows {
		nameW = max(nameW, utf8.RuneCountInString(d.sandboxNames()))
	}
	return []topColumn{
		{name: "DENIED", width: 6, right: true},
		{name: "LAST", width: 8},
		{name: "SANDBOX", width: min(nameW, 24)},
		{name: "TARGET"},
	}
}

func (s *topState) deniedHeader(cols []topColumn) string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		cells[i] = c.name
	}
	line, _ := joinCells(cols, cells)
	return line
}

func (s *topState) deniedRow(cols []topColumn, d denial) string {
	last := ""
	if !d.last.IsZero() {
		last = d.last.Format("15:04:05")
	}
	line, _ := joinCells(cols, []string{fmt.Sprint(d.count), last, d.sandboxNames(), d.target})
	return line
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
// cleared to its end, the header lines in reverse video with the sort column
// in bold, the key help dimmed, and whatever is left below is cleared. No
// newline follows the last line so the screen never scrolls.
func (s *topState) draw(w *bufio.Writer, width, height int) error {
	lines, headers := s.lines(width, height)
	hdr := map[int]topHeader{}
	for _, h := range headers {
		hdr[h.line] = h
	}
	b := new(strings.Builder)
	b.WriteString("\x1b[H")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		h, isHeader := hdr[i]
		switch {
		case isHeader:
			r := []rune(line)
			b.WriteString("\x1b[7m")
			b.WriteString(string(r[:h.from]))
			if h.to > h.from {
				b.WriteString("\x1b[27m\x1b[1m")
				b.WriteString(string(r[h.from:h.to]))
				b.WriteString("\x1b[22m\x1b[7m")
			}
			b.WriteString(string(r[h.to:]))
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
// CPU usage rate, resident memory and uptime, and below them the requests
// the proxy denied those sandboxes, grouped by target host and counted.
func cmdTop(argv []string) error {
	var c common
	fs := newFlagSet("top", &c)
	var interval time.Duration
	fs.DurationVar(&interval, "i", time.Second, "refresh interval")
	fs.DurationVar(&interval, "interval", time.Second, "refresh interval")
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
	cfg, err := c.load()
	if err != nil {
		return err
	}
	dirs, err := logDirs(cfg)
	if err != nil {
		return err
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

	st := newTopState(interval, filter, dirs)
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
		st.updateDenied()
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
