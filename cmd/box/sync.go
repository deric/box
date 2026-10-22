package main

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/deric/box/internal/diff"
	"github.com/deric/box/internal/sandbox"
)

// resolveConflicts settles the sync files whose host original changed while
// the sandbox ran. Each is shown as a diff of the host file against the
// sandbox copy on out (through diff(1) when the sandbox has it, see
// diff.Write); with a terminal (ask) the user is then asked whether
// to overwrite the host file or to keep it and save the copy aside to merge
// by hand, with answers read from in. Without one the host file is kept and
// the copy saved. Copies are saved under saveDir, the directory bound from
// hostDir on the host, which is what the user is told.
func resolveConflicts(conflicts []sandbox.Conflict, ask bool, in io.Reader, out io.Writer, saveDir, hostDir string) {
	r := bufio.NewReader(in)
	for _, c := range conflicts {
		say(out, "box: %s changed on the host while the sandbox ran, and the sandbox changed its copy:\n", c.File.Path)
		if err := diff.Write(out, c.File.Path+" (host)", c.File.Path+" (sandbox)", c.Host, c.Copy); err != nil {
			say(out, "box: %v\n", err)
		}
		overwrite := false
		if ask {
			overwrite = askOverwrite(r, out, c.File.Path)
		} else {
			say(out, "box: warning: no terminal to ask; keeping the host file\n")
		}
		if overwrite {
			if err := c.Overwrite(); err != nil {
				say(out, "box: %v\n", err)
			}
			continue
		}
		name, err := c.Save(saveDir, time.Now())
		if err != nil {
			say(out, "box: %v\n", err)
			continue
		}
		say(out, "box: kept %s; the sandbox copy is saved as %s to merge by hand\n", c.File.Path, filepath.Join(hostDir, name))
	}
}

// say prints a message for the user on out. A failed write to the terminal
// has nowhere else to be reported, so the error is dropped.
func say(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// askOverwrite puts the question for path and reads the answer: true to
// overwrite, false (the default, also on end of input) to keep and save.
func askOverwrite(r *bufio.Reader, out io.Writer, path string) bool {
	for {
		say(out, "box: overwrite %s with the sandbox copy? No keeps it and saves the copy aside to merge by hand. [y/N] ", path)
		line, err := r.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		switch answer {
		case "y", "yes", "o", "overwrite":
			return true
		case "", "n", "no", "k", "keep", "m", "merge":
			return false
		}
		if err != nil {
			say(out, "\n")
			return false
		}
	}
}
