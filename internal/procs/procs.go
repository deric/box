// Package procs finds running box sandboxes by scanning /proc.
package procs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of times in /proc/<pid>/stat. The kernel
// fixes it at 100 for userspace on all mainstream architectures.
const clockTicks = 100

// Sandbox summarizes one running sandbox and every process inside it.
type Sandbox struct {
	PID     int           // outermost bwrap process
	Name    string        // base name of the sandboxed binary
	Command []string      // command run inside the sandbox
	Dir     string        // working directory inside the sandbox
	Procs   int           // processes in the tree, bwrap included
	CPU     time.Duration // user+system time consumed by the tree
	RSS     uint64        // resident memory of the tree in bytes
	Started time.Time
}

type proc struct {
	pid     int
	ppid    int
	argv    []string
	cpu     uint64 // clock ticks, including reaped children
	rss     uint64 // pages
	started uint64 // clock ticks since boot
}

// List returns the sandboxes whose bwrap argv[0] starts with prefix, sorted
// by start time. root is the procfs mount point, normally "/proc".
func List(root, prefix string) ([]Sandbox, error) {
	boot, err := bootTime(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	all := map[int]*proc{}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readProc(root, pid)
		if err != nil {
			continue // exited while scanning, or not ours to read
		}
		all[pid] = p
		children[p.ppid] = append(children[p.ppid], pid)
	}

	tagged := func(p *proc) bool {
		return p != nil && len(p.argv) > 0 && strings.HasPrefix(p.argv[0], prefix)
	}
	pageSize := uint64(os.Getpagesize())
	var out []Sandbox
	for _, p := range all {
		// bwrap forks an inner bwrap as PID 1 of the new namespace; both carry
		// the tag, so only the one without a tagged parent is a sandbox root.
		if !tagged(p) || tagged(all[p.ppid]) {
			continue
		}
		s := Sandbox{
			PID:     p.pid,
			Name:    strings.TrimPrefix(p.argv[0], prefix),
			Started: boot.Add(ticks(p.started)),
		}
		s.Command, s.Dir = parseArgs(p.argv[1:])
		var cpu, rss uint64
		stack := []int{p.pid}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if c := all[pid]; c != nil {
				s.Procs++
				cpu += c.cpu
				rss += c.rss
			}
			stack = append(stack, children[pid]...)
		}
		s.CPU = ticks(cpu)
		s.RSS = rss * pageSize
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.Before(out[j].Started)
		}
		return out[i].PID < out[j].PID
	})
	return out, nil
}

func ticks(n uint64) time.Duration {
	return time.Duration(n) * time.Second / clockTicks
}

// parseArgs extracts the sandboxed command (everything after "--") and the
// last --chdir from a bwrap argument list.
func parseArgs(args []string) (cmd []string, dir string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			return args[i+1:], dir
		case "--chdir":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		}
	}
	return nil, dir
}

func readProc(root string, pid int) (*proc, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return nil, err
	}
	// The comm field is parenthesized and may itself contain spaces or
	// parentheses, so split after its last ")".
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return nil, fmt.Errorf("%s/stat: malformed", dir)
	}
	f := strings.Fields(string(stat[end+1:]))
	// f[0] is field 3 (state) in proc(5) numbering.
	if len(f) < 22 {
		return nil, fmt.Errorf("%s/stat: too few fields", dir)
	}
	num := func(field int) uint64 {
		n, _ := strconv.ParseUint(f[field-3], 10, 64)
		return n
	}
	p := &proc{
		pid:     pid,
		ppid:    int(num(4)),
		cpu:     num(14) + num(15) + num(16) + num(17),
		started: num(22),
		rss:     num(24),
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return nil, err
	}
	if cmdline = bytes.TrimSuffix(cmdline, []byte{0}); len(cmdline) > 0 {
		p.argv = strings.Split(string(cmdline), "\x00")
	}
	return p, nil
}

func bootTime(root string) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("%s/stat: bad btime: %w", root, err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("%s/stat: no btime", root)
}
