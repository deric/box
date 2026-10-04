package procs

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
	"time"
)

// parsePS reads the output of `ps -axo pid=,ppid=,rss=,time=,etime=` as
// printed on macOS: rss in KiB, time and etime as [dd-][hh:]mm:ss[.cc].
func parsePS(out []byte, now time.Time) map[int]*proc {
	all := map[int]*proc{}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseUint(f[2], 10, 64)
		cpu, ok1 := parseClock(f[3])
		elapsed, ok2 := parseClock(f[4])
		if err1 != nil || err2 != nil || err3 != nil || !ok1 || !ok2 {
			continue
		}
		all[pid] = &proc{pid: pid, ppid: ppid, rss: rss * 1024, cpu: cpu, started: now.Add(-elapsed)}
	}
	return all
}

// parseClock parses a ps(1) duration, [dd-][hh:]mm:ss[.cc]. Minutes may
// exceed 59 when there is no hours field.
func parseClock(s string) (time.Duration, bool) {
	var d time.Duration
	if days, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, false
		}
		d, s = time.Duration(n)*24*time.Hour, rest
	}
	var secs float64
	for part := range strings.SplitSeq(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + v
	}
	return d + time.Duration(secs*float64(time.Second)), true
}

// parseProcArgs decodes a kern.procargs2 buffer: a native-endian int32 argc,
// the executable path and its NUL padding, argc arguments, then environment
// entries up to an empty string. Every string is NUL-terminated.
func parseProcArgs(buf []byte) (argv, env []string) {
	if len(buf) < 4 {
		return nil, nil
	}
	argc := int(binary.NativeEndian.Uint32(buf))
	rest := buf[4:]
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = bytes.TrimLeft(rest[i:], "\x00")
	} else {
		return nil, nil
	}
	next := func() (string, bool) {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return "", false
		}
		s := string(rest[:i])
		rest = rest[i+1:]
		return s, true
	}
	for len(argv) < argc {
		s, ok := next()
		if !ok {
			return argv, nil
		}
		argv = append(argv, s)
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			return argv, env
		}
		env = append(env, s)
	}
}

// envTag tags processes by the environment variables box sets inside macOS
// sandboxes: nameKey holds the sandbox name and dirKey its directory.
func envTag(nameKey, dirKey string) func(*proc) (Sandbox, bool) {
	return func(p *proc) (Sandbox, bool) {
		s := Sandbox{Command: p.argv}
		found := false
		for _, kv := range p.env {
			switch k, v, _ := strings.Cut(kv, "="); k {
			case nameKey:
				s.Name, found = v, true
			case dirKey:
				s.Dir = v
			}
		}
		return s, found
	}
}
