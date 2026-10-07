package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/deric/box/internal/config"
	"github.com/deric/box/internal/sandbox"
)

// diskEntry is one thing a binary's sandboxes keep on the host: an overlay
// layer, a private /tmp directory or a proxy log.
type diskEntry struct {
	name string // binary
	kind string // "overlay", "tmp" or "log"
	path string
	size int64 // bytes on disk
}

// diskUsage is what one binary's sandboxes occupy on the host, in bytes.
type diskUsage struct {
	overlays int64 // persistent overlay layers under <state>/overlays/<name>
	tmp      int64 // private /tmp directories <tmp root>/<name>-<pid>
	logs     int64 // proxy logs <log dir>/<name>-<pid>-<dir>.log
}

func (u diskUsage) total() int64 { return u.overlays + u.tmp + u.logs }

func (u *diskUsage) add(e diskEntry) {
	switch e.kind {
	case "overlay":
		u.overlays += e.size
	case "tmp":
		u.tmp += e.size
	case "log":
		u.logs += e.size
	}
}

// cmdDf prints how much disk space each binary's sandboxes take on the
// host: its persistent overlay layers, private /tmp directories and proxy
// logs, which is what `box clean` would remove. With -a every layer,
// directory and log is listed on its own.
func cmdDf(argv []string) error {
	var c common
	var all bool
	flags := newFlagSet("df", &c)
	flags.BoolVar(&all, "a", false, "list every overlay layer, /tmp directory and log")
	flags.BoolVar(&all, "all", false, "list every overlay layer, /tmp directory and log")
	if err := flags.Parse(argv); err != nil {
		return err
	}
	var filter string
	if flags.NArg() > 0 {
		filter = filepath.Base(flags.Arg(0))
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	dirs, err := logDirs(cfg)
	if err != nil {
		return err
	}
	entries, err := gatherUsage(filepath.Join(stateDir, "overlays"), sandbox.DefaultTmpRoot, dirs, filter)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if all {
		err = printEntries(w, entries)
	} else {
		err = printSummary(w, entries)
	}
	if err != nil {
		return err
	}
	return w.Flush()
}

// printSummary writes one row per binary with its overlay, /tmp and log
// usage and the sum, plus a total row when there is more than one binary.
func printSummary(w io.Writer, entries []diskEntry) error {
	usage := map[string]diskUsage{}
	for _, e := range entries {
		u := usage[e.name]
		u.add(e)
		usage[e.name] = u
	}
	names := make([]string, 0, len(usage))
	for name := range usage {
		names = append(names, name)
	}
	sort.Strings(names)
	if _, err := fmt.Fprintln(w, "NAME\tOVERLAYS\tTMP\tLOGS\tTOTAL"); err != nil {
		return err
	}
	row := func(name string, u diskUsage) error {
		_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name,
			formatBytes(uint64(u.overlays)), formatBytes(uint64(u.tmp)),
			formatBytes(uint64(u.logs)), formatBytes(uint64(u.total())))
		return err
	}
	var sum diskUsage
	for _, name := range names {
		u := usage[name]
		sum.overlays += u.overlays
		sum.tmp += u.tmp
		sum.logs += u.logs
		if err := row(name, u); err != nil {
			return err
		}
	}
	if len(names) > 1 {
		return row("total", sum)
	}
	return nil
}

// printEntries writes every entry on its own row, grouped by binary with a
// subtotal after each binary that has more than one, and a total at the end
// when there is more than one binary.
func printEntries(w io.Writer, entries []diskEntry) error {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if a.kind != b.kind {
			return kindOrder(a.kind) < kindOrder(b.kind)
		}
		return a.path < b.path
	})
	if _, err := fmt.Fprintln(w, "NAME\tKIND\tPATH\tSIZE"); err != nil {
		return err
	}
	var total, subtotal int64
	count, names := 0, 0
	flush := func() error {
		if count > 1 {
			if _, err := fmt.Fprintf(w, "\t\ttotal\t%s\n", formatBytes(uint64(subtotal))); err != nil {
				return err
			}
		}
		subtotal, count = 0, 0
		return nil
	}
	for i, e := range entries {
		if i == 0 || e.name != entries[i-1].name {
			if err := flush(); err != nil {
				return err
			}
			names++
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.name, e.kind, e.path, formatBytes(uint64(e.size))); err != nil {
			return err
		}
		subtotal += e.size
		total += e.size
		count++
	}
	if err := flush(); err != nil {
		return err
	}
	if names > 1 {
		if _, err := fmt.Fprintf(w, "total\t\t\t%s\n", formatBytes(uint64(total))); err != nil {
			return err
		}
	}
	return nil
}

func kindOrder(kind string) int {
	switch kind {
	case "overlay":
		return 0
	case "tmp":
		return 1
	}
	return 2
}

// gatherUsage measures the disk usage of every binary that has overlay
// layers under overlayRoot, private /tmp directories under tmpRoot or proxy
// logs in logDirs (or in tmpRoot), optionally only of binary filter: one
// entry per layer, directory and log. Entries that cannot be read are
// skipped with a warning.
func gatherUsage(overlayRoot, tmpRoot string, logDirs []string, filter string) ([]diskEntry, error) {
	var out []diskEntry
	add := func(name, kind, path string) error {
		if filter != "" && name != filter {
			return nil
		}
		n, err := dirSize(path)
		if err != nil {
			return err
		}
		out = append(out, diskEntry{name: name, kind: kind, path: path, size: n})
		return nil
	}

	bins, err := readDir(overlayRoot)
	if err != nil {
		return nil, err
	}
	for _, b := range bins {
		if !b.IsDir() {
			continue
		}
		dir := filepath.Join(overlayRoot, b.Name())
		layers, err := readDir(dir)
		if err != nil {
			return nil, err
		}
		for _, l := range layers {
			if !l.IsDir() {
				continue
			}
			if err := add(b.Name(), "overlay", filepath.Join(dir, l.Name())); err != nil {
				return nil, err
			}
		}
	}

	dirs := append([]string{tmpRoot}, logDirs...)
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := readDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(dir, name)
			switch {
			case e.IsDir() && dir == tmpRoot:
				if bin, _, ok := splitEntry(name, false); ok {
					if err := add(bin, "tmp", path); err != nil {
						return nil, err
					}
				}
			case !e.IsDir() && filepath.Ext(name) == ".log":
				if bin, _, ok := splitEntry(strings.TrimSuffix(name, ".log"), true); ok {
					if err := add(bin, "log", path); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return out, nil
}

// readDir lists dir, treating a missing directory as empty.
func readDir(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return entries, err
}

// dirSize returns the space path and everything below it occupy on disk,
// like du: allocated blocks rather than file lengths, so sparse files count
// what they use. Symlinks are not followed. Entries that cannot be read are
// skipped with a warning, except directories that may not be listed: the
// kernel keeps a mode 000 "work/work" directory in every overlay work dir
// for copy-ups in progress, which is empty once the sandbox has exited.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == path {
				return err
			}
			if d != nil && d.IsDir() {
				if !errors.Is(err, fs.ErrPermission) {
					fmt.Fprintln(os.Stderr, "box: warning:", err)
				}
				return fs.SkipDir
			}
			fmt.Fprintln(os.Stderr, "box: warning:", err)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintln(os.Stderr, "box: warning:", err)
			return nil
		}
		total += allocated(info)
		return nil
	})
	return total, err
}
