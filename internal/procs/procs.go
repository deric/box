// Package procs finds running box sandboxes: by scanning /proc on Linux, and
// via ps(1) and the kern.procargs2 sysctl on macOS.
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
	env     []string // macOS only
	cpu     time.Duration
	rss     uint64 // bytes
	started time.Time
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
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readProc(root, pid, boot)
		if err != nil {
			continue // exited while scanning, or not ours to read
		}
		all[pid] = p
	}
	// bwrap forks an inner bwrap as PID 1 of the new namespace; both carry
	// the tag, and group only reports the outer one.
	return group(all, func(p *proc) (Sandbox, bool) {
		if len(p.argv) == 0 || !strings.HasPrefix(p.argv[0], prefix) {
			return Sandbox{}, false
		}
		s := Sandbox{Name: strings.TrimPrefix(p.argv[0], prefix)}
		s.Command, s.Dir = parseArgs(p.argv[1:])
		return s, true
	}), nil
}

// Running reports whether pid is a live sandbox for binary name: a process
// whose argv[0] is prefix+name. Unlike List it also recognizes a sandbox
// nested inside another one. root is the procfs mount point.
func Running(root, prefix string, pid int, name string) bool {
	cmdline, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	argv0, _, _ := bytes.Cut(cmdline, []byte{0})
	return string(argv0) == prefix+name
}

// group builds one Sandbox per process tree whose root is tagged and has an
// untagged parent, summing usage over the whole tree. tag reports whether a
// process is tagged and fills in Name, Command and Dir.
func group(all map[int]*proc, tag func(*proc) (Sandbox, bool)) []Sandbox {
	children := map[int][]int{}
	for _, p := range all {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	tagged := func(p *proc) bool {
		if p == nil {
			return false
		}
		_, ok := tag(p)
		return ok
	}
	var out []Sandbox
	for _, p := range all {
		s, ok := tag(p)
		if !ok || tagged(all[p.ppid]) {
			continue
		}
		s.PID, s.Started = p.pid, p.started
		stack := []int{p.pid}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if c := all[pid]; c != nil {
				s.Procs++
				s.CPU += c.cpu
				s.RSS += c.rss
			}
			stack = append(stack, children[pid]...)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.Before(out[j].Started)
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func ticks(n uint64) time.Duration {
	return time.Duration(n) * time.Second / clockTicks
}

// parseArgs extracts the sandboxed command (everything after "--", minus
// the `box _forward` wrapper that exposes the proxy) and the last --chdir
// from a bwrap argument list.
func parseArgs(args []string) (cmd []string, dir string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			cmd = args[i+1:]
			if len(cmd) > 1 && cmd[1] == "_forward" {
				for j, a := range cmd {
					if a == "--" {
						return cmd[j+1:], dir
					}
				}
			}
			return cmd, dir
		case "--chdir":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		}
	}
	return nil, dir
}

func readProc(root string, pid int, boot time.Time) (*proc, error) {
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
		pid:  pid,
		ppid: int(num(4)),
		// Times include reaped children.
		cpu:     ticks(num(14) + num(15) + num(16) + num(17)),
		started: boot.Add(ticks(num(22))),
		rss:     num(24) * uint64(os.Getpagesize()),
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
