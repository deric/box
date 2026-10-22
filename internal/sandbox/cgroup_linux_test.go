package sandbox

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnCgroup(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cgroup")
	if err := os.WriteFile(file, []byte("1:name=systemd:/legacy\n0::/user.slice/user-1000.slice/user@1000.service/app.slice/box-sh-42.scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ownCgroup(file)
	if err != nil || got != "/user.slice/user-1000.slice/user@1000.service/app.slice/box-sh-42.scope" {
		t.Errorf("ownCgroup = %q, %v", got, err)
	}
	if err := os.WriteFile(file, []byte("1:name=systemd:/legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ownCgroup(file); err == nil {
		t.Error("want an error without a cgroups v2 entry")
	}
}

func TestVerifyLimits(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := Limits{CPUs: 1.5, Memory: 2 << 30}
	write("memory.max", "2147483648\n")
	write("memory.swap.max", "0\n")
	write("cpu.max", "150000 100000\n")
	if w := verifyLimits(dir, l); len(w) != 0 {
		t.Errorf("matching limits produced warnings %v", w)
	}

	write("memory.max", "max\n")
	if err := os.Remove(filepath.Join(dir, "cpu.max")); err != nil {
		t.Fatal(err)
	}
	w := verifyLimits(dir, l)
	if len(w) != 2 || !strings.Contains(w[0], `memory.max is "max"`) || !strings.Contains(w[1], "cpu controller is not available") {
		t.Errorf("warnings = %v", w)
	}
	if w := verifyLimits(dir, Limits{}); len(w) != 0 {
		t.Errorf("no limits, but warnings %v", w)
	}
}

func TestScopeProperties(t *testing.T) {
	names := func(l Limits) []string {
		var out []string
		for _, p := range scopeProperties("sh", l) {
			out = append(out, p.Name)
		}
		return out
	}
	got := strings.Join(names(Limits{CPUs: 2, Memory: 1 << 30}), " ")
	if got != "Description PIDs CollectMode MemoryMax MemorySwapMax CPUQuotaPerSecUSec" {
		t.Errorf("properties = %s", got)
	}
	props := scopeProperties("sh", Limits{CPUs: 2})
	if v := props[len(props)-1].Value.Value(); v != uint64(2000000) {
		t.Errorf("CPUQuotaPerSecUSec = %v (%T)", v, v)
	}
	if got := strings.Join(names(Limits{Memory: 1}), " "); strings.Contains(got, "CPU") {
		t.Errorf("properties without cpus = %s", got)
	}
	if pids := props[1].Value.Value().([]uint32); len(pids) != 1 || pids[0] != uint32(os.Getpid()) {
		t.Errorf("PIDs = %v", pids)
	}
}

// silentManager listens where connectUserManager looks for the systemd
// user manager's private socket and plays its part of the authentication
// exchange, then answers nothing: the state a real manager has been seen
// to leave a connection in.
func silentManager(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "systemd"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "systemd", "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(dir, "nobus"))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				if b, err := r.ReadByte(); err != nil || b != 0 {
					return
				}
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var reply string
					switch {
					case strings.HasPrefix(line, "AUTH EXTERNAL"):
						reply = "OK 00000000000000000000000000000000\r\n"
					case strings.HasPrefix(line, "AUTH"):
						reply = "REJECTED EXTERNAL\r\n"
					case strings.HasPrefix(line, "NEGOTIATE_UNIX_FD"):
						reply = "AGREE_UNIX_FD\r\n"
					case strings.HasPrefix(line, "BEGIN"):
						// Authenticated; swallow everything from here on.
						_, _ = io.Copy(io.Discard, r)
						return
					}
					if _, err := io.WriteString(c, reply); err != nil {
						return
					}
				}
			}()
		}
	}()
}

func TestApplyLimitsTimesOut(t *testing.T) {
	silentManager(t)
	old := scopeTimeout
	scopeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { scopeTimeout = old })
	start := time.Now()
	warnings, err := applyLimits("box-test-1.scope", "test", Limits{Memory: 1 << 20})
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("applyLimits took %s, want the deadline of %s to end it", took, scopeTimeout)
	}
	if err == nil {
		t.Fatalf("applyLimits returned warnings %v and no error against a manager that never answers", warnings)
	}
	t.Logf("applyLimits: %v", err)
	if !strings.Contains(err.Error(), "no answer from the systemd user manager within") {
		t.Errorf("error = %v, want the deadline named", err)
	}
}
