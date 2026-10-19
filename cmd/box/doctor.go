package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/deric/box/internal/config"
	"github.com/deric/box/internal/sandbox"
)

// box doctor runs every check on this host that box run would otherwise
// trip over at runtime: the isolation mechanism and the kernel features it
// needs, the helpers behind DNS and resource limits, the configuration and
// the directories box writes to, and the tools the configuration refers
// to (the sandboxed binaries, podman, ssh). Each check ends in a status
// line and, when something is off, how to put it right. Checks of a
// feature no profile uses are left out.

// status is the outcome of one check.
type status int

const (
	statusOK   status = iota
	statusWarn        // box works, a feature is degraded
	statusFail        // box run cannot work
)

func (s status) String() string {
	switch s {
	case statusOK:
		return "ok"
	case statusWarn:
		return "warn"
	}
	return "FAIL"
}

// icon is the mark printed in front of the status.
func (s status) icon() string {
	switch s {
	case statusOK:
		return "\u2714" // heavy check mark
	case statusWarn:
		return "\u26a0" // warning sign
	}
	return "\u2718" // heavy ballot x
}

// color is the SGR sequence the status is shown in: green, yellow, red.
func (s status) color() string {
	switch s {
	case statusOK:
		return "\x1b[32m"
	case statusWarn:
		return "\x1b[33m"
	}
	return "\x1b[31;1m"
}

const (
	sgrReset = "\x1b[0m"
	sgrBold  = "\x1b[1m"
	sgrDim   = "\x1b[2m"
)

// useColor reports whether output to a terminal should be colored: yes
// unless NO_COLOR (https://no-color.org) is set or TERM is dumb.
func useColor(fd int) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(fd)
}

// painter wraps text in a color when colors are on.
type painter bool

func (p painter) paint(sgr, text string) string {
	if !p {
		return text
	}
	return sgr + text + sgrReset
}

// check is the result of one check: what was looked at, what was found,
// and for a warning or failure how to fix it (one or more lines).
type check struct {
	name   string
	status status
	detail string
	fix    string
}

// features summarises which features the configuration asks for, over every
// effective profile (see effectiveProfiles); checks of unused features are
// skipped.
type features struct {
	overlays bool
	dns      bool
	cpu      bool
	memory   bool
	podman   bool
	pasta    bool
	ssh      bool
	// sshKeys maps the ssh_key paths in use to the sections naming them.
	sshKeys map[string][]string
	// binaries lists the names with a section, sorted.
	binaries []string
}

func cmdDoctor(argv []string) error {
	var c common
	fs := newFlagSet("doctor", &c)
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
	cfg, checks := checkConfig(configPath)
	u := featureUsage(cfg)

	checks = append(checks, platformChecks(u)...)
	checks = append(checks, checkDirs()...)
	checks = append(checks, checkBinaries(u))
	if u.ssh {
		checks = append(checks, checkSSH(u)...)
	}
	checks = append(checks, checkTool("diff", "diff", "box init cannot show the differences to the defaults without it; sync_files conflicts fall back to a built-in diff", statusWarn))

	color := painter(useColor(int(os.Stdout.Fd())))
	if err := printChecks(os.Stdout, checks, color); err != nil {
		return err
	}
	fails, warns := 0, 0
	for _, ch := range checks {
		switch ch.status {
		case statusFail:
			fails++
		case statusWarn:
			warns++
		}
	}
	switch {
	case fails > 0:
		// The summary is the verdict, printed with the report rather than
		// as an error of box itself; the exit status still says it failed.
		fmt.Println("\n" + color.paint(statusFail.color(), plural(fails, "problem")+", "+plural(warns, "warning")+": box run will not work until the problems are fixed"))
		os.Exit(1)
	case warns > 0:
		fmt.Println("\n" + color.paint(statusWarn.color(), plural(warns, "warning")+", box works with reduced features"))
	default:
		fmt.Println("\n" + color.paint(statusOK.color(), "everything box needs is in place"))
	}
	return nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// printChecks renders the checks as aligned rows, an icon and the status
// in the status's color in front of each; fix lines follow the row they
// belong to, indented and dimmed. Columns are padded by visible width,
// which the color sequences do not add to.
func printChecks(out io.Writer, checks []check, color painter) error {
	mark := func(s status) string { return s.icon() + " " + s.String() }
	markWidth, nameWidth := 0, 0
	for _, ch := range checks {
		markWidth = max(markWidth, utf8.RuneCountInString(mark(ch.status)))
		nameWidth = max(nameWidth, utf8.RuneCountInString(ch.name))
	}
	pad := func(s string, width int) string {
		return strings.Repeat(" ", width-utf8.RuneCountInString(s))
	}
	indent := strings.Repeat(" ", markWidth+2+nameWidth+2+2)
	for _, ch := range checks {
		m := mark(ch.status)
		name := ch.name
		if ch.status != statusOK {
			name = color.paint(sgrBold, name)
		}
		line := color.paint(ch.status.color(), m) + pad(m, markWidth) + "  " + name + pad(ch.name, nameWidth) + "  " + ch.detail + "\n"
		if _, err := io.WriteString(out, line); err != nil {
			return err
		}
		if ch.status == statusOK || ch.fix == "" {
			continue
		}
		for _, fix := range strings.Split(strings.TrimRight(ch.fix, "\n"), "\n") {
			if _, err := io.WriteString(out, indent+color.paint(sgrDim, fix)+"\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkConfig loads the configuration and reports on the file and the
// profile directory. It always returns a usable configuration: the
// built-in defaults when the file is missing or broken.
func checkConfig(path string) (*config.Config, []check) {
	cfg, exists, err := config.Load(path)
	var checks []check
	switch {
	case err != nil:
		checks = append(checks, check{"config", statusFail, err.Error(),
			"fix the file, or move it away and run `box init` to start from the defaults"})
	case !exists:
		checks = append(checks, check{"config", statusWarn, path + " does not exist; the built-in defaults apply",
			"run `box init` to write it, then edit it to your needs"})
	default:
		checks = append(checks, check{"config", statusOK, path, ""})
	}
	if cfg == nil {
		cfg, _ = config.Parse(config.DefaultTOML)
	}
	dir := config.ProfileDir(path)
	switch fi, err := os.Stat(dir); {
	case os.IsNotExist(err):
		checks = append(checks, check{"profiles", statusOK, dir + " does not exist (no profiles)", ""})
	case err != nil:
		checks = append(checks, check{"profiles", statusWarn, dir + ": " + err.Error(), "make the directory readable, or remove it"})
	case !fi.IsDir():
		checks = append(checks, check{"profiles", statusWarn, dir + " is not a directory", "remove it, or replace it with a directory of <name>.toml profiles"})
	default:
		names := make([]string, len(cfg.Profiles))
		for i, p := range cfg.Profiles {
			names[i] = p.Name
		}
		detail := dir + " (none)"
		if len(names) > 0 {
			detail = dir + " (" + strings.Join(names, ", ") + ")"
		}
		checks = append(checks, check{"profiles", statusOK, detail, ""})
	}
	return cfg, checks
}

// featureUsage walks every effective profile for the features it enables.
func featureUsage(cfg *config.Config) features {
	u := features{sshKeys: map[string][]string{}}
	seen := map[string]bool{}
	for _, e := range effectiveProfiles(cfg) {
		p := e.profile
		if len(p.Overlays) > 0 {
			u.overlays = true
		}
		if p.DNS && p.Proxy && !p.Network {
			u.dns = true
		}
		if p.CPUs > 0 {
			u.cpu = true
		}
		if p.Memory != "" {
			u.memory = true
		}
		if p.Podman {
			u.podman = true
		}
		if p.Pasta && p.Proxy && !p.Network {
			u.pasta = true
		}
		if p.SSH {
			u.ssh = true
			if p.SSHKey != "" {
				label := e.label
				if label == "" {
					label = "default"
				}
				u.sshKeys[p.SSHKey] = append(u.sshKeys[p.SSHKey], label)
			}
		}
	}
	for name := range cfg.Binaries {
		seen[name] = true
	}
	for _, p := range cfg.Profiles {
		for name := range p.Binaries {
			seen[name] = true
		}
	}
	for name := range seen {
		u.binaries = append(u.binaries, name)
	}
	sort.Strings(u.binaries)
	return u
}

// checkDirs makes sure box can write where it keeps overlay layers and
// podman state (the state directory) and private /tmp directories, proxy
// sockets and logs (the tmp root).
func checkDirs() []check {
	var checks []check
	stateDir, err := config.StateDir()
	if err != nil {
		checks = append(checks, check{"state dir", statusFail, err.Error(), "set $HOME to your home directory"})
	} else {
		checks = append(checks, checkWritable("state dir", stateDir))
	}
	checks = append(checks, checkWritable("tmp root", sandbox.DefaultTmpRoot))
	return checks
}

// checkWritable reports whether dir, or when it does not exist yet the
// closest existing parent, can be written to.
func checkWritable(name, dir string) check {
	probe := dir
	for {
		fi, err := os.Stat(probe)
		if err == nil {
			if !fi.IsDir() {
				return check{name, statusFail, probe + " is not a directory", "remove it; box creates the directory itself"}
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return check{name, statusFail, dir + ": " + err.Error(), ""}
		}
		probe = parent
	}
	f, err := os.CreateTemp(probe, ".box-doctor-*")
	if err != nil {
		return check{name, statusFail, dir + ": " + err.Error(),
			fmt.Sprintf("make %s writable by you", probe)}
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	detail := dir
	if probe != dir {
		detail += " (will be created)"
	}
	return check{name, statusOK, detail, ""}
}

// checkBinaries looks for every binary the configuration has a section
// for; one that is missing only matters when it is run. podman has a check
// of its own when a profile enables it.
func checkBinaries(u features) check {
	var found, missing []string
	for _, name := range u.binaries {
		if name == "podman" && u.podman {
			continue
		}
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		} else {
			found = append(found, name)
		}
	}
	switch {
	case len(found)+len(missing) == 0:
		return check{"binaries", statusOK, "no [binaries.<name>] sections", ""}
	case len(missing) == 0:
		return check{"binaries", statusOK, "configured binaries found in PATH: " + strings.Join(found, ", "), ""}
	}
	var fixes []string
	for _, name := range missing {
		if hint := installHint(name); hint != "" {
			fixes = append(fixes, hint)
		}
	}
	fixes = append(fixes, "ignore this for binaries you do not run under box")
	detail := "not found in PATH: " + strings.Join(missing, ", ")
	if len(found) > 0 {
		detail += " (found: " + strings.Join(found, ", ") + ")"
	}
	return check{"binaries", statusWarn, detail, strings.Join(fixes, "\n")}
}

// checkSSH looks for the ssh client and the keys ssh_key names.
func checkSSH(u features) []check {
	checks := []check{checkTool("ssh", "ssh", "ssh = true is set but there is no ssh client to run inside the sandbox", statusWarn)}
	keys := make([]string, 0, len(u.sshKeys))
	for key := range u.sshKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	home, _ := os.UserHomeDir()
	for _, raw := range keys {
		sections := strings.Join(u.sshKeys[raw], ", ")
		optional := strings.HasPrefix(raw, "?")
		path := expandHome(strings.TrimPrefix(raw, "?"), home)
		switch fi, err := os.Stat(path); {
		case err != nil && optional:
			checks = append(checks, check{"ssh key", statusOK, path + " (optional, not present; " + sections + ")", ""})
		case err != nil:
			checks = append(checks, check{"ssh key", statusWarn, path + ": " + err.Error() + " (" + sections + ")",
				"create the key (ssh-keygen -t ed25519 -f " + path + "), fix ssh_key, or prefix it with ? to make it optional"})
		case !fi.Mode().IsRegular():
			checks = append(checks, check{"ssh key", statusWarn, path + " is not a regular file (" + sections + ")",
				"point ssh_key at the private key file"})
		default:
			checks = append(checks, check{"ssh key", statusOK, path + " (" + sections + ")", ""})
		}
	}
	return checks
}

// expandHome resolves a leading ~ and $HOME, $PWD and other variables the
// way sandbox paths are expanded.
func expandHome(p, home string) string {
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	return filepath.Clean(os.Expand(p, func(k string) string {
		if k == "HOME" {
			return home
		}
		return os.Getenv(k)
	}))
}

// checkTool looks for an executable in PATH; missing, it is reported with
// the given status, why it matters and how to install it.
func checkTool(name, exe, why string, missing status) check {
	path, err := exec.LookPath(exe)
	if err != nil {
		fix := why
		if hint := installHint(exe); hint != "" {
			fix = hint + "\n" + why
		}
		return check{name, missing, exe + " not found in PATH", fix}
	}
	return check{name, statusOK, path, ""}
}

// versionAtLeast reports whether version got, "0.11.1" or "2.41.3", is
// want or newer; a missing component counts as zero and anything after
// the dotted numbers is ignored.
func versionAtLeast(got, want string) bool {
	parse := func(v string) []int {
		var out []int
		for _, part := range strings.Split(v, ".") {
			i := 0
			for i < len(part) && part[i] >= '0' && part[i] <= '9' {
				i++
			}
			n, err := strconv.Atoi(part[:i])
			if err != nil {
				break
			}
			out = append(out, n)
			if i < len(part) {
				break
			}
		}
		return out
	}
	g, w := parse(got), parse(want)
	for i := range max(len(g), len(w)) {
		var a, b int
		if i < len(g) {
			a = g[i]
		}
		if i < len(w) {
			b = w[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// packages maps a tool to the package providing it, per package manager.
var packages = map[string]map[string]string{
	"bwrap":   {"apt": "bubblewrap", "dnf": "bubblewrap", "pacman": "bubblewrap", "apk": "bubblewrap", "zypper": "bubblewrap", "brew": ""},
	"unshare": {"apt": "util-linux", "dnf": "util-linux", "pacman": "util-linux", "apk": "util-linux-misc", "zypper": "util-linux"},
	"podman":  {"apt": "podman", "dnf": "podman", "pacman": "podman", "apk": "podman", "zypper": "podman", "brew": "podman"},
	"crun":    {"apt": "crun", "dnf": "crun", "pacman": "crun", "apk": "crun", "zypper": "crun"},
	"pasta":   {"apt": "passt", "dnf": "passt", "pacman": "passt", "apk": "passt", "zypper": "passt"},
	"nft":     {"apt": "nftables", "dnf": "nftables", "pacman": "nftables", "apk": "nftables", "zypper": "nftables"},
	"ssh":     {"apt": "openssh-client", "dnf": "openssh-clients", "pacman": "openssh", "apk": "openssh-client", "zypper": "openssh-clients"},
	"diff":    {"apt": "diffutils", "dnf": "diffutils", "pacman": "diffutils", "apk": "diffutils", "zypper": "diffutils", "brew": "diffutils"},
}

// installHint returns the command that installs tool on this system, or
// "" when box does not know the package or the package manager.
func installHint(tool string) string {
	pkgs, ok := packages[tool]
	if !ok {
		return ""
	}
	mgr := packageManager()
	pkg, ok := pkgs[mgr]
	if !ok || pkg == "" {
		return ""
	}
	switch mgr {
	case "apt":
		return "sudo apt install " + pkg
	case "dnf":
		return "sudo dnf install " + pkg
	case "pacman":
		return "sudo pacman -S " + pkg
	case "apk":
		return "sudo apk add " + pkg
	case "zypper":
		return "sudo zypper install " + pkg
	case "brew":
		return "brew install " + pkg
	}
	return ""
}

// packageManager guesses the system's package manager: brew on macOS,
// otherwise from the ID and ID_LIKE fields of /etc/os-release.
func packageManager() string {
	if runtime.GOOS == "darwin" {
		return "brew"
	}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return packageManagerFor(f)
}

// packageManagerFor derives the package manager from os-release text.
func packageManagerFor(r io.Reader) string {
	var ids []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if key == "ID" || key == "ID_LIKE" {
			ids = append(ids, strings.Fields(strings.Trim(value, `"`))...)
		}
	}
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "apt"
		case "fedora", "rhel", "centos":
			return "dnf"
		case "arch":
			return "pacman"
		case "alpine":
			return "apk"
		case "suse", "opensuse", "sles":
			return "zypper"
		}
	}
	return ""
}
