package main

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deric/box/internal/procs"
)

// logTimeLayout is the timestamp log.LstdFlags writes, in local time.
const logTimeLayout = "2006/01/02 15:04:05"

// denial sums the requests the proxy denied to one target host.
type denial struct {
	target string
	count  int
	last   time.Time
	names  map[string]bool // sandboxes that tried
}

// logTail follows one proxy log file, counting its denied requests per
// target. Only complete lines are consumed; a line still being written is
// read on the next pass.
type logTail struct {
	name   string // sandbox the log belongs to
	offset int64
	counts map[string]*denial
}

// read consumes what was appended to path since the last call. A file that
// shrank is counted again from the start.
func (t *logTail) read(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < t.offset {
		t.offset, t.counts = 0, nil
	}
	if st.Size() == t.offset {
		return nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil
		}
		t.offset += int64(len(line))
		t.add(strings.TrimRight(line, "\r\n"))
	}
}

// add counts one log line if it records a denied request.
func (t *logTail) add(line string) {
	at, target, ok := parseDenied(line)
	if !ok {
		return
	}
	if t.counts == nil {
		t.counts = map[string]*denial{}
	}
	d := t.counts[target]
	if d == nil {
		d = &denial{target: target}
		t.counts[target] = d
	}
	d.count++
	if at.After(d.last) {
		d.last = at
	}
}

// parseDenied reads a proxy log line of the form
// "2006/01/02 15:04:05 denied METHOD WHAT", where WHAT is host:port for
// CONNECT and an absolute URL otherwise, and returns when it happened and
// the target host. Other lines (failed requests, anything unexpected) are
// not denials.
func parseDenied(line string) (at time.Time, target string, ok bool) {
	f := strings.Fields(line)
	if len(f) < 5 || f[2] != "denied" {
		return time.Time{}, "", false
	}
	at, _ = time.ParseInLocation(logTimeLayout, f[0]+" "+f[1], time.Local)
	return at, deniedTarget(f[3], f[4]), true
}

// deniedTarget is the host a denied request was for: the host of a CONNECT
// host:port, or of an absolute URL. The port is dropped, as the allowlist
// is by host.
func deniedTarget(method, what string) string {
	if method == "CONNECT" {
		if host, _, err := net.SplitHostPort(what); err == nil {
			return host
		}
		return what
	}
	if u, err := url.Parse(what); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return what
}

// deniedLogs follows the proxy logs of the sandboxes shown in the top view
// and aggregates their denied requests by target.
type deniedLogs struct {
	tails map[string]*logTail // by log path
	err   error
}

// update follows exactly the logs in files (path to sandbox name): new ones
// are picked up, vanished ones dropped along with what they counted, and
// the rest are read on. The first error reading a log is kept for display.
func (d *deniedLogs) update(files map[string]string) {
	if d.tails == nil {
		d.tails = map[string]*logTail{}
	}
	for path := range d.tails {
		if _, ok := files[path]; !ok {
			delete(d.tails, path)
		}
	}
	d.err = nil
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		t := d.tails[path]
		if t == nil {
			t = &logTail{name: files[path]}
			d.tails[path] = t
		}
		if err := t.read(path); err != nil && d.err == nil {
			d.err = err
		}
	}
}

// rows merges the counts of all logs, most denied target first, then the
// most recently denied, then by name.
func (d *deniedLogs) rows() []denial {
	byTarget := map[string]*denial{}
	for _, t := range d.tails {
		for target, c := range t.counts {
			m := byTarget[target]
			if m == nil {
				m = &denial{target: target, names: map[string]bool{}}
				byTarget[target] = m
			}
			m.count += c.count
			if c.last.After(m.last) {
				m.last = c.last
			}
			m.names[t.name] = true
		}
	}
	out := make([]denial, 0, len(byTarget))
	for _, m := range byTarget {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.count != b.count {
			return a.count > b.count
		}
		if !a.last.Equal(b.last) {
			return a.last.After(b.last)
		}
		return a.target < b.target
	})
	return out
}

// total is the number of denied requests over all logs.
func (d *deniedLogs) total() int {
	n := 0
	for _, t := range d.tails {
		for _, c := range t.counts {
			n += c.count
		}
	}
	return n
}

// sandboxNames lists the sandboxes of a merged denial, sorted and joined.
func (d denial) sandboxNames() string {
	names := make([]string, 0, len(d.names))
	for n := range d.names {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// deniedFiles finds the proxy logs of the given sandboxes in logDirs,
// keyed by path with the sandbox name as value. A log is
// <name>-<pid>-<dir>.log (see sandbox.ProxyFiles); it belongs to a sandbox
// when name and PID match. Unreadable directories are skipped.
func deniedFiles(logDirs []string, list []procs.Sandbox) map[string]string {
	running := map[int]string{}
	for _, sb := range list {
		running[sb.PID] = sb.Name
	}
	files := map[string]string{}
	for _, dir := range logDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || filepath.Ext(name) != ".log" {
				continue
			}
			bin, pid, ok := splitEntry(strings.TrimSuffix(name, ".log"), true)
			if !ok || running[pid] != bin {
				continue
			}
			files[filepath.Join(dir, name)] = bin
		}
	}
	return files
}
