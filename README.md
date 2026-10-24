# box - A sandbox for running agents

`box` runs a program inside a [bubblewrap](https://github.com/containers/bubblewrap)
(`bwrap`) sandbox on Linux, or a `sandbox-exec` (Seatbelt) sandbox on macOS,
with sensible defaults, configured per binary from `~/.config/box/box.toml`.
See [macOS](#macos) for how the two differ.

## Getting started

```sh
box init                        # writes config into ~/.config/box/box.toml
box run claude                  # start `claude` in a sandbox
```

```sh
box run claude --some-flag      # run claude sandboxed
box show claude                 # print the sandbox command line without running it
box config claude               # print the effective, merged profile (and the profiles that apply here)
box run podman build -t app .   # build an image in a sandbox of its own (see Podman)
box clean claude                # drop claude's overlay layers, podman images and stale private /tmp dirs
box df                          # disk usage per binary: overlay layers, podman images, private /tmp dirs, logs
box df -a claude                # the same listing every layer, /tmp dir and log of claude
box ps                          # list running sandboxes by name (the working directory's) with process count, CPU, memory
box top                         # the same as a live view that refreshes every second, plus what the proxy denied
box ps api                      # only the sandbox named api, or those running a binary called api
box info                        # print version, isolation mechanism, config path, log dir, running sandboxes
box doctor                      # check the host for everything box needs and say how to fix what is missing
```

## Defaults (Linux)

| Host path                        | Inside the sandbox                                   |
|----------------------------------|------------------------------------------------------|
| `/usr`, `/bin`, `/lib*`, `/etc`  | read-only (merged-/usr symlinks are recreated)       |
| current directory                | read-write                                           |
| `/tmp`                           | private: a fresh host dir `/tmp/box/<binary>-<pid>`  |
| `/var/tmp`                       | empty tmpfs, 1 GiB at most                           |
| `$HOME`                          | empty tmpfs, 1 GiB at most; only listed paths underneath are visible |
| `~/.local/share/mise`, `~/.local/state/mise`, `~/.local/bin` | overlay: host content visible, writes land in a per-binary upper layer under `~/.local/state/box/overlays/`; the state dir carries mise's `trusted-configs/`, so configs trusted on the host are trusted inside and `mise trust` inside never trusts anything on the host |
| `~/.claude` (claude only)        | temporary overlay: host content visible, writes discarded on exit; `projects/` and `.credentials.json` read-write |
| `~/.claude/skills`, `~/.claude/plugins`, `~/.agents/skills` (claude only) | persistent overlays: the host's skills and plugins are visible, ones installed inside land in the per-binary upper layer |
| `~/.claude.json` (claude only)   | private copy of the host file                        |
| network                          | own empty namespace; HTTP(S) only through an allowlisting proxy on the host (`allow_hosts`); DNS through the proxy to the host's resolver (`dns`); or, with `pasta`, an interface of its own whose firewall only admits what the allowed names resolve to |
| `/proc`, `/dev`                  | fresh, minimal                                       |
| PID, IPC, UTS, cgroup namespaces | unshared; hostname is `box`                          |
| user namespaces                  | cannot be created inside (`--disable-userns`)        |
| terminal                         | a pseudo-terminal of its own, relayed to the host's (see [Terminal](#terminal)); without one, a new session (`--new-session`) |
| CPU, memory                      | 1 core and 2 GiB, no swap: a transient systemd scope (cgroup) of its own, see [Resource limits](#resource-limits) |
| environment                      | cleared; only `pass_env` variables (`HOME`, `PATH`, `TERM`, `LANG`, `LC_*`, proxies, ...) and `BOX_SANDBOX=1` are set |

Everything is driven by `--unshare-all`; `BOX_SANDBOX=1` lets programs detect
they are sandboxed.

### Resource limits

Every sandbox runs in a cgroup of its own with a CPU and a memory limit
(`cpus`, `memory`), and each tmpfs mount is capped in size (`tmpfs_size`).
Since an unprivileged process cannot write the cgroup it was started in,
`box` asks the systemd user manager for a transient scope unit named
`box-<binary>-<pid>.scope` with the limits as properties and its own PID as
member, which is what `systemd-run --user --scope` does, and then replaces
itself with `bwrap` as usual; the scope covers the sandboxed program,
everything it starts and the host-side proxy, and ends with them.
`box show` prints the equivalent `systemd-run` prefix in front of the
`bwrap` command line, `box info` reports whether the user manager is
reachable and which controllers are delegated to it, and
`systemd-cgtop --user` or `systemctl --user status box-claude-1234.scope`
show the usage. A memory limit comes with `MemorySwapMax=0`, so swap cannot
stretch it: a sandbox that outgrows it is OOM-killed instead. Once the
scope exists `box` reads the limits back from cgroupfs and warns about any
the kernel does not show, which happens when a controller is not delegated
to the user manager. Without a reachable user manager (no systemd, no
session, nested `box`), or with one that does not answer within ten
seconds, it warns and runs without limits.

`tmpfs_size` is passed as `bwrap --size` before every `--tmpfs`, so
`$HOME` and `/var/tmp` (and podman's runtime directory) each hold at most
that much. tmpfs lives in memory and counts towards `memory`, as does the
root filesystem of the sandbox, which is a tmpfs too. The private `/tmp`
is a host directory, which no unprivileged mechanism can cap; to bound it
as well, set `private_tmp = false` and add `/tmp` to `tmpfs`, at the price
of inspecting it from the host.

### Network

The sandbox has no network of its own. Instead `box` starts a small HTTP
proxy on the host, listening on a unix socket (`/tmp/box/<binary>-<pid>.sock`)
that is bound into the sandbox at `/run/box/proxy.sock`. Inside, `box`
exposes it on `127.0.0.1:3128` and sets `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY` accordingly, then runs the command as its child. The proxy
handles `CONNECT` (HTTPS) and plain HTTP requests and only lets them through
to hosts on `allow_hosts`: an entry is an exact name, `*.suffix` for anything
under a domain, or `*`. Everything else gets `403 Forbidden` and a line in
`<log_dir>/<binary>-<pid>-<dir>.log` (`log_dir` defaults to `/tmp/box`), where
`<dir>` is the working directory without its leading slash and with the other
slashes replaced by dashes. `box info` shows the log directory in use,
`box top` shows the denied requests of the running sandboxes grouped by
target host in its lower pane, and `box clean` removes logs of sandboxes that
have exited. Names that match only through a wildcard may not
resolve to loopback or link-local addresses, so a `*` entry does not reach
services on the host. Programs that ignore the proxy variables have no
network at all. `box show` prints the proxy command along with the `bwrap`
command line.

Name resolution works inside anyway (`dns = true`, the default): the sandbox's
`/etc/resolv.conf` names `127.0.0.1`, where `box` answers DNS and relays
every query through the same proxy to the nameservers of the host's
`/etc/resolv.conf`, over UDP or TCP as the query came in. Nothing inside a
`bwrap` sandbox may bind port 53, so `box` creates the sandbox's network
namespace itself: it runs `unshare -U -n --keep-caps` (util-linux) first,
binds the listeners there with `box _netns`, drops its capabilities and only
then starts `bwrap` with `--share-net`; the sandboxed program has no
capabilities, as before, and the listeners are served by the `box _forward`
wrapper inside. The answers are not filtered: a program can look up any
name, although it can only connect to `allow_hosts`. Set `dns = false` to
leave a sandbox without a resolver, which closes that channel. Where the
namespaces cannot be set up (no `unshare`, user namespaces disabled) `box`
warns and runs the sandbox without DNS; `box info` reports what it found.

On macOS the same proxy runs on a loopback port of the host instead of a
unix socket, and the Seatbelt profile lets the sandbox connect to that port
and nothing else; see [macOS](#macos).

#### The pasta backend

The proxy only carries HTTP, HTTPS and (with `ssh = true`) SSH, and only
for programs that honour the proxy variables. With `pasta = true` the
sandbox gets a network interface of its own instead, from
[pasta](https://passt.top) (package `passt`), which translates the
sandbox's traffic into ordinary sockets on the host without privileges, so
any protocol works inside: ssh, git over ssh, gRPC, QUIC, WebSockets, and
programs that know nothing of proxies. `allow_hosts` is then enforced by
name resolution. `box` loads an nftables firewall into the sandbox's
network namespace (with `nft`, package `nftables`) that rejects every
outgoing connection except to loopback and to the addresses in its allow
sets, and runs its own resolver there, outside the sandbox proper and
with `CAP_NET_ADMIN` kept: it answers only queries for names on
`allow_hosts`, relaying them to the host's resolver through the proxy
process as before, and admits the addresses it hands out into the allow
sets for their time to live (15 minutes at least); established
connections keep going after that. Every other name gets `NXDOMAIN` and a
`denied DNS <name>` line in the log, which `box top` counts like a denied
request, and a connection to an address that was never resolved is
rejected on the spot. As with the proxy, a name that matches only through
a wildcard may not resolve to a loopback or link-local address.

pasta is told to forward no port in either direction and not to map the
host's loopback, so nothing on the host is reachable from inside and
nothing inside is exposed, and the sandboxed program runs in bwrap's
nested user namespace, which has no say over the firewall. The proxy
still runs on the host, but its socket is not bound into the sandbox and
no proxy variables are set. Since the firewall works by address, every
port of an allowed host is reachable: `ssh = false` does not keep ssh
out, so the generated ssh_config (which comments the host's `Include`
lines out, see [SSH](#ssh)) and the known hosts copy are set up either
way, and `ssh = true` only adds the key and user, as with
`network = true`. The backend needs `pasta`, `nft`, `unshare` and a
kernel with nftables and conntrack available to an unprivileged namespace
(the `nf_tables`, `nft_ct`, `nf_conntrack` and `nft_reject_inet` modules
loaded, which any host running a firewall has). `box doctor` checks all
of that and attaches pasta to a throwaway namespace; where something is
missing the sandbox falls back to the proxy with a warning. `box show`
prints the `box _netns -pasta` step that prepares the namespace.

### Terminal

When `box run` is started on a terminal, the sandbox gets a pseudo-terminal
of its own and `box` stays on the host relaying the two: keystrokes go in
unchanged, output comes back, and whenever the terminal is resized the new
size is passed on, so programs inside (Claude Code, editors, pagers) get
`SIGWINCH` and redraw. The sandbox runs as the leader of a new session with
that pseudo-terminal as its controlling terminal, which also means `^C`
reaches the program as `SIGINT`, `/dev/tty` can be opened, and a shell
inside has job control (`fg`, `bg`, `^Z`). `--new-session` is not passed
in that case: `TIOCSTI` on the sandbox's terminal only types into the
sandbox itself. `^Z` typed while no shell inside does job control suspends
`box` as a whole, and `fg` resumes it. The relay works the same on macOS.

Without a terminal (stdin or stdout redirected) or with `box run -T`, the
command runs on the host's descriptors as before, under `--new-session`
when the profile says so. `box` exits with the command's status either way.

## Configuration

`box init` writes a commented default file and creates the `profile.d/`
directory next to it (see [Profiles](#profiles)). If the file already exists
it is left untouched and a unified diff against the built-in defaults is
printed instead; pass `-f` to overwrite it. The `[default]` section applies to
every binary; `[binaries.<name>]` (the binary's base name) overrides it. List
values are **appended** to the defaults, scalar values **replace** them, and
`drop_binds` removes inherited entries from any mount list.

```toml
[default]
ro_binds = ["/bin", "/sbin", "/lib", "?/lib32", "/lib64", "/usr", "/etc", "?/opt"]
rw_binds = ["$PWD"]
tmpfs    = ["$HOME", "/var/tmp"]
private_tmp = true
overlays = [
  { path = "~/.local/share/mise", persist = true },
  { path = "?~/.local/state/mise", persist = true },
  { path = "?~/.local/bin", persist = true },
]
network  = false
proxy    = true
dns      = true
pasta    = false
allow_hosts = ["github.com", "*.github.com", "registry.npmjs.org"]
cpus     = 1
memory   = "2G"
tmpfs_size = "1G"
hostname = "box"
new_session    = true
disable_userns = true
clear_env = true
pass_env  = ["HOME", "USER", "PATH", "SHELL", "TERM", "LANG", "LC_*"]
env       = { BOX_SANDBOX = "1" }

[binaries.claude]
overlays   = [
  { path = "?~/.claude", persist = false },
  { path = "?~/.claude/skills", persist = true },
  { path = "?~/.claude/plugins", persist = true },
  { path = "?~/.agents/skills", persist = true },
]
rw_binds   = ["?~/.claude/projects", "?~/.claude/.credentials.json"]
sync_files = ["?~/.claude.json"]
pass_env   = ["ANTHROPIC_*", "CLAUDE_*"]
allow_hosts = ["*.anthropic.com", "claude.ai", "*.claude.ai"]

[binaries.trusted-tool]
private_tmp = false          # share the host's /tmp instead
rw_binds    = ["/tmp"]
network     = true           # share the host network, no proxy

[binaries.untrusted-tool]
proxy      = false           # no network at all
inherit    = true            # set false to ignore [default] entirely

[binaries.quiet-tool]
dns        = false           # proxy only, no name resolution

[binaries.native-tool]
pasta      = true            # any protocol to allow_hosts, through pasta

[binaries.build-tool]
cpus       = 4               # more room: four cores, 8 GiB, no tmpfs cap
memory     = "8G"
tmpfs_size = ""
```

Paths accept `~`, `$HOME`, `$PWD` (the directory `box` was started in) and any
other environment variable. A missing path is skipped with a warning; prefix it
with `?` to skip silently.

| Key               | Type              | Meaning                                                        |
|-------------------|-------------------|----------------------------------------------------------------|
| `ro_binds`        | list              | host paths mounted read-only                                   |
| `rw_binds`        | list              | host paths mounted read-write                                  |
| `rw_parent`       | bool              | also mount the parent of the working directory read-write (never `/` or `$HOME`); see [Profiles](#profiles) |
| `dev_binds`       | list              | device paths (`--dev-bind`), e.g. `/dev/dri`                   |
| `tmpfs`           | list              | fresh tmpfs mounts                                             |
| `overlays`        | list              | `"path"` or `{ path, persist }`; `persist = false` discards writes on exit |
| `copy_files`      | list              | host files copied into the sandbox (`--file`); changes made inside never reach the host |
| `sync_files`      | list              | like `copy_files`, but the copy is written back over the host file when the command exits; if the host file changed meanwhile you are shown the diff and asked |
| `drop_binds`      | list              | inherited entries to remove (binary sections only)             |
| `network`         | bool              | share the host network; `false` gives an empty namespace (on macOS: no network but the proxy) |
| `proxy`           | bool              | with `network = false`: reach `allow_hosts` through a proxy on the host |
| `allow_hosts`     | list              | hosts the proxy lets through: `name`, `*.suffix` or `*`        |
| `dns`             | bool              | with `proxy`: let the sandbox resolve names (through the proxy on Linux, the system resolver on macOS); answers are not filtered |
| `pasta`           | bool              | with `proxy`: a network interface of its own through pasta instead of the HTTP proxy, any protocol to `allow_hosts` (Linux only) |
| `log_dir`         | string            | directory for the proxy's denied-request logs (default `/tmp/box`) |
| `hostname`        | string            | hostname inside the sandbox; empty keeps the host's            |
| `new_session`     | bool              | `--new-session` when `box run` is not on a terminal (blocks TIOCSTI, breaks shell job control); on one the sandbox has a terminal of its own, see [Terminal](#terminal) |
| `disable_userns`  | bool              | `--disable-userns`: no user namespaces inside, so no nested sandboxes |
| `die_with_parent` | bool              | kill the sandbox when `box` dies                               |
| `clear_env`       | bool              | start from an empty environment                                |
| `pass_env`        | list              | with `clear_env`: host variables to pass through, names or patterns (`LC_*`) |
| `env`             | table             | variables to set                                               |
| `unset_env`       | list              | variables to unset (also removes `pass_env` entries)           |
| `bind_binary`     | bool              | mount the resolved executable if no other mount exposes it     |
| `podman`          | bool              | set the sandbox up for rootless podman inside (see [Podman](#podman)); implies `disable_userns = false` |
| `ssh`             | bool              | let ssh inside reach `allow_hosts` through the proxy (see [SSH](#ssh)); off by default |
| `ssh_key`         | string            | with `ssh`: host private key mounted read-only at its own path and used as the identity |
| `ssh_user`        | string            | with `ssh`: user name to log in as                             |
| `cpus`            | number            | CPU time per second in cores (`1.5` is one and a half); `0` lifts the limit |
| `memory`          | string            | memory limit, swap excluded, as a size (`2G`, `512M`, `1.5G`, bytes); `""` lifts it |
| `tmpfs_size`      | string            | size of each tmpfs mount, as a size; `""` leaves the kernel default |
| `extra_args`      | list              | raw arguments appended to `bwrap` / `sandbox-exec`             |
| `seatbelt_rules`  | list              | raw SBPL rules appended to the macOS profile                   |
| `inherit`         | bool              | `false` ignores `[default]` for this binary                    |

Locations: config at `$BOX_CONFIG`, else `$XDG_CONFIG_HOME/box/box.toml`, else
`~/.config/box/box.toml`; profiles in `profile.d/` next to it; overlay layers
at `$XDG_STATE_HOME/box`, else `~/.local/state/box`.

### Profiles

A profile is a configuration that applies only when `box` is started in a
directory under certain paths: looser network rules for work projects, a
second checkout a project depends on, a tool that one tree needs. Profiles
live in `~/.config/box/profile.d/` (next to `box.toml`, so `-c FILE` reads
them from next to `FILE`; `box init` creates the directory), one
`<name>.toml` per profile:

```toml
# ~/.config/box/profile.d/work.toml
paths = ["~/work", "/srv/projects"]   # applies under any of these

[default]
rw_parent   = true                    # the parent of the working directory is read-write too
allow_hosts = ["gitlab.example.com", "*.internal.example.com"]
ssh         = true
ssh_key     = "~/.ssh/id_work"

[binaries.claude]
pass_env = ["WORK_*"]
```

`paths` take `~`, `$HOME` and environment variables; a directory matches
when it is one of them or lies underneath (symlinks in the working directory
are resolved as well). The rest of the file has the same sections and keys
as `box.toml` and is merged on top of it by the same rules: `[default]`
first, then `[binaries.<name>]`, lists appended, scalars replaced,
`drop_binds` removing entries inherited from `box.toml`. When several
profiles match, the one with the deepest path is applied last and wins; with
equal depth, file name order decides. `box config claude` names the profiles
that apply in the current directory and prints the result, `box show` lists
them in its output and `box info` the profiles found. A profile that does
not parse, lists no `paths` or uses an unknown key stops `box` with an error
naming the file.

`rw_parent = true` mounts the parent of the working directory read-write in
addition to `$PWD`, for a checkout that builds against its siblings. It is
refused with a warning when that parent is `/` or the home directory, since
either would undo most of the sandbox; a profile is the natural place for it,
as it then applies only under the listed paths.

## Podman

With `podman = true` a sandbox can run rootless `podman` (and `buildah`),
so an agent can build and run containers without leaving it. The built-in
`[binaries.podman]` section enables it for `box run podman ...`; add
`podman = true` to `[binaries.claude]` to let Claude Code build containers
from inside its sandbox. `box` then:

- leaves user namespaces enabled (`disable_userns = false`), since rootless
  podman runs inside a user namespace of its own, and creates that
  namespace for it: the `box _forward` wrapper that runs the command
  starts a pause process in a new user and mount namespace (the sandbox
  user mapped to root) and writes its PID to
  `$XDG_RUNTIME_DIR/libpod/tmp/pause.pid`, which podman joins on every
  invocation;
- mounts `/sys` read-only, which the OCI runtime bind-mounts into containers;
- binds `~/.local/state/box/podman/<binary>` read-write at the same path:
  it holds the per-binary image store (`storage/`) and, written before
  each run, a `containers.conf` and `storage.conf` pointed at by
  `CONTAINERS_CONF_OVERRIDE` and `CONTAINERS_STORAGE_CONF` (an entry in
  `env` for either variable replaces the generated file) and an empty
  `subid` file bound over `/etc/subuid` and `/etc/subgid`. `box df` lists
  the directory and `box clean` removes it; `box show` names the files it
  would write. The host's own images are not shared: they were created
  with a different ID mapping;
- gives the sandbox a private tmpfs at `/run/user/<uid>` as
  `XDG_RUNTIME_DIR` for podman's runtime state.

The generated configuration follows from how the sandbox is built. `bwrap`
maps a single user ID into its user namespace, so the subordinate ID ranges
of `/etc/subuid` cannot be used inside. Left to create its own namespace,
podman would insist on mapping them, log an error and two warnings and
settle for a single mapping; joining the pause process `box` provides
skips all that. The hidden files keep the fallback quiet should the pause
process be gone (`podman system migrate` ends it, and the next command
creates its own, noisily), and `ignore_chown_errors` in `storage.conf`
makes image layers extract with every file owned by root. Images build and run fine
this way; what does not work is running a container as another user
(`USER app`, `--user`) or mapping one in (`--userns=keep-id`). Containers
and `RUN` steps share the sandbox's network namespace (`netns = "host"`),
which has loopback and the proxy only, and inherit `HTTP_PROXY` and
friends, so they reach `allow_hosts` like the sandboxed program itself; the
defaults include Docker Hub, quay.io and ghcr.io. Cgroups are disabled
inside, as none is delegated to the sandbox, so `--memory` and the like
have no effect; the sandbox's own `cpus` and `memory` limits cover the
containers all the same. Native (kernel) overlayfs is used for the store; `fuse-overlayfs`
would need `/dev/fuse` in `dev_binds` and a `storage.conf` of your own.

## SSH

A sandbox cannot open SSH connections by default: it has no network of its
own, and the proxy refuses `CONNECT` requests to port 22 (with a
`403 Forbidden` and a line in the log, like a host that is not on
`allow_hosts`). With `ssh = true` in a binary's section the proxy lets
them through to `allow_hosts`, and `box` binds a generated `ssh_config`
into the sandbox whose `ProxyCommand` is `box _connect %h %p`: it asks the
proxy for a tunnel over its unix socket and relays the connection, so
`ssh`, and `git` over ssh, reach the allowed hosts the way HTTPS clients
do. `ssh_key` names a private key on the host, mounted read-only at its own
path (a symlink is followed on the host) and set as `IdentityFile`;
`ssh_user` sets `User`. A copy of the host's `~/.ssh/known_hosts` is placed
inside, so known hosts are trusted and new ones can be added without
touching the host's file. The host's ssh-agent is not reachable from inside,
so a key with a passphrase prompts for it.

```toml
[binaries.claude]
ssh = true
ssh_key = "~/.ssh/id_ed25519_deploy"
ssh_user = "git"
```

The generated file is bound over `/etc/ssh/ssh_config`, with the host's own
content appended so its defaults keep applying, except for `Include` lines,
which are commented out: ssh refuses to include a file that belongs to
neither root nor the user, and inside the sandbox's user namespace every
root-owned file appears to belong to `nobody`. A `~/.ssh/config` of your
own (added to `ro_binds`) is read first and wins, as usual. On a host
without `/etc/ssh/ssh_config` the generated file goes to `~/.ssh/config`
instead. With `network = true` ssh connects directly and only the key, user
and known hosts are set up; with `proxy = false` there is no network to
reach and `ssh` is ignored with a warning. `box show` prints the generated
file and the `-ssh` flag of the proxy.


## DNS

In order to make the DNS isolation work, updating apparmor profile might be needed.

Check `box doctor` to inspect system capabilities.

```bash
sudo tee /etc/apparmor.d/unshare-userns <<'EOF'
abi <abi/5.0>,
include <tunables/global>

profile unshare /usr/bin/unshare flags=(unconfined) {
  userns,
  include if exists <local/unshare-userns>
}
EOF
sudo apparmor_parser -r /etc/apparmor.d/unshare-userns
```

## macOS

On macOS `box` runs the program under `sandbox-exec` with a generated Seatbelt
profile (`box show <command>` prints it). The profile starts from
`(deny default)`, then grants what the config lists, so the result matches
the Linux sandbox as closely as Seatbelt allows:

| Linux (`bwrap`)                       | macOS (`sandbox-exec`)                                        |
|---------------------------------------|---------------------------------------------------------------|
| `ro_binds` / `rw_binds` / `dev_binds` | read / read-write access to those paths (symlinks such as `/tmp` → `/private/tmp` are resolved) |
| unmounted paths are absent            | unlisted paths are denied; `stat` still works everywhere so path resolution does |
| `tmpfs`: empty, writable, discarded   | hidden: contents can be neither read nor written              |
| `overlays`                            | read-only (with a warning)                                    |
| `private_tmp`: own `/tmp`             | own `/tmp/box/<binary>-<pid>` in `TMPDIR`; the host's `/tmp` and `$TMPDIR` (and the sockets in them) are hidden |
| `copy_files`, `sync_files`, `ssh`, `pasta` | not supported (warned and ignored; `pasta` falls back to the proxy) |
| `network = false`: own empty netns    | no IP traffic at all, loopback included                       |
| `proxy`: unix socket bound into the sandbox, exposed on `127.0.0.1:3128` by the forwarder | the proxy listens on a loopback port of the host, the only address the sandbox may connect to; `HTTP_PROXY` and `HTTPS_PROXY` point at it, `NO_PROXY` is left unset |
| `allow_hosts`, `log_dir`, `box top`   | the same: the proxy is the same program                       |
| `dns`: box's resolver inside, relaying through the proxy | the system resolver (mDNSResponder) is reachable; `dns = false` hides it |
| loopback: the sandbox's own           | the host's: programs may listen on it (and the host reach them), but connect to nothing there except the proxy; list `localhost` in `allow_hosts` to reach a server on the host through the proxy |
| Unix sockets under mounted paths      | Unix sockets under exposed paths                              |
| PID / IPC namespaces                  | processes may only inspect and signal their own sandbox; Mach services are limited to a short list (name lookup, logging, and with network or the proxy: TLS trust and, with `dns`, DNS) |
| `hostname`, `new_session`, `disable_userns` | not supported (warned and ignored)                      |
| `cpus`, `memory`, `tmpfs_size`        | not supported (warned and ignored)                            |
| `clear_env`, `pass_env`, `env`, `unset_env` | applied by `box` before starting `sandbox-exec`         |

The macOS default config (written by `box init` on a Mac) exposes the system
directories, Homebrew and the current directory, gives the sandbox a private
temporary directory, hides `$HOME`, and puts the sandbox behind the proxy
with the same `allow_hosts` as on Linux. Seatbelt cannot put another directory
at `/tmp`, so programs that use a fixed path there instead of `$TMPDIR` fail
until it is added to `rw_binds` and `private_tmp` is turned off, which also
exposes the sockets of the host's `/tmp` (ssh-agent's, tmux's). Programs that expect a writable `$HOME` (caches, lock files) need the
relevant paths added to `rw_binds`. Claude Code's section additionally exposes
`~/Library/Keychains`, where its login is stored. Anything else a program needs
(a Mach service, an IOKit class) can be granted with `seatbelt_rules`, for
example `'(allow mach-lookup (global-name "com.apple.pasteboard.1"))'`.

### Network on macOS

Seatbelt has no network namespaces, and its network filters tell loopback
from everything else but cannot name a host, so the allowlist is enforced
the way it is on Linux: by the proxy, which runs on the host and only lets
`allow_hosts` through. With `network = false` and `proxy = true` (the macOS
defaults) `box` picks a free loopback port, starts `box _proxy -tcp
127.0.0.1:<port>` on it before `sandbox-exec`, and the generated profile
allows outgoing connections to that port and to nothing else: no other IP
traffic, no UDP, no DNS on the wire. `HTTP_PROXY`, `HTTPS_PROXY` (and their
lower-case twins) point at the proxy; programs that ignore them have no
network. Denied requests land in the same log as on Linux and `box top`
shows them. The port is part of the profile, so `box show` prints it. Any
local process may connect to the proxy, since a TCP port has no owner the
way the unix socket does; what gets through it is still only what
`allow_hosts` admits.

Loopback is the host's, not the sandbox's. Programs may bind and accept
connections on it, so a dev server started inside is reachable from a
browser on the host, but they cannot connect to anything there other than
the proxy: neither the host's services nor a server they started
themselves. Since `NO_PROXY` is not set, a request to `localhost` goes
through the proxy like any other and is refused unless `localhost` (or
`127.0.0.1`) is on `allow_hosts`; listing it is what reaches a server on
the host's loopback, as on Linux, where a name that matches only through a
wildcard never does. Binding to all interfaces (`0.0.0.0`) is refused.

With `dns = true` the sandbox can talk to the system resolver,
mDNSResponder, over its socket and XPC service, so `getaddrinfo` works
inside; its answers are not filtered, as on Linux, and `dns = false` hides
the resolver (proxy-aware programs need none, since the proxy resolves the
hosts it connects to). The Mach services TLS trust evaluation uses
(`trustd`, `SecurityServer`, `ocspd`) and path evaluation (`networkd`,
`configd`) stay reachable; none of them opens connections on the sandbox's
behalf. `pasta` and `ssh` are Linux features: `pasta = true` falls back to
the proxy with a warning, `ssh = true` is ignored. `box doctor` compiles the
network rules on the host it runs on, since `sandbox-exec` is deprecated
and a release could stop accepting them.

Inside a macOS sandbox `BOX_NAME`, `BOX_BINARY` and `BOX_DIR` are set to the
sandbox's name, the binary's base name and the working directory; `box ps`
finds sandboxes by these, since `sandbox-exec` leaves no wrapper process
behind.

`sandbox-exec` is marked deprecated by Apple but ships with every macOS
release and is what other agent sandboxes build on.

## Requirements

- macOS: nothing beyond the system's `/usr/bin/sandbox-exec`. `box` runs
  exactly that path and never searches `PATH` for `sandbox-exec`, so a
  program planted earlier in `PATH` cannot replace the sandbox.
- Linux with unprivileged user namespaces.
- For `cpus` and `memory`: a systemd user manager on cgroups v2, reachable
  through `$XDG_RUNTIME_DIR/systemd/private` or the session bus, with the
  `cpu` and `memory` controllers delegated to it (systemd ≥ 239; every
  current distribution). Otherwise `box` warns and runs without limits.
- `bwrap` ≥ 0.8 for `disable_userns`, ≥ 0.10 for overlay mounts (uses
  `--overlay-src`, `--overlay`, `--tmp-overlay`).
  Overlays need a non-setuid `bwrap` and a kernel with unprivileged overlayfs (≥ 5.11).
- `podman` ≥ 4.4 (for `CONTAINERS_CONF_OVERRIDE`) with `crun` or `runc`
  for the `podman` option; its image store also relies on unprivileged
  overlayfs.
- Go 1.24 or newer to build: `go build -o box ./cmd/box`

`box doctor` checks all of this on the host it runs on: it looks for
`bwrap` (or `sandbox-exec`) and its version, starts an empty sandbox (on
macOS also one under the proxy's network rules) and
mounts an overlay inside one, probes `unshare` for DNS and the systemd user
manager and its delegated controllers for limits, loads the configuration
and the profiles, makes sure the state directory and `/tmp/box` are
writable, and looks for the binaries the configuration has sections for,
`podman` with an OCI runtime and `ssh` with the configured keys where a
profile enables them. Every check prints `ok`, `warn` (`box` works with
reduced features) or `FAIL` (`box run` cannot work) with what was found and,
for the latter two, how to fix it; it exits non-zero when anything failed.
Checks of features no profile uses are left out. On a terminal the statuses
are colored unless `NO_COLOR` is set.

### Install with Go

```
go install github.com/deric/box/cmd/box@latest
```

### Debian / Ubuntu

```
sudo apt install bubblewrap
```

DNS inside sandboxes needs `unshare` from util-linux 2.37 or newer, which
every distribution installs by default.

## Development

Tools are pinned in `mise.toml` (`mise install` sets up Go and
[Task](https://taskfile.dev)). Common tasks:

```sh
task build          # build ./box with the git version baked in
task test           # go test ./...
task lint           # gofmt check + go vet
task check          # lint, test, build
task smoke          # run a probe inside a real sandbox
task run -- show sh # build, then run box with the given arguments
task install        # copy the binary to ~/.local/bin (override with INSTALL_DIR=...)
task package        # build tar.gz, deb and rpm packages into ./dist (needs dagger)
task package:check  # install the deb and rpm into fresh containers and run box version
task clean          # remove build artifacts
```

### Packaging

Release artifacts are built with [Dagger](https://dagger.io) (pinned in
`mise.toml`, needs a container runtime) from the module in `.dagger/`:

```sh
dagger call package --version=v1.2.3 export --path=dist
dagger call check --version=v1.2.3        # install the deb/rpm in Debian/Fedora containers
dagger call deb --arch=arm64 export --path=box.deb
dagger call tarball --os=darwin --arch=arm64 export --path=box.tar.gz
```

`package` produces `box_<version>_{linux,darwin}_{amd64,arm64}.tar.gz`
(binary, `README.md`, `LICENSE`), `box_<version>_linux_{amd64,arm64}.deb`
and `.rpm` (installs `/usr/bin/box`, depends on `bubblewrap`) and a
`SHA256SUMS` file. Package metadata lives in `nfpm.yaml`. The version is
baked into `box version`; for the deb/rpm it is normalised to something
their version rules accept (`v1.2.3-4-gabc-dirty` becomes `1.2.3~4.gabc.dirty`,
an untagged commit becomes a prerelease of `0.0.0`). Pushing a `v*` tag runs
the same build in CI and attaches the artifacts to a GitHub release.

## Notes

- `box` replaces itself with `bwrap` (or `sandbox-exec`) via `exec`, so
  signals and the terminal behave as if the program ran directly. On a
  terminal it first starts a copy of itself on a pseudo-terminal, which is
  the one that does so, and stays to relay the terminal and pass the exit
  status on (see [Terminal](#terminal)). With the proxy enabled it forks the
  proxy process before the `exec`, which is told to die with it (`bwrap`
  inherits that role; on macOS the proxy watches its parent instead);
  inside a Linux sandbox the command runs as a child of the forwarder,
  which passes signals and the exit status through, on macOS directly.
- Every sandbox has a name, shown in the `NAME` column of `box ps`: the last
  component of the working directory it was started in (`box` for
  `~/dev/box`), with `-2`, `-3`, ... appended while a running sandbox
  already has that name, so two sandboxes started in the same directory
  are `box` and `box-2`. In the root directory the binary's base name is
  used instead. State on the host (overlay layers, podman store, private
  `/tmp`, logs) stays keyed by the binary.
- `box` sets bwrap's `argv[0]` to `box:<name>`, which is how `box ps` finds
  its sandboxes; stats cover the whole process tree under that bwrap.
- `box top` is `box ps` as a full-screen view that refreshes every second
  (`-i 2s` changes that, `+`/`-` adjust it while running). `CPU%` is the
  tree's CPU time per wall-clock time since the previous refresh, so `100`
  is one busy core; a sandbox seen for the first time shows its lifetime
  average. Sort with `c` (CPU), `m` (memory), `p` (processes), `t` (uptime)
  or `n` (name), `r` reverses, `q` quits. A name argument limits the view to
  one sandbox, or to the sandboxes running that binary, as with `box ps`.
- `box df` shows, per binary, how much disk its sandboxes take on the host:
  the persistent overlay layers, the podman image store, the private `/tmp`
  directories (of running and exited sandboxes alike) and the proxy logs,
  which is what `box clean` removes. Sizes are allocated blocks, as `du` reports them. A name argument
  limits it to one binary; `-a` lists every layer, directory and log on its
  own row instead of the per-binary sums.
- Mounts are applied parents-first, so a tmpfs on `$HOME` never hides a bind
  or overlay placed underneath it.
- The Linux defaults clear the environment and pass through an explicit list
  (`pass_env`), so tokens and session details in the host environment stay
  outside; add what a program needs to its section, as the claude section
  does with `ANTHROPIC_*` and `CLAUDE_*`. `disable_userns` keeps programs
  from building their own namespaces, which also means `box` inside `box`
  and Claude Code's own bwrap-based bash sandbox cannot start; set it to
  `false` in that binary's section if you rely on them. `new_session`
  detaches from the controlling terminal's session, so interactive shells
  inside lose job control; it only applies without a terminal of the
  sandbox's own (see [Terminal](#terminal)).
- `copy_files` opens each file before `exec` and hands the descriptor to
  `bwrap`, which writes a copy at the same path with the host file's mode
  (`box show` prints the matching `N<file` redirection). `sync_files` does
  the same and additionally binds the host original read-write under
  `/run/box/sync/`; when the command exits, the `box _forward` wrapper that
  runs it compares the copy with the original and, if they differ, rewrites
  the original in place (same inode, mode and owner). An original that
  changed on the host while the sandbox ran (another `box` sandbox of the
  same program exiting, or an edit by hand) is never overwritten
  unasked: the wrapper prints a unified diff of the host file against the
  sandbox copy (through `colordiff` or `diff` when the sandbox has one on
  its `PATH`, with a built-in diff otherwise) and asks whether to overwrite
  it or to keep it. Keeping it
  (the default, and what happens without a terminal to ask on) saves the
  sandbox copy as `~/.local/state/box/sync/<name>.<date>-<time>`, e.g.
  `.claude.json.20261019-153000`, for you to merge by hand; that directory
  is bound at `/run/box/saved` inside the sandbox and `box clean` leaves it
  alone. This suits files their
  owner saves by writing a temporary file and renaming it over the original,
  which fails with `EBUSY` on a file that is itself a bind mount. Claude
  Code's `~/.claude.json` (folder trust, MCP servers, account state) is such
  a file, so its defaults use `sync_files` for it and a temporary overlay for
  `~/.claude`: `~/.claude.json`, `~/.claude/projects` (sessions, memory) and
  `~/.claude/.credentials.json` (tokens are refreshed in place) reach the
  host, while other edits under `~/.claude` are lost when the sandbox exits.
  Skills are the exception: `~/.claude/skills` (personal skills and those
  synced from claude.ai), `~/.claude/plugins` (installed plugins and
  marketplaces) and the cross-agent `~/.agents/skills` are persistent
  overlays, so the host's skills are visible and a skill or plugin installed
  inside the sandbox stays in claude's layer under `~/.local/state/box`
  across runs, without being written to the host directories; `box clean
  claude` drops it. Project skills in `.claude/skills` live under `$PWD` and
  are read-write like the rest of it.
- A program that updates itself inside the sandbox may stop starting there.
  claude installs a new version under `~/.local/share/claude/versions`, in
  the tmpfs `$HOME` that is gone on exit, and repoints the `~/.local/bin/claude`
  symlink, which lives in a persistent overlay and so survives in claude's
  layer; the next run execs a dangling symlink and the forwarder inside
  reports `exec: "claude": executable file not found in $PATH`. `box run`
  checks the layer before starting and refuses with a message naming the
  version the symlink points at and the one the host has; run `claude update`
  on the host so it has that version too, or `box clean claude` to drop the
  layer and start over from the host's installation.
  The write-back happens once, on exit: a sandbox that is killed outright
  leaves the host file untouched, and when two sandboxes edit the same file
  the last one to exit wins (a warning is printed when the host file changed
  while the sandbox ran). Since the original is writable from inside the
  sandbox, list only files the program may legitimately change.
- With `private_tmp` (the Linux default) each sandbox gets a fresh host
  directory `/tmp/box/<binary>-<pid>` mounted as `/tmp`, where `<pid>` is the
  PID `box ps` shows. Nothing other host processes put in `/tmp` (X11
  sockets, for example) is exposed, while the sandbox's temporary files stay
  inspectable from the host. Because `box` execs into `bwrap` nothing removes
  the directory on exit; `box clean` deletes those whose sandbox is gone, and
  a leftover from a reused PID is cleared before the next run. To share the
  host's `/tmp` instead, set `private_tmp = false` and add `/tmp` to
  `rw_binds` as in the example above. Seatbelt cannot remount paths, so on
  macOS the directory is not mounted as `/tmp`: `TMPDIR` points at it, and
  the host's `/tmp` and `$TMPDIR` are hidden instead.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
