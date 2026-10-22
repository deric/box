package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Resource limits are cgroups, and an unprivileged process cannot write
// the cgroup it was started in (a session scope owned by the system
// manager, or a scope the terminal lives in alongside the shell). What it
// can do is ask the systemd user manager, to which the user's cgroup
// subtree is delegated, for a transient scope of its own, which is what
// systemd-run --user --scope does. box does the same over D-Bus with its
// own PID, so argv[0] and the process tree stay as they are, then verifies
// the limits through cgroupfs: the manager silently drops a limit whose
// controller is not delegated.

const (
	systemdDest    = "org.freedesktop.systemd1"
	systemdPath    = dbus.ObjectPath("/org/freedesktop/systemd1")
	systemdManager = "org.freedesktop.systemd1.Manager"
	cgroupRoot     = "/sys/fs/cgroup"
)

// scopeTimeout bounds the whole exchange with the manager: connecting,
// authenticating, the method calls and the wait for the scope to start.
// The manager has been seen to leave a call on its private socket
// unanswered for good while serving other connections, and box must not
// hang on that; the sandbox then runs without limits, with a warning.
// Tests shorten it.
var scopeTimeout = 10 * time.Second

// property is a systemd unit property as StartTransientUnit takes it.
type property struct {
	Name  string
	Value dbus.Variant
}

// auxUnit is an auxiliary unit for StartTransientUnit; none are used.
type auxUnit struct {
	Name  string
	Props []property
}

// applyLimits moves the current process into the transient scope unit
// with limits l and returns warnings about limits the kernel does not
// confirm. An error means no scope was created.
func applyLimits(unit, name string, l Limits) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scopeTimeout)
	defer cancel()
	conn, bus, err := connectUserManager(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := startScope(ctx, conn, bus, unit, scopeProperties(name, l)); err != nil {
		return nil, err
	}
	own, err := ownCgroup("/proc/self/cgroup")
	if err != nil {
		return []string{"cannot verify resource limits: " + err.Error()}, nil
	}
	if filepath.Base(own) != unit {
		return []string{fmt.Sprintf("resource limits may not apply: process is in cgroup %s, not in scope %s", own, unit)}, nil
	}
	return verifyLimits(filepath.Join(cgroupRoot, own), l), nil
}

// connectUserManager connects to the systemd user manager: through its
// private socket under $XDG_RUNTIME_DIR, which needs no bus daemon, or
// else through the session bus. The returned bool reports the latter. The
// connection is tied to ctx: when that ends, so does the connection, and
// with it anything blocked on it, authentication included.
func connectUserManager(ctx context.Context) (*dbus.Conn, bool, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	var errs []error
	private := filepath.Join(runtimeDir, "systemd", "private")
	if _, err := os.Stat(private); err == nil {
		conn, err := dbus.Dial("unix:path="+private, dbus.WithContext(ctx))
		if err == nil {
			if err = conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err == nil {
				return conn, false, nil
			}
			_ = conn.Close()
		}
		errs = append(errs, expired(ctx, private, err))
	} else {
		errs = append(errs, err)
	}
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		addr = "unix:path=" + filepath.Join(runtimeDir, "bus")
	}
	conn, err := dbus.Connect(addr, dbus.WithContext(ctx))
	if err != nil {
		errs = append(errs, expired(ctx, "session bus "+addr, err))
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return nil, false, fmt.Errorf("systemd user manager unreachable: %s", strings.Join(msgs, "; "))
	}
	return conn, true, nil
}

// expired describes err as a failure of what; once ctx has run out it
// says so instead, as the error a connection torn down by its context
// yields (a closed socket) does not.
func expired(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: no answer from the systemd user manager within %s", what, scopeTimeout)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// scopeProperties returns the properties of the scope for the current
// process: besides the limits, MemorySwapMax=0 so swap cannot stretch the
// memory limit, and a collect mode that lets systemd forget the scope
// even if it ends in failure (an OOM kill), so the name is free for a
// later process with the same PID.
func scopeProperties(name string, l Limits) []property {
	props := []property{
		{"Description", dbus.MakeVariant("box sandbox for " + name)},
		{"PIDs", dbus.MakeVariant([]uint32{uint32(os.Getpid())})},
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
	}
	if l.Memory > 0 {
		props = append(props,
			property{"MemoryMax", dbus.MakeVariant(l.Memory)},
			property{"MemorySwapMax", dbus.MakeVariant(uint64(0))})
	}
	if l.CPUs > 0 {
		props = append(props, property{"CPUQuotaPerSecUSec", dbus.MakeVariant(uint64(l.CPUs * 1e6))})
	}
	return props
}

// startScope asks the manager for the transient scope and waits for its
// start job to finish, which is when the PID has been moved. A bus
// connection receives job signals only once it matches them and
// subscribes; the private connection gets every signal of the manager as
// it is, so nothing is asked of it first. Every wait ends with ctx.
func startScope(ctx context.Context, conn *dbus.Conn, bus bool, unit string, props []property) error {
	sigs := make(chan *dbus.Signal, 64)
	conn.Signal(sigs)
	defer conn.RemoveSignal(sigs)
	mgr := conn.Object(systemdDest, systemdPath)
	if bus {
		if err := conn.AddMatchSignalContext(ctx, dbus.WithMatchInterface(systemdManager), dbus.WithMatchMember("JobRemoved")); err != nil {
			return expired(ctx, "matching job signals", err)
		}
		if err := mgr.CallWithContext(ctx, systemdManager+".Subscribe", 0).Err; err != nil {
			var dbusErr dbus.Error
			if !errors.As(err, &dbusErr) || dbusErr.Name != "org.freedesktop.systemd1.AlreadySubscribed" {
				return expired(ctx, "subscribing to job signals", err)
			}
		}
	}
	var job dbus.ObjectPath
	if err := mgr.CallWithContext(ctx, systemdManager+".StartTransientUnit", 0, unit, "fail", props, []auxUnit{}).Store(&job); err != nil {
		return expired(ctx, "starting scope "+unit, err)
	}
	for {
		select {
		case s, ok := <-sigs:
			if !ok {
				return expired(ctx, "starting scope "+unit, errors.New("connection closed while the scope was starting"))
			}
			if s.Name != systemdManager+".JobRemoved" || len(s.Body) < 4 {
				continue
			}
			if path, _ := s.Body[1].(dbus.ObjectPath); path != job {
				continue
			}
			if result, _ := s.Body[3].(string); result != "done" {
				return fmt.Errorf("starting scope %s: job %s", unit, result)
			}
			return nil
		case <-ctx.Done():
			return fmt.Errorf("starting scope %s: not started within %s", unit, scopeTimeout)
		}
	}
}

// ownCgroup returns the cgroup (v2) path of the current process from a
// /proc/<pid>/cgroup file: the "0::" line.
func ownCgroup(procFile string) (string, error) {
	f, err := os.Open(procFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if path, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return path, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no cgroups v2 entry", procFile)
}

// verifyLimits reads the limit files of cgroup dir and reports every
// limit that does not match l, which happens when the controller is not
// delegated to the user manager.
func verifyLimits(dir string, l Limits) []string {
	var warnings []string
	check := func(file, want, limit, controller string) {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			if os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("%s limit not applied: the %s controller is not available to the systemd user manager", limit, controller))
			} else {
				warnings = append(warnings, fmt.Sprintf("cannot verify %s limit: %v", limit, err))
			}
			return
		}
		if got := strings.TrimSpace(string(b)); !sameLimit(got, want) {
			warnings = append(warnings, fmt.Sprintf("%s limit not applied: %s is %q, want %q", limit, file, got, want))
		}
	}
	if l.Memory > 0 {
		check("memory.max", strconv.FormatUint(l.Memory, 10), "memory", "memory")
		check("memory.swap.max", "0", "swap", "memory")
	}
	if l.CPUs > 0 {
		// cpu.max holds "<quota> <period>"; systemd's default period is
		// 100ms, so the quota per core is 100000.
		check("cpu.max", strconv.FormatUint(uint64(l.CPUs*100000), 10), "cpu", "cpu")
	}
	return warnings
}

// sameLimit compares a cgroup limit file's content with the expected value,
// looking at the first field only (cpu.max carries the period as well).
func sameLimit(got, want string) bool {
	if f := strings.Fields(got); len(f) > 0 {
		got = f[0]
	}
	return got == want
}

// LimitsStatus reports the cgroup controllers delegated to the systemd
// user manager, sorted, or an error when the manager is unreachable or the
// delegation cannot be read.
func LimitsStatus() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scopeTimeout)
	defer cancel()
	conn, _, err := connectUserManager(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var v dbus.Variant
	if err := conn.Object(systemdDest, systemdPath).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, systemdManager, "ControlGroup").Store(&v); err != nil {
		return nil, expired(ctx, "reading the manager's cgroup", err)
	}
	cg, _ := v.Value().(string)
	b, err := os.ReadFile(filepath.Join(cgroupRoot, cg, "cgroup.subtree_control"))
	if err != nil {
		return nil, fmt.Errorf("reading delegated controllers: %w", err)
	}
	controllers := strings.Fields(string(b))
	sort.Strings(controllers)
	return controllers, nil
}

// LimitsInfo describes how resource limits are applied on this host: the
// systemd user manager and the controllers delegated to it, or why it is
// out of reach.
func LimitsInfo() string {
	controllers, err := LimitsStatus()
	if err != nil {
		return "not available: " + err.Error()
	}
	info := "cgroups v2 via the systemd user manager"
	if len(controllers) == 0 {
		return info + " (no controllers delegated)"
	}
	return info + " (controllers: " + strings.Join(controllers, " ") + ")"
}
