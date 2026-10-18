package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deric/box/internal/config"
)

// SSH inside a bwrap sandbox.
//
// The sandbox has no network of its own, so ssh cannot connect anywhere by
// itself; what it can do is run a ProxyCommand. With ssh = true box binds a
// generated ssh_config into the sandbox whose ProxyCommand is `box
// _connect %h %p`, which asks the proxy for a CONNECT tunnel to the host
// over its unix socket and relays the connection, so ssh reaches
// allow_hosts on port 22 the way HTTPS clients do on 443 (see
// proxy.Connect). The proxy only lets CONNECT through to port 22 when the
// sandbox has ssh = true; otherwise the port is refused for every host.
// The generated file is bound over the host's /etc/ssh/ssh_config (the
// user's own ~/.ssh/config, read first, still wins), with the host's
// content appended so its defaults keep applying; a host without that
// file gets the generated one at ~/.ssh/config instead. The key named by
// ssh_key is mounted read-only at its own path and set as IdentityFile,
// ssh_user as User, and a copy of the host's ~/.ssh/known_hosts is placed
// inside so known hosts are trusted and new ones can be added without
// touching the host's file. With network = true or pasta = true ssh
// connects directly and only the key, user and known hosts are set up;
// with pasta the generated configuration (without key or user) and the
// known hosts are set up even with ssh = false, as the host's Include
// lines would otherwise keep ssh from running at all inside.

// sshConfig is the host's system-wide client configuration, which the
// generated one is bound over; tests replace it.
var sshConfig = "/etc/ssh/ssh_config"

// sshMounts returns the mounts an ssh-enabled sandbox needs: the key, the
// known hosts copy and the generated configuration (see above). It marks
// the proxy, if there is one, to let ssh through.
func sshMounts(prof config.Profile, opts *Options, plan *Plan) ([]*mount, error) {
	if !prof.Network && plan.Proxy == nil {
		plan.Warnings = append(plan.Warnings, "ssh ignored: the sandbox has no network (proxy = false)")
		return nil, nil
	}
	exp := expander(*opts)
	var mounts []*mount
	var key string
	if prof.SSHKey != "" {
		if m := sshKeyMount(exp, prof.SSHKey, plan); m != nil {
			key = m.dest
			mounts = append(mounts, m)
		}
	}
	if m := copyFile(exp, "?~/.ssh/known_hosts", plan); m != nil {
		mounts = append(mounts, m)
	}

	var b strings.Builder
	switch {
	case plan.proxyInside():
		b.WriteString("# Written by box: ssh connections go through its proxy to allow_hosts.\n")
	case plan.Pasta != nil:
		b.WriteString("# Written by box: ssh connects through pasta; only allow_hosts resolve.\n")
	default:
		b.WriteString("# Written by box.\n")
	}
	b.WriteString("Host *\n")
	if plan.proxyInside() {
		plan.Proxy.SSH = true
		b.WriteString("\tProxyCommand " + shellQuote(opts.Self) + " _connect %h %p\n")
	}
	if key != "" {
		b.WriteString("\tIdentityFile " + key + "\n")
	}
	if prof.SSHUser != "" {
		b.WriteString("\tUser " + prof.SSHUser + "\n")
	}
	dest := filepath.Join(opts.Home, ".ssh", "config")
	mode := os.FileMode(0o600)
	if host, err := os.ReadFile(sshConfig); err == nil {
		dest, mode = realPath(sshConfig), 0o644
		b.WriteString("\n# The host's " + sshConfig + " follows.\n")
		b.WriteString(withoutIncludes(string(host)))
	}
	m, err := dataMount(plan, dest, b.String(), mode)
	if err != nil {
		return nil, fmt.Errorf("ssh config: %w", err)
	}
	return append(mounts, m), nil
}

// withoutIncludes returns the ssh_config text conf with its Include lines
// commented out. ssh insists that an included file belongs to root or to
// the user, and inside the sandbox's user namespace a root-owned file
// (every file under /etc/ssh/ssh_config.d) appears to belong to nobody,
// which makes ssh fail on the spot; the directives are kept visible so the
// omission can be seen.
func withoutIncludes(conf string) string {
	lines := strings.Split(strings.TrimSuffix(conf, "\n"), "\n")
	for i, line := range lines {
		word := strings.ToLower(strings.TrimLeft(line, " \t"))
		if rest := strings.TrimPrefix(word, "include"); len(rest) < len(word) &&
			(rest == "" || strings.ContainsAny(rest[:1], " \t=")) {
			lines[i] = "# skipped by box (root-owned files appear owned by nobody inside, which ssh refuses to include): " +
				strings.TrimSpace(line)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// sshKeyMount exposes the private key at raw read-only at its own path.
// A symlink is followed on the host, since its target need not be visible
// inside; a missing or unusable key is skipped with a warning (silently
// with the ? prefix), and the configuration then names no identity.
func sshKeyMount(expanded func(string) (string, bool), raw string, plan *Plan) *mount {
	src, optional := expanded(raw)
	if src == "" {
		return nil
	}
	real, err := filepath.EvalSymlinks(src)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(real)
	}
	if err != nil {
		if !optional {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("ssh_key %s skipped: %v", src, err))
		}
		return nil
	}
	if !fi.Mode().IsRegular() {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("ssh_key %s skipped: not a regular file", src))
		return nil
	}
	return &mount{kind: kindRO, src: real, dest: src, visible: true}
}
