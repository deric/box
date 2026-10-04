package sandbox

import (
	"path/filepath"

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
	}
	for _, k := range sortedKeys(prof.Env) {
		args = append(args, "--setenv", k, prof.Env[k])
	}
	for _, k := range prof.UnsetEnv {
		args = append(args, "--unsetenv", k)
	}
	args = append(args, prof.ExtraArgs...)
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
	}
	return nil
}
