// Command box runs programs inside a sandbox (bubblewrap on Linux,
// sandbox-exec on macOS) using defaults and per-binary overrides from
// ~/.config/box/box.toml.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"box/internal/config"
	"box/internal/procs"
	"box/internal/sandbox"
)

// The _proxy and _forward commands are started by box itself (see
// internal/proxy) and are not part of the documented interface.

var version = "dev"

const usage = `box - run programs inside a sandbox (bwrap on Linux, sandbox-exec on macOS)

Usage:
  box run [-n] [-v] [-c FILE] <command> [args...]   run command in a sandbox
  box show [-c FILE] <command> [args...]             print the sandbox command line
  box config [-c FILE] [<command>]                   print the effective profile
  box init [-c FILE]                                 write the default config file
  box clean [<command>]                              delete overlay layers and stale /tmp dirs
  box ps [<command>]                                 list running sandboxes
  box info [-c FILE]                                 print version, isolation mechanism, config path
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
	case "info":
		err = cmdInfo(os.Args[2:])
	case "_proxy":
		err = cmdProxy(os.Args[2:])
	case "_forward":
		err = cmdForward(os.Args[2:])
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
		if pc := plan.ProxyCommand(); pc != "" {
			fmt.Println("# started on the host first:", pc)
		}
		fmt.Println(plan.Command())
		return nil
	}
	if verbose {
		if pc := plan.ProxyCommand(); pc != "" {
			fmt.Fprintln(os.Stderr, "box:", pc)
		}
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
	var filter string
	if len(argv) > 0 {
		filter = filepath.Base(argv[0])
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(stateDir, "overlays")
	if filter != "" {
		dir = filepath.Join(dir, filter)
	}
	removed := 0
	if _, err := os.Stat(dir); err == nil {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Println("removed", dir)
		removed++
	}
	n, err := cleanTmp(filter)
	if err != nil {
		return err
	}
	if removed+n == 0 {
		fmt.Println("nothing to clean")
	}
	return nil
}

// cleanTmp removes private /tmp directories (see sandbox.TmpDir) and proxy
// sockets and logs (sandbox.ProxyFiles) whose sandbox is no longer running,
// optionally only those of binary filter. It returns the number of entries
// removed.
func cleanTmp(filter string) (int, error) {
	entries, err := os.ReadDir(sandbox.DefaultTmpRoot)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			ext := filepath.Ext(name)
			if ext != ".sock" && ext != ".log" {
				continue
			}
			name = strings.TrimSuffix(name, ext)
			if ext == ".log" {
				// Drop the working directory, which starts with the
				// dash that replaced its leading slash.
				name, _, _ = strings.Cut(name, "--")
			}
		}
		i := strings.LastIndex(name, "-")
		if i <= 0 || (filter != "" && name[:i] != filter) {
			continue
		}
		if pid, err := strconv.Atoi(name[i+1:]); err != nil || procs.IsRunning(pid, name[:i]) {
			continue
		}
		path := filepath.Join(sandbox.DefaultTmpRoot, e.Name())
		if err := os.RemoveAll(path); err != nil {
			fmt.Fprintln(os.Stderr, "box: warning:", err)
			continue
		}
		fmt.Println("removed", path)
		n++
	}
	return n, nil
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

// cmdInfo prints what box would use on this host: its version, the
// isolation mechanism, the configuration and state locations and how many
// sandboxes are running.
func cmdInfo(argv []string) error {
	var c common
	fs := newFlagSet("info", &c)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	configPath := c.configPath
	if configPath == "" {
		var err error
		if configPath, err = config.Path(); err != nil {
			return err
		}
	}
	configNote := ""
	switch _, err := os.Stat(configPath); {
	case os.IsNotExist(err):
		configNote = " (missing, using built-in defaults; run `box init`)"
	case err != nil:
		configNote = " (" + err.Error() + ")"
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}

	r := sandbox.LookupRunner()
	runner := r.Mechanism + " (" + r.Name
	if r.Version != "" {
		runner += " " + r.Version
	}
	runner += ")"
	if r.Exe == "" {
		runner += " - not found in PATH"
	} else {
		runner += " at " + r.Exe
	}

	running := countSandboxes()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows := [][2]string{
		{"version", version},
		{"platform", runtime.GOOS + "/" + runtime.GOARCH},
		{"isolation", runner},
		{"config", configPath + configNote},
		{"state dir", stateDir},
		{"tmp root", sandbox.DefaultTmpRoot},
		{"running", running},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%s:\t%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return w.Flush()
}

// countSandboxes describes the running sandboxes as a total followed by a
// per-binary breakdown, e.g. "3 (claude 2, sh 1)".
func countSandboxes() string {
	list, err := procs.Sandboxes()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	if len(list) == 0 {
		return "0"
	}
	counts := map[string]int{}
	for _, s := range list {
		counts[s.Name]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s %d", name, counts[name])
	}
	return fmt.Sprintf("%d (%s)", len(list), strings.Join(parts, ", "))
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
