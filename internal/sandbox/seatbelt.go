package sandbox

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deric/box/internal/config"
)

// seatbeltBase opens every macOS profile. Everything not allowed is denied;
// file access is granted per mount by buildSeatbelt. In SBPL the last
// matching rule wins, which mirrors bwrap's parents-first mount order.
const seatbeltBase = `(version 1)
(deny default)

; Processes may run and fork, but only inspect and signal their own sandbox.
(allow process-exec process-fork)
(allow process-info* (target same-sandbox))
(allow signal (target same-sandbox))

; System information and local IPC ordinary programs rely on.
(allow sysctl-read)
(allow user-preference-read)
(allow ipc-posix-sem ipc-posix-shm)
(allow mach-lookup
  (global-name "com.apple.bsd.dirhelper")
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.system.notification_center")
  (global-name "com.apple.system.logger")
  (global-name "com.apple.logd")
  (global-name "com.apple.diagnosticd"))

; stat() works on every path so path resolution does; reading contents and
; listing directories needs a rule below.
(allow file-read-metadata)
(allow file-read-data (literal "/"))

; Devices. The terminal box was started from is inherited; ioctl on it is
; allowed so programs detect a TTY, and new PTYs may be opened.
(allow file-read* file-write*
  (literal "/dev/null")
  (literal "/dev/zero")
  (literal "/dev/tty")
  (literal "/dev/dtracehelper")
  (subpath "/dev/fd"))
(allow file-read*
  (literal "/dev/random")
  (literal "/dev/urandom")
  (literal "/dev/autofs_nowait"))
(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx"))
(allow file-read* file-write*
  (require-all (regex #"^/dev/ttys[0-9]+$") (extension "com.apple.sandbox.pty")))
(allow file-ioctl
  (literal "/dev/tty")
  (literal "/dev/dtracehelper")
  (regex #"^/dev/ttys[0-9]+$"))
`

// seatbeltNetwork is added when the profile shares the network. Unix sockets
// stay restricted to exposed paths, as with bwrap's mount namespace.
const seatbeltNetwork = `
; Network: IP traffic plus the system services name resolution and TLS
; certificate checks use.
(allow network* system-socket)
(deny network-outbound (subpath "/"))
` + seatbeltDNS + seatbeltNetServices

// seatbeltDNS lets the sandbox use the system resolver: libinfo hands
// every lookup to mDNSResponder over its socket or its XPC service.
const seatbeltDNS = `(allow network-outbound (literal "/private/var/run/mDNSResponder"))
(allow mach-lookup (global-name "com.apple.dnssd.service"))
`

// seatbeltNetServices names the system services programs that use the
// network rely on: path evaluation (networkd, configd) and TLS trust
// (SecurityServer, trustd, ocspd). None of them opens connections on the
// sandbox's behalf.
const seatbeltNetServices = `(allow mach-lookup
  (global-name "com.apple.networkd")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.trustd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.ocspd"))
`

// SeatbeltProxyRules returns the network rules of a profile behind the
// proxy listening on addr, a loopback TCP address (see Proxy): the sandbox
// may connect to that port and to nothing else, so the proxy's allowlist
// is all the network it has, the way the empty network namespace is on
// Linux. Programs may listen on loopback, where the host reaches them (a
// dev server in a browser), but not connect to anything else there: the
// host's loopback is not the sandbox's, so a service on it is reached, if
// at all, through the proxy by listing localhost in allow_hosts. With dns
// the system resolver is reachable too; its answers are not filtered, as
// on Linux. `box doctor` compiles these rules to check the release
// accepts them.
func SeatbeltProxyRules(addr string, dns bool) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("proxy address %q: %w", addr, err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("proxy address %q: port is not a number", addr)
	}
	var b strings.Builder
	b.WriteString("\n; Network: only the proxy box runs on the host is reachable, at " + addr + ";\n")
	b.WriteString("; it lets HTTP(S) through to allow_hosts. Programs may listen on loopback\n")
	b.WriteString("; but reach nothing else there.\n")
	b.WriteString(`(allow network-outbound (remote tcp "localhost:` + port + `"))` + "\n")
	b.WriteString(`(allow network-bind network-inbound (local ip "localhost:*"))` + "\n")
	if dns {
		b.WriteString("; Name resolution through the system resolver (dns = true).\n")
		b.WriteString(seatbeltDNS)
	}
	b.WriteString(seatbeltNetServices)
	return b.String(), nil
}

// buildSeatbelt computes a sandbox-exec invocation whose profile grants the
// access the mounts describe; exe is the sandbox-exec executable. Seatbelt
// only filters access to the real filesystem, so tmpfs paths are hidden
// rather than replaced and overlays are read-only (collect warns).
func buildSeatbelt(prof config.Profile, opts Options, exe string) (*Plan, error) {
	plan := &Plan{Binary: filepath.Base(opts.Binary), Exe: exe}
	mounts, err := collect(prof, &opts, plan, true)
	if err != nil {
		return nil, err
	}
	plan.Name = opts.Name
	if prof.Hostname != "" {
		plan.Warnings = append(plan.Warnings, "hostname is not supported by sandbox-exec; ignored")
	}
	if prof.NewSession {
		plan.Warnings = append(plan.Warnings, "new_session is not supported by sandbox-exec; ignored")
	}
	if prof.DisableUserns {
		plan.Warnings = append(plan.Warnings, "disable_userns is not supported by sandbox-exec; ignored")
	}
	if l, err := limits(prof); err != nil {
		return nil, err
	} else if l != nil {
		plan.Warnings = append(plan.Warnings, "cpus and memory limits are not supported by sandbox-exec; ignored")
	}
	if prof.TmpfsSize != "" {
		plan.Warnings = append(plan.Warnings, "tmpfs_size is not supported by sandbox-exec; ignored")
	}

	var b strings.Builder
	b.WriteString(seatbeltBase)
	switch {
	case prof.Network:
		b.WriteString(seatbeltNetwork)
	case plan.Proxy != nil:
		rules, err := SeatbeltProxyRules(plan.Proxy.Addr, prof.DNS)
		if err != nil {
			return nil, err
		}
		b.WriteString(rules)
	}
	b.WriteString("\n; Mounts from the box profile, parents first.\n")
	for _, m := range mounts {
		b.WriteString(m.seatbelt())
	}
	if len(prof.SeatbeltRules) > 0 {
		b.WriteString("\n; seatbelt_rules from the box profile.\n")
		for _, r := range prof.SeatbeltRules {
			b.WriteString(r + "\n")
		}
	}

	plan.ClearEnv = prof.ClearEnv
	plan.UnsetEnv = prof.UnsetEnv
	if prof.ClearEnv {
		plan.Env = append(plan.Env, passEnv(prof)...)
	}
	if plan.TmpDir != "" {
		// Point programs at the private directory, since the host's /tmp
		// is hidden. Claude Code ignores TMPDIR and uses /tmp unless
		// CLAUDE_CODE_TMPDIR is set.
		for _, kv := range [][2]string{{"TMPDIR", plan.TmpDir + "/"}, {"CLAUDE_CODE_TMPDIR", plan.TmpDir}} {
			if _, set := prof.Env[kv[0]]; !set {
				plan.Env = append(plan.Env, kv[0]+"="+kv[1])
			}
		}
	}
	if plan.Proxy != nil {
		// Point proxy-aware programs at the proxy. Unlike on Linux no
		// NO_PROXY is set: loopback here is the host's, which the
		// profile keeps out of reach, so a request to localhost goes
		// through the proxy like any other and is subject to allow_hosts.
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			if _, set := prof.Env[k]; !set {
				plan.Env = append(plan.Env, k+"=http://"+plan.Proxy.Addr)
			}
		}
	}
	for _, k := range sortedKeys(prof.Env) {
		plan.Env = append(plan.Env, k+"="+prof.Env[k])
	}
	plan.Env = append(plan.Env, NameEnv+"="+plan.Name, BinaryEnv+"="+plan.Binary, DirEnv+"="+opts.Cwd)
	plan.Dir = opts.Cwd

	args := append([]string{}, prof.ExtraArgs...)
	args = append(args, "-p", b.String(), "--", opts.Binary)
	plan.Args = append(args, opts.Args...)
	return plan, nil
}

func (m *mount) seatbelt() string {
	p := sbplString(m.dest)
	switch m.kind {
	case kindRO:
		return fmt.Sprintf("(allow file-read* (subpath %s))\n(allow network-outbound (subpath %[1]s))\n", p)
	case kindRW:
		return fmt.Sprintf("(allow file-read* file-write* (subpath %s))\n(allow network-outbound (subpath %[1]s))\n", p)
	case kindDev:
		return fmt.Sprintf("(allow file-read* file-write* file-ioctl (subpath %s))\n", p)
	case kindTmpfs:
		return fmt.Sprintf("(deny file-read* file-write* network-outbound (subpath %s))\n(allow file-read-metadata (subpath %[1]s))\n", p)
	}
	return "" // symlinks and overlays are resolved by collect
}

// sbplString quotes s as an SBPL string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
