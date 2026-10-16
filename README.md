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
box config claude               # print the effective, merged profile
box run podman build -t app .   # build an image in a sandbox of its own (see Podman)
box clean claude                # drop claude's overlay layers, podman images and stale private /tmp dirs
box df                          # disk usage per binary: overlay layers, podman images, private /tmp dirs, logs
box df -a claude                # the same listing every layer, /tmp dir and log of claude
box ps                          # list running sandboxes with process count, CPU, memory
box top                         # the same as a live view that refreshes every second, plus what the proxy denied
box info                        # print version, isolation mechanism, config path, log dir, running sandboxes
```

## Defaults (Linux)

| Host path                        | Inside the sandbox                                   |
|----------------------------------|------------------------------------------------------|
| `/usr`, `/bin`, `/lib*`, `/etc`  | read-only (merged-/usr symlinks are recreated)       |
| current directory                | read-write                                           |
| `/tmp`                           | private: a fresh host dir `/tmp/box/<name>-<pid>`    |
| `/var/tmp`                       | empty tmpfs, 1 GiB at most                           |
| `$HOME`                          | empty tmpfs, 1 GiB at most; only listed paths underneath are visible |
| `~/.local/share/mise`, `~/.local/bin` | overlay: host content visible, writes land in a per-binary upper layer under `~/.local/state/box/overlays/` |
| `~/.claude` (claude only)        | temporary overlay: host content visible, writes discarded on exit; `projects/` and `.credentials.json` read-write |
| `~/.claude/skills`, `~/.claude/plugins`, `~/.agents/skills` (claude only) | persistent overlays: the host's skills and plugins are visible, ones installed inside land in the per-binary upper layer |
| `~/.claude.json` (claude only)   | private copy of the host file                        |
| network                          | own empty namespace; HTTP(S) only through an allowlisting proxy on the host (`allow_hosts`); DNS through the proxy to the host's resolver (`dns`) |
| `/proc`, `/dev`                  | fresh, minimal                                       |
| PID, IPC, UTS, cgroup namespaces | unshared; hostname is `box`                          |
| user namespaces                  | cannot be created inside (`--disable-userns`)        |
| terminal                         | new session (`--new-session`)                        |
| CPU, memory                      | 1 core and 2 GiB, no swap: a transient systemd scope (cgroup) of its own, see [Resource limits](#resource-limits) |
| environment                      | cleared; only `pass_env` variables (`HOME`, `PATH`, `TERM`, `LANG`, `LC_*`, proxies, ...) and `BOX_SANDBOX=1` are set |

Everything is driven by `--unshare-all`; `BOX_SANDBOX=1` lets programs detect
they are sandboxed.

### Resource limits

Every sandbox runs in a cgroup of its own with a CPU and a memory limit
(`cpus`, `memory`), and each tmpfs mount is capped in size (`tmpfs_size`).
Since an unprivileged process cannot write the cgroup it was started in,
`box` asks the systemd user manager for a transient scope unit named
`box-<name>-<pid>.scope` with the limits as properties and its own PID as
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
session, nested `box`) it warns and runs without limits.

`tmpfs_size` is passed as `bwrap --size` before every `--tmpfs`, so
`$HOME` and `/var/tmp` (and podman's runtime directory) each hold at most
that much. tmpfs lives in memory and counts towards `memory`, as does the
root filesystem of the sandbox, which is a tmpfs too. The private `/tmp`
is a host directory, which no unprivileged mechanism can cap; to bound it
as well, set `private_tmp = false` and add `/tmp` to `tmpfs`, at the price
of inspecting it from the host.

### Network

The sandbox has no network of its own. Instead `box` starts a small HTTP
proxy on the host, listening on a unix socket (`/tmp/box/<name>-<pid>.sock`)
that is bound into the sandbox at `/run/box/proxy.sock`. Inside, `box`
exposes it on `127.0.0.1:3128` and sets `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY` accordingly, then runs the command as its child. The proxy
handles `CONNECT` (HTTPS) and plain HTTP requests and only lets them through
to hosts on `allow_hosts`: an entry is an exact name, `*.suffix` for anything
under a domain, or `*`. Everything else gets `403 Forbidden` and a line in
`<log_dir>/<name>-<pid>-<dir>.log` (`log_dir` defaults to `/tmp/box`), where
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

## Configuration

`box init` writes a commented default file. If the file already exists it is
left untouched and a unified diff against the built-in defaults is printed
instead; pass `-f` to overwrite it. The `[default]` section applies to
every binary; `[binaries.<name>]` (the binary's base name) overrides it. List
values are **appended** to the defaults, scalar values **replace** them, and
`drop_binds` removes inherited entries from any mount list.

```toml
[default]
ro_binds = ["/bin", "/sbin", "/lib", "?/lib32", "/lib64", "/usr", "/etc", "?/opt"]
rw_binds = ["$PWD"]
tmpfs    = ["$HOME", "/var/tmp"]
private_tmp = true
overlays = [{ path = "~/.local/share/mise", persist = true }, { path = "?~/.local/bin", persist = true }]
network  = false
proxy    = true
dns      = true
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
| `dev_binds`       | list              | device paths (`--dev-bind`), e.g. `/dev/dri`                   |
| `tmpfs`           | list              | fresh tmpfs mounts                                             |
| `overlays`        | list              | `"path"` or `{ path, persist }`; `persist = false` discards writes on exit |
| `copy_files`      | list              | host files copied into the sandbox (`--file`); changes made inside never reach the host |
| `sync_files`      | list              | like `copy_files`, but the copy is written back over the host file when the command exits |
| `drop_binds`      | list              | inherited entries to remove (binary sections only)             |
| `network`         | bool              | share the host network; `false` gives an empty namespace       |
| `proxy`           | bool              | with `network = false`: reach `allow_hosts` through a proxy on the host |
| `allow_hosts`     | list              | hosts the proxy lets through: `name`, `*.suffix` or `*`        |
| `log_dir`         | string            | directory for the proxy's denied-request logs (default `/tmp/box`) |
| `hostname`        | string            | hostname inside the sandbox; empty keeps the host's            |
| `new_session`     | bool              | `--new-session` (blocks TIOCSTI, breaks shell job control)     |
| `disable_userns`  | bool              | `--disable-userns`: no user namespaces inside, so no nested sandboxes |
| `die_with_parent` | bool              | kill the sandbox when `box` dies                               |
| `clear_env`       | bool              | start from an empty environment                                |
| `pass_env`        | list              | with `clear_env`: host variables to pass through, names or patterns (`LC_*`) |
| `env`             | table             | variables to set                                               |
| `unset_env`       | list              | variables to unset (also removes `pass_env` entries)           |
| `bind_binary`     | bool              | mount the resolved executable if no other mount exposes it     |
| `podman`          | bool              | set the sandbox up for rootless podman inside (see [Podman](#podman)); implies `disable_userns = false` |
| `cpus`            | number            | CPU time per second in cores (`1.5` is one and a half); `0` lifts the limit |
| `memory`          | string            | memory limit, swap excluded, as a size (`2G`, `512M`, `1.5G`, bytes); `""` lifts it |
| `tmpfs_size`      | string            | size of each tmpfs mount, as a size; `""` leaves the kernel default |
| `extra_args`      | list              | raw arguments appended to `bwrap` / `sandbox-exec`             |
| `seatbelt_rules`  | list              | raw SBPL rules appended to the macOS profile                   |
| `inherit`         | bool              | `false` ignores `[default]` for this binary                    |

Locations: config at `$BOX_CONFIG`, else `$XDG_CONFIG_HOME/box/box.toml`, else
`~/.config/box/box.toml`; overlay layers at `$XDG_STATE_HOME/box`, else
`~/.local/state/box`.

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
- binds `~/.local/state/box/podman/<name>` read-write at the same path:
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
| `copy_files`, `sync_files`, `private_tmp`, `proxy` | not supported (warned and ignored)               |
| `network = false`: own empty netns    | no IP traffic at all, loopback included                       |
| Unix sockets under mounted paths      | Unix sockets under exposed paths                              |
| PID / IPC namespaces                  | processes may only inspect and signal their own sandbox; Mach services are limited to a short list (name lookup, logging, and with network: DNS and TLS trust) |
| `hostname`, `new_session`, `disable_userns` | not supported (warned and ignored)                      |
| `cpus`, `memory`, `tmpfs_size`        | not supported (warned and ignored)                            |
| `clear_env`, `pass_env`, `env`, `unset_env` | applied by `box` before starting `sandbox-exec`         |

The macOS default config (written by `box init` on a Mac) exposes the system
directories, Homebrew, the current directory, `/tmp` and `$TMPDIR`, and hides
`$HOME`. Programs that expect a writable `$HOME` (caches, lock files) need the
relevant paths added to `rw_binds`. Claude Code's section additionally exposes
`~/Library/Keychains`, where its login is stored. Anything else a program needs
(a Mach service, an IOKit class) can be granted with `seatbelt_rules`, for
example `'(allow mach-lookup (global-name "com.apple.pasteboard.1"))'`.

Inside a macOS sandbox `BOX_NAME` and `BOX_DIR` are set to the binary's name
and the working directory; `box ps` finds sandboxes by these, since
`sandbox-exec` leaves no wrapper process behind.

`sandbox-exec` is marked deprecated by Apple but ships with every macOS
release and is what other agent sandboxes build on.

## Requirements

- macOS: nothing beyond the system's `/usr/bin/sandbox-exec`.
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
  signals and the terminal behave as if the program ran directly. With the
  proxy enabled it first forks the proxy process, which is told to die with
  it (`bwrap` inherits that role); inside the sandbox the command runs as a
  child of the forwarder, which passes signals and the exit status through.
- `box` sets bwrap's `argv[0]` to `box:<name>`, which is how `box ps` finds
  its sandboxes; stats cover the whole process tree under that bwrap.
- `box top` is `box ps` as a full-screen view that refreshes every second
  (`-i 2s` changes that, `+`/`-` adjust it while running). `CPU%` is the
  tree's CPU time per wall-clock time since the previous refresh, so `100`
  is one busy core; a sandbox seen for the first time shows its lifetime
  average. Sort with `c` (CPU), `m` (memory), `p` (processes), `t` (uptime)
  or `n` (name), `r` reverses, `q` quits. A name argument limits the view to
  one binary, as with `box ps`.
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
  inside lose job control.
- `copy_files` opens each file before `exec` and hands the descriptor to
  `bwrap`, which writes a copy at the same path with the host file's mode
  (`box show` prints the matching `N<file` redirection). `sync_files` does
  the same and additionally binds the host original read-write under
  `/run/box/sync/`; when the command exits, the `box _forward` wrapper that
  runs it compares the copy with the original and, if they differ, rewrites
  the original in place (same inode, mode and owner). This suits files their
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
  directory `/tmp/box/<name>-<pid>` mounted as `/tmp`, where `<pid>` is the
  PID `box ps` shows. Nothing other host processes put in `/tmp` (X11
  sockets, for example) is exposed, while the sandbox's temporary files stay
  inspectable from the host. Because `box` execs into `bwrap` nothing removes
  the directory on exit; `box clean` deletes those whose sandbox is gone, and
  a leftover from a reused PID is cleared before the next run. To share the
  host's `/tmp` instead, set `private_tmp = false` and add `/tmp` to
  `rw_binds` as in the example above. Seatbelt cannot remount paths, so the
  option is ignored on macOS.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
