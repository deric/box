package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/deric/box/internal/sandbox"
)

// probeTimeout bounds each sandbox probe; a hung bwrap must not hang the
// doctor.
const probeTimeout = 10 * time.Second

// platformChecks covers bubblewrap and the kernel and systemd features a
// Linux sandbox relies on: user namespaces, unprivileged overlayfs for
// overlays, unshare(1) for DNS and the systemd user manager for resource
// limits. Probes that would fail for a reason already reported are
// skipped.
func platformChecks(u features) []check {
	var checks []check
	r := sandbox.LookupRunner()
	if r.Exe == "" {
		checks = append(checks, check{"bwrap", statusFail, "not found in PATH",
			installHint("bwrap") + "\nbox runs programs under bubblewrap: https://github.com/containers/bubblewrap"})
	} else {
		checks = append(checks, checkBwrapVersion(r, u))
	}
	if sandboxed() {
		checks = append(checks, check{"host", statusWarn, "box doctor is running inside a box sandbox: user namespaces, dns and limits cannot be probed here",
			"run box doctor on the host"})
	} else {
		if r.Exe != "" {
			ch := probeUserns(r.Exe)
			checks = append(checks, ch)
			if ch.status == statusOK && u.overlays && (r.Version == "" || versionAtLeast(r.Version, "0.10")) {
				checks = append(checks, probeOverlay(r.Exe))
			}
		}
		if u.dns {
			checks = append(checks, checkDNS())
		}
		if u.cpu || u.memory {
			checks = append(checks, checkLimits(u))
		}
	}
	if u.podman {
		checks = append(checks, checkPodman()...)
	}
	return checks
}

// sandboxed reports whether the doctor itself runs inside a box sandbox,
// where disable_userns leaves no way to create a namespace.
func sandboxed() bool { return os.Getenv("BOX_SANDBOX") == "1" }

// checkBwrapVersion reports the bwrap found and whether it is recent
// enough: 0.8 brought --disable-userns, 0.10 the overlay options.
func checkBwrapVersion(r sandbox.Runner, u features) check {
	detail := r.Mechanism + " " + r.Version + " at " + r.Exe
	if r.Version == "" {
		return check{"bwrap", statusOK, r.Mechanism + " at " + r.Exe + " (version unknown)", ""}
	}
	if fi, err := os.Stat(r.Exe); err == nil && fi.Mode()&os.ModeSetuid != 0 {
		detail += ", setuid"
	}
	fix := installHint("bwrap")
	if fix != "" {
		fix += "\n"
	}
	switch {
	case !versionAtLeast(r.Version, "0.8"):
		return check{"bwrap", statusFail, detail + "; 0.8 or newer is needed for --disable-userns", fix + "upgrade bubblewrap to 0.10 or newer"}
	case !versionAtLeast(r.Version, "0.10"):
		st := statusWarn
		if u.overlays {
			st = statusFail
		}
		return check{"bwrap", st, detail + "; overlays need 0.10 or newer (--overlay-src, --overlay, --tmp-overlay)",
			fix + "upgrade bubblewrap to 0.10 or newer, or drop every overlay from the configuration"}
	}
	return check{"bwrap", statusOK, detail, ""}
}

// probeUserns starts an empty sandbox the way box does, with every
// namespace unshared and nested user namespaces disabled.
func probeUserns(bwrap string) check {
	// --unshare-all only tries to unshare the user namespace;
	// --disable-userns insists on it (see buildBwrap).
	err := runBwrap(bwrap, "--unshare-all", "--unshare-user", "--disable-userns")
	if err == nil {
		return check{"user namespaces", statusOK, "bwrap can create a sandbox", ""}
	}
	return check{"user namespaces", statusFail, "bwrap cannot create a sandbox: " + err.Error(), usernsHint(err)}
}

// probeOverlay mounts a temporary overlay inside a sandbox, which needs a
// non-setuid bwrap and a kernel with unprivileged overlayfs.
func probeOverlay(bwrap string) check {
	err := runBwrap(bwrap, "--unshare-all", "--unshare-user", "--overlay-src", "/etc", "--tmp-overlay", "/etc")
	if err == nil {
		return check{"overlays", statusOK, "bwrap can mount an overlay (kernel " + kernelVersion() + ")", ""}
	}
	var fixes []string
	if fi, serr := os.Stat(bwrap); serr == nil && fi.Mode()&os.ModeSetuid != 0 {
		fixes = append(fixes, bwrap+" is setuid root, which cannot mount overlays: install a non-setuid bubblewrap (every current distribution's package)")
	}
	if kv := kernelVersion(); !versionAtLeast(kv, "5.11") {
		fixes = append(fixes, "kernel "+kv+" has no unprivileged overlayfs: 5.11 or newer is needed")
	}
	if len(fixes) == 0 {
		fixes = append(fixes, "overlays need a non-setuid bwrap 0.10+ on a kernel with unprivileged overlayfs (5.11+)")
	}
	fixes = append(fixes, "or drop every overlay from the configuration (overlays = [] and drop_binds)")
	return check{"overlays", statusFail, "bwrap cannot mount an overlay: " + err.Error(), strings.Join(fixes, "\n")}
}

// runBwrap runs `bwrap --ro-bind / / <args> true` and returns what bwrap
// complained about, if anything.
func runBwrap(bwrap string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	argv := append([]string{"--ro-bind", "/", "/", "--die-with-parent"}, args...)
	argv = append(argv, "true")
	out, err := exec.CommandContext(ctx, bwrap, argv...).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(strings.TrimPrefix(msg, "bwrap: "))
}

// usernsHint explains why unprivileged user namespaces may be unavailable
// from what the kernel exposes: the Debian and generic sysctls that turn
// them off, Ubuntu's AppArmor restriction, which a program needs a profile
// to get past, and the nesting limit a container or another sandbox may
// impose, which shows as ENOSPC.
func usernsHint(err error) string {
	var lines []string
	if v := sysctl("/proc/sys/user/max_user_namespaces"); v == "0" {
		lines = append(lines, "user namespaces are turned off (user.max_user_namespaces = 0): sudo sysctl -w user.max_user_namespaces=15000")
	}
	if v := sysctl("/proc/sys/kernel/unprivileged_userns_clone"); v == "0" {
		lines = append(lines, "unprivileged user namespaces are turned off: sudo sysctl -w kernel.unprivileged_userns_clone=1 (and persist it in /etc/sysctl.d)")
	}
	if v := sysctl("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); v == "1" {
		lines = append(lines,
			"AppArmor restricts unprivileged user namespaces to programs with a profile that allows them (kernel.apparmor_restrict_unprivileged_userns = 1);",
			"Ubuntu's apparmor and bubblewrap packages ship such profiles (bwrap-userns-restrict, unshare-userns-restrict in /etc/apparmor.d): reinstall them and run `sudo systemctl reload apparmor`,",
			"or turn the restriction off: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0")
	}
	if msg := err.Error(); strings.Contains(msg, "ENOSPC") || strings.Contains(msg, "No space left") {
		lines = append(lines, "ENOSPC here means the namespace nesting limit was hit: inside a container or another sandbox the parent may have disabled user namespaces")
	}
	if len(lines) == 0 {
		lines = append(lines, "enable unprivileged user namespaces: the kernel needs CONFIG_USER_NS, user.max_user_namespaces must be above 0, and no LSM may deny them")
	}
	return strings.Join(lines, "\n")
}

// sysctl reads a /proc/sys value; "" when the file is absent.
func sysctl(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// kernelVersion returns the running kernel's release, "6.8.0-45-generic".
func kernelVersion() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "unknown"
	}
	return unix.ByteSliceToString(u.Release[:])
}

// checkDNS makes sure unshare(1) is there and can create the namespaces
// the DNS listeners live in.
func checkDNS() check {
	unshare, version, err := sandbox.DNSStatus()
	switch {
	case errors.Is(err, sandbox.ErrNoUnshare):
		return check{"dns", statusWarn, "unshare not found in PATH; sandboxes get no resolver",
			installHint("unshare") + "\nunshare comes with util-linux 2.37 or newer; or set dns = false to run without a resolver inside"}
	case err != nil:
		return check{"dns", statusWarn, "unshare cannot create the network namespace; sandboxes get no resolver: " + err.Error(),
			usernsHint(err) + "\nor set dns = false to run without a resolver inside"}
	case version != "" && !versionAtLeast(version, "2.37"):
		return check{"dns", statusWarn, "unshare from util-linux " + version + " at " + unshare + "; 2.37 or newer is needed for --keep-caps",
			"upgrade util-linux"}
	}
	detail := "unshare at " + unshare
	if version != "" {
		detail += " (util-linux " + version + ")"
	}
	return check{"dns", statusOK, detail, ""}
}

// checkLimits makes sure the systemd user manager is reachable and has the
// cpu and memory controllers the configured limits need delegated.
func checkLimits(u features) check {
	controllers, err := sandbox.LimitsStatus()
	if err != nil {
		return check{"limits", statusWarn, "cpu and memory limits cannot be applied: " + err.Error(),
			"limits need a systemd user session (systemd 239+ on cgroups v2): log in through systemd (a display manager, ssh, or `machinectl shell`), or make sure $XDG_RUNTIME_DIR is set\n" +
				"or set cpus = 0 and memory = \"\" to run without limits"}
	}
	have := map[string]bool{}
	for _, c := range controllers {
		have[c] = true
	}
	var missing []string
	if u.cpu && !have["cpu"] {
		missing = append(missing, "cpu")
	}
	if u.memory && !have["memory"] {
		missing = append(missing, "memory")
	}
	detail := "systemd user manager with controllers: " + strings.Join(controllers, " ")
	if len(controllers) == 0 {
		detail = "systemd user manager with no controllers delegated"
	}
	if len(missing) > 0 {
		return check{"limits", statusWarn, detail + "; " + strings.Join(missing, " and ") + " not delegated, so those limits will not apply",
			"delegate them to the user manager: write\n" +
				"  [Service]\n  Delegate=cpu cpuset io memory pids\n" +
				"to /etc/systemd/system/user@.service.d/delegate.conf, then sudo systemctl daemon-reload and log in again"}
	}
	return check{"limits", statusOK, detail, ""}
}

// checkPodman looks for podman and an OCI runtime, which a sandbox with
// podman = true runs from the host's /usr.
func checkPodman() []check {
	checks := []check{checkTool("podman", "podman", "podman = true is set but there is no podman to run inside the sandbox", statusWarn)}
	if checks[0].status != statusOK {
		return checks
	}
	// Prints "podman version 4.9.3".
	if out, err := exec.Command("podman", "--version").Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) >= 3 {
			version := f[len(f)-1]
			if !versionAtLeast(version, "4.4") {
				checks[0] = check{"podman", statusWarn, checks[0].detail + " (" + version + "); 4.4 or newer is needed for CONTAINERS_CONF_OVERRIDE",
					"upgrade podman"}
			} else {
				checks[0].detail += " (" + version + ")"
			}
		}
	}
	for _, rt := range []string{"crun", "runc"} {
		if path, err := exec.LookPath(rt); err == nil {
			return append(checks, check{"oci runtime", statusOK, path, ""})
		}
	}
	fix := "podman needs crun or runc to start containers"
	if hint := installHint("crun"); hint != "" {
		fix = hint + "\n" + fix
	}
	return append(checks, check{"oci runtime", statusWarn, "neither crun nor runc found in PATH", fix})
}
