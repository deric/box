package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/deric/box/internal/config"
	"github.com/deric/box/internal/sandbox"
)

// diskUsage is what one binary's sandboxes occupy on the host, in bytes.
type diskUsage struct {
	overlays int64 // persistent overlay layers under <state>/overlays/<name>
	tmp      int64 // private /tmp directories <tmp root>/<name>-<pid>
	logs     int64 // proxy logs <log dir>/<name>-<pid>-<dir>.log
}

func (u diskUsage) total() int64 { return u.overlays + u.tmp + u.logs }

// cmdDf prints how much disk space each binary's sandboxes take on the
// host: its persistent overlay layers, private /tmp directories and proxy
// logs, which is what `box clean` would remove.
func cmdDf(argv []string) error {
	var c common
	flags := newFlagSet("df", &c)
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
	usage, err := gatherUsage(filepath.Join(stateDir, "overlays"), sandbox.DefaultTmpRoot, dirs, filter)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(usage))
	for name := range usage {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tOVERLAYS\tTMP\tLOGS\tTOTAL"); err != nil {
		return err
	}
	var sum diskUsage
	for _, name := range names {
		u := usage[name]
		sum.overlays += u.overlays
		sum.tmp += u.tmp
		sum.logs += u.logs
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name,
			formatBytes(uint64(u.overlays)), formatBytes(uint64(u.tmp)),
			formatBytes(uint64(u.logs)), formatBytes(uint64(u.total()))); err != nil {
			return err
		}
	}
	if len(names) > 1 {
		if _, err := fmt.Fprintf(w, "total\t%s\t%s\t%s\t%s\n",
			formatBytes(uint64(sum.overlays)), formatBytes(uint64(sum.tmp)),
			formatBytes(uint64(sum.logs)), formatBytes(uint64(sum.total()))); err != nil {
			return err
		}
	}
	return w.Flush()
}

// gatherUsage measures the disk usage of every binary that has overlay
// layers under overlayRoot, private /tmp directories under tmpRoot or proxy
// logs in logDirs (or in tmpRoot), optionally only of binary filter. Entries
// that cannot be read are skipped with a warning.
func gatherUsage(overlayRoot, tmpRoot string, logDirs []string, filter string) (map[string]diskUsage, error) {
	usage := map[string]diskUsage{}
	add := func(name string, f func(u *diskUsage, n int64), path string) error {
		if filter != "" && name != filter {
			return nil
		}
		n, err := dirSize(path)
		if err != nil {
			return err
		}
		u := usage[name]
		f(&u, n)
		usage[name] = u
		return nil
	}
	overlay := func(u *diskUsage, n int64) { u.overlays += n }
	tmp := func(u *diskUsage, n int64) { u.tmp += n }
	logs := func(u *diskUsage, n int64) { u.logs += n }

	entries, err := readDir(overlayRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := add(e.Name(), overlay, filepath.Join(overlayRoot, e.Name())); err != nil {
			return nil, err
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
					if err := add(bin, tmp, path); err != nil {
						return nil, err
					}
				}
			case !e.IsDir() && filepath.Ext(name) == ".log":
				if bin, _, ok := splitEntry(strings.TrimSuffix(name, ".log"), true); ok {
					if err := add(bin, logs, path); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return usage, nil
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
