package sandbox

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"box/internal/config"
)

// buildBwrap computes the bwrap command line; exe is the bwrap executable.
func buildBwrap(prof config.Profile, opts Options, exe string) (*Plan, error) {
	plan := &Plan{Name: filepath.Base(opts.Binary), Exe: exe}
	mounts, err := collect(prof, &opts, plan, false)
	if err != nil {
		return nil, err
	}

	args := []string{"--unshare-all"}
	if prof.Network {
		args = append(args, "--share-net")
	}
	if prof.DisableUserns {
		// --unshare-all only tries to unshare the user namespace;
		// --disable-userns insists on it.
		args = append(args, "--unshare-user", "--disable-userns")
	}
	if prof.Hostname != "" {
		args = append(args, "--hostname", prof.Hostname)
	}
	if prof.DieWithParent {
		args = append(args, "--die-with-parent")
	}
	if prof.NewSession {
		args = append(args, "--new-session")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	for _, m := range mounts {
		args = append(args, m.args()...)
	}
	args = append(args, "--chdir", opts.Cwd)
	if prof.ClearEnv {
		args = append(args, "--clearenv")
		for _, kv := range passEnv(prof) {
			k, v, _ := strings.Cut(kv, "=")
			args = append(args, "--setenv", k, v)
		}
	}
	for _, k := range sortedKeys(prof.Env) {
		args = append(args, "--setenv", k, prof.Env[k])
	}
	for _, k := range prof.UnsetEnv {
		args = append(args, "--unsetenv", k)
	}
	if plan.Proxy != nil {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			if _, set := prof.Env[k]; !set {
				args = append(args, "--setenv", k, "http://"+ProxyAddr)
			}
		}
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			if _, set := prof.Env[k]; !set {
				args = append(args, "--setenv", k, "localhost,127.0.0.1,::1")
			}
		}
	}
	args = append(args, prof.ExtraArgs...)
	if plan.Proxy != nil {
		// The forwarder exposes the proxy socket on loopback, then runs
		// the command as its child.
		args = append(args, "--", plan.Self, "_forward", "-s", ProxySocket, "-l", ProxyAddr)
	}
	args = append(args, "--", opts.Binary)
	args = append(args, opts.Args...)
	plan.Args = args
	return plan, nil
}

func (m *mount) args() []string {
	switch m.kind {
	case kindRO:
		return []string{"--ro-bind", m.src, m.dest}
	case kindRW:
		return []string{"--bind", m.src, m.dest}
	case kindDev:
		return []string{"--dev-bind", m.src, m.dest}
	case kindTmpfs:
		return []string{"--tmpfs", m.dest}
	case kindSymlink:
		return []string{"--symlink", m.src, m.dest}
	case kindOverlay:
		if m.upper == "" {
			return []string{"--overlay-src", m.src, "--tmp-overlay", m.dest}
		}
		return []string{"--overlay-src", m.src, "--overlay", m.upper, m.work, m.dest}
	case kindFile:
		return []string{"--perms", fmt.Sprintf("%04o", m.mode), "--file", strconv.Itoa(m.fd), m.dest}
	}
	return nil
}
