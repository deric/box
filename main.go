// Command box runs programs inside a sandbox (bubblewrap on Linux,
// sandbox-exec on macOS) using defaults and per-binary overrides from
// ~/.config/box/box.toml.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"box/internal/config"
	"box/internal/procs"
	"box/internal/sandbox"
)

var version = "dev"

const usage = `box - run programs inside a sandbox (bwrap on Linux, sandbox-exec on macOS)

Usage:
  box run [-n] [-v] [-c FILE] <command> [args...]   run command in a sandbox
  box show [-c FILE] <command> [args...]             print the sandbox command line
  box config [-c FILE] [<command>]                   print the effective profile
  box init [-c FILE]                                 write the default config file
  box clean [<command>]                              delete persistent overlay layers
  box ps [<command>]                                 list running sandboxes
  box version

Flags:
  -n, --dry-run   print the sandbox command instead of running it
  -v, --verbose   print the sandbox command before running it
  -c, --config    configuration file (default $BOX_CONFIG or ~/.config/box/box.toml)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:], false)
	case "show":
		err = cmdRun(os.Args[2:], true)
	case "config":
		err = cmdConfig(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "clean":
		err = cmdClean(os.Args[2:])
	case "ps":
		err = cmdPs(os.Args[2:])
	case "version", "--version":
		fmt.Println("box", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "box: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "box:", err)
		os.Exit(1)
	}
}

type common struct {
	configPath string
}

func newFlagSet(name string, c *common) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&c.configPath, "c", "", "configuration file")
	fs.StringVar(&c.configPath, "config", "", "configuration file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	return fs
}

func (c *common) load() (*config.Config, error) {
	path := c.configPath
	if path == "" {
		var err error
		if path, err = config.Path(); err != nil {
			return nil, err
		}
	}
	cfg, _, err := config.Load(path)
	return cfg, err
}

func cmdRun(argv []string, dryRun bool) error {
	var c common
	var verbose bool
	fs := newFlagSet("run", &c)
	fs.BoolVar(&dryRun, "n", dryRun, "dry run")
	fs.BoolVar(&dryRun, "dry-run", dryRun, "dry run")
	fs.BoolVar(&verbose, "v", false, "verbose")
	fs.BoolVar(&verbose, "verbose", false, "verbose")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("missing command\n\n%s", usage)
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	binary := fs.Arg(0)
	prof := cfg.Resolve(filepath.Base(binary))
	plan, err := sandbox.Build(prof, sandbox.Options{
		Binary:   binary,
		Args:     fs.Args()[1:],
		StateDir: stateDir,
	})
	if err != nil {
		return err
	}
	for _, w := range plan.Warnings {
		fmt.Fprintln(os.Stderr, "box: warning:", w)
	}
	if dryRun {
		fmt.Println(plan.Command())
		return nil
	}
	if verbose {
		fmt.Fprintln(os.Stderr, "box:", plan.Command())
	}
	return plan.Exec()
}

func cmdConfig(argv []string) error {
	var c common
	fs := newFlagSet("config", &c)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	path := c.configPath
	if path == "" {
		var err error
		if path, err = config.Path(); err != nil {
			return err
		}
	}
	cfg, exists, err := config.Load(path)
	if err != nil {
		return err
	}
	if exists {
		fmt.Printf("# %s\n", path)
	} else {
		fmt.Printf("# %s does not exist; showing built-in defaults (run `box init`)\n", path)
	}
	name := "default"
	if fs.NArg() > 0 {
		name = filepath.Base(fs.Arg(0))
	}
	fmt.Printf("# effective profile for %s\n", name)
	return toml.NewEncoder(os.Stdout).Encode(cfg.Resolve(name))
}

func cmdInit(argv []string) error {
	var c common
	fs := newFlagSet("init", &c)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	path := c.configPath
	if path == "" {
		var err error
		if path, err = config.Path(); err != nil {
			return err
		}
	}
	if err := config.WriteDefault(path); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func cmdClean(argv []string) error {
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(stateDir, "overlays")
	if len(argv) > 0 {
		dir = filepath.Join(dir, filepath.Base(argv[0]))
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Println("nothing to clean at", dir)
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Println("removed", dir)
	return nil
}

func cmdPs(argv []string) error {
	list, err := procs.Sandboxes()
	if err != nil {
		return err
	}
	var filter string
	if len(argv) > 0 {
		filter = filepath.Base(argv[0])
	}
	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "PID\tNAME\tPROCS\tCPU\tMEM\tUPTIME\tDIR\tCOMMAND"); err != nil {
		return err
	}
	for _, s := range list {
		if filter != "" && s.Name != filter {
			continue
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			s.PID, s.Name, s.Procs, formatDuration(s.CPU), formatBytes(s.RSS),
			formatDuration(now.Sub(s.Started)), s.Dir, strings.Join(s.Command, " ")); err != nil {
			return err
		}
	}
	return w.Flush()
}

// formatDuration renders d compactly at whole-second precision: 45s, 3m07s,
// 2h05m, 3d04h.
func formatDuration(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd%02dh", s/86400, s%86400/3600)
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
