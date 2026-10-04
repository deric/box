// Command box runs programs inside a bubblewrap (bwrap) sandbox using
// defaults and per-binary overrides from ~/.config/box/box.toml.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"box/internal/config"
	"box/internal/sandbox"
)

var version = "dev"

const usage = `box - run programs inside a bubblewrap sandbox

Usage:
  box run [-n] [-v] [-c FILE] <command> [args...]   run command in a sandbox
  box show [-c FILE] <command> [args...]             print the bwrap command line
  box config [-c FILE] [<command>]                   print the effective profile
  box init [-c FILE]                                 write the default config file
  box clean [<command>]                              delete persistent overlay layers
  box version

Flags:
  -n, --dry-run   print the bwrap command instead of running it
  -v, --verbose   print the bwrap command before running it
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
