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

	"github.com/deric/box/internal/config"
	"github.com/deric/box/internal/procs"
	"github.com/deric/box/internal/sandbox"
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
  box clean [-c FILE] [<command>]                    delete overlay layers, stale /tmp dirs and logs
  box df [-c FILE] [<command>]                       show disk usage of overlay layers, /tmp dirs and logs
  box ps [<command>]                                 list running sandboxes
  box top [-i DURATION] [<command>]                  watch running sandboxes in the terminal
  box info [-c FILE]                                 print version, isolation mechanism, config path, log dir
  box version

Flags:
  -n, --dry-run   print the sandbox command instead of running it
  -v, --verbose   print the sandbox command before running it
  -c, --config    configuration file (default $BOX_CONFIG or ~/.config/box/box.toml)
  -i, --interval  refresh interval of box top (default 1s)

Keys in box top:
  q quit   c/m/p/t/n sort by cpu, memory, processes, uptime, name   r reverse   +/- interval
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
	case "df":
		err = cmdDf(os.Args[2:])
	case "ps":
		err = cmdPs(os.Args[2:])
	case "top":
		err = cmdTop(os.Args[2:])
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
	var c common
	fs := newFlagSet("clean", &c)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	var filter string
	if fs.NArg() > 0 {
		filter = filepath.Base(fs.Arg(0))
	}
	cfg, err := c.load()
	if err != nil {
		return err
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
	dirs, err := logDirs(cfg)
	if err != nil {
		return err
	}
	n, err := cleanTmp(filter, dirs)
	if err != nil {
		return err
	}
	if removed+n == 0 {
		fmt.Println("nothing to clean")
	}
	return nil
}

// logDirs returns the proxy log directories cfg uses, the default section's
// first, then those of binary sections that differ from it.
func logDirs(cfg *config.Config) ([]string, error) {
	names := make([]string, 0, len(cfg.Binaries))
	for name := range cfg.Binaries {
		names = append(names, name)
	}
	sort.Strings(names)
	var dirs []string
	seen := map[string]bool{}
	for _, name := range append([]string{""}, names...) {
		dir, err := sandbox.LogDir(cfg.Resolve(name), sandbox.Options{})
		if err != nil {
			return nil, err
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// cleanTmp removes private /tmp directories (see sandbox.TmpDir) and proxy
// sockets and logs (sandbox.ProxyFiles) whose sandbox is no longer running,
// optionally only those of binary filter. Logs are looked for in logDirs as
// well as in the tmp root. It returns the number of entries removed.
func cleanTmp(filter string, logDirs []string) (int, error) {
	n, err := cleanDir(sandbox.DefaultTmpRoot, filter, true)
	if err != nil {
		return 0, err
	}
	for _, dir := range logDirs {
		if dir == sandbox.DefaultTmpRoot {
			continue
		}
		m, err := cleanDir(dir, filter, false)
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

// cleanDir removes stale entries from one directory; with tmp it is the tmp
// root holding private /tmp directories and sockets, otherwise only logs are
// considered.
func cleanDir(dir, filter string, tmp bool) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		ext := ""
		if !e.IsDir() {
			ext = filepath.Ext(name)
			if ext != ".log" && (!tmp || ext != ".sock") {
				continue
			}
			name = strings.TrimSuffix(name, ext)
		} else if !tmp {
			continue
		}
		bin, pid, ok := splitEntry(name, ext == ".log")
		if !ok || (filter != "" && bin != filter) || procs.IsRunning(pid, bin) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			fmt.Fprintln(os.Stderr, "box: warning:", err)
			continue
		}
		fmt.Println("removed", path)
		n++
	}
	return n, nil
}

// splitEntry parses a /tmp/box entry name into the binary and PID. Private
// /tmp directories and sockets are <name>-<pid>; proxy logs additionally end
// in -<dir>, the working directory with slashes turned into dashes, so there
// the PID is the first all-digit component that follows a non-empty name.
func splitEntry(name string, log bool) (bin string, pid int, ok bool) {
	if !log {
		i := strings.LastIndex(name, "-")
		if i <= 0 {
			return "", 0, false
		}
		pid, err := strconv.Atoi(name[i+1:])
		if err != nil {
			return "", 0, false
		}
		return name[:i], pid, true
	}
	for i := 1; i < len(name); i++ {
		if name[i] != '-' {
			continue
		}
		digits := name[i+1:]
		if j := strings.IndexByte(digits, '-'); j >= 0 {
			digits = digits[:j]
		}
		if pid, err := strconv.Atoi(digits); err == nil {
			return name[:i], pid, true
		}
	}
	return "", 0, false
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
// isolation mechanism, the configuration, state and log locations and how
// many sandboxes are running.
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
	cfg, err := c.load()
	if err != nil {
		return err
	}
	logDir, err := describeLogDirs(cfg)
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
		{"log dir", logDir},
		{"running", running},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%s:\t%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return w.Flush()
}

// describeLogDirs renders the default log directory followed by the binaries
// that use a different one, e.g. "/tmp/box (claude: /home/u/logs)".
func describeLogDirs(cfg *config.Config) (string, error) {
	def, err := sandbox.LogDir(cfg.Resolve(""), sandbox.Options{})
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(cfg.Binaries))
	for name := range cfg.Binaries {
		names = append(names, name)
	}
	sort.Strings(names)
	var extra []string
	for _, name := range names {
		dir, err := sandbox.LogDir(cfg.Resolve(name), sandbox.Options{})
		if err != nil {
			return "", err
		}
		if dir != def {
			extra = append(extra, name+": "+dir)
		}
	}
	if len(extra) > 0 {
		def += " (" + strings.Join(extra, ", ") + ")"
	}
	return def, nil
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
