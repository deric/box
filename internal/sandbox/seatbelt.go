package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"

	"box/internal/config"
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
(allow network-outbound (literal "/private/var/run/mDNSResponder"))
(allow mach-lookup
  (global-name "com.apple.dnssd.service")
  (global-name "com.apple.networkd")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.trustd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.ocspd"))
`

// buildSeatbelt computes a sandbox-exec invocation whose profile grants the
// access the mounts describe; exe is the sandbox-exec executable. Seatbelt
// only filters access to the real filesystem, so tmpfs paths are hidden
// rather than replaced and overlays are read-only (collect warns).
func buildSeatbelt(prof config.Profile, opts Options, exe string) (*Plan, error) {
	plan := &Plan{Name: filepath.Base(opts.Binary), Exe: exe}
	mounts, err := collect(prof, &opts, plan, true)
	if err != nil {
		return nil, err
	}
	if prof.Hostname != "" {
		plan.Warnings = append(plan.Warnings, "hostname is not supported by sandbox-exec; ignored")
	}
	if prof.NewSession {
		plan.Warnings = append(plan.Warnings, "new_session is not supported by sandbox-exec; ignored")
	}
	if prof.DisableUserns {
		plan.Warnings = append(plan.Warnings, "disable_userns is not supported by sandbox-exec; ignored")
	}

	var b strings.Builder
	b.WriteString(seatbeltBase)
	if prof.Network {
		b.WriteString(seatbeltNetwork)
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
	for _, k := range sortedKeys(prof.Env) {
		plan.Env = append(plan.Env, k+"="+prof.Env[k])
	}
	plan.Env = append(plan.Env, NameEnv+"="+plan.Name, DirEnv+"="+opts.Cwd)
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
