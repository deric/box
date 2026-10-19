# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `pasta = true` gives a sandbox a network interface of its own through
  [pasta](https://passt.top) instead of the HTTP proxy, so any protocol
  reaches `allow_hosts`: ssh, git over ssh, gRPC, QUIC, and programs that
  ignore proxy variables. The allowlist is enforced by name resolution:
  `box` loads an nftables firewall into the sandbox's network namespace
  that rejects every outgoing connection except to admitted addresses, and
  its resolver inside (`box _filter`, started by `box _netns -pasta` with
  `CAP_NET_ADMIN`) answers only names on `allow_hosts` and admits what they
  resolve to for their time to live. Other names get `NXDOMAIN` and a
  `denied DNS <name>` log line that `box top` counts. pasta forwards no
  ports and does not map the host's loopback, and ssh connects directly:
  the generated ssh_config and known hosts are set up even with
  `ssh = false`, `ssh = true` adds the key and user. Needs `pasta`, `nft`
  and `unshare`; without them the sandbox falls back to the proxy with a
  warning. `box doctor` checks the pieces and attaches pasta to a throwaway
  namespace, `box info` shows a `pasta` line, and `box show` prints the
  `_netns -pasta` step. The default configuration keeps the proxy.

### Changed

- A `sync_files` entry whose host original changed while the sandbox ran
  (another sandbox of the same program exiting, an edit by hand) is no
  longer overwritten with a warning. The `box _forward` wrapper now prints
  a unified diff of the host file against the sandbox copy (`colordiff` or
  `diff` when the sandbox has one, a built-in diff otherwise) and asks
  whether to overwrite the host file or keep it. Keeping it, the default and the
  only outcome without a terminal, saves the sandbox copy under
  `~/.local/state/box/sync/` as `<name>.<date>-<time>`
  (`.claude.json.20261019-153000`) to merge by hand; that directory is
  bound at `/run/box/saved` inside the sandbox, the wrapper gets it with
  `-W`, and `box clean` leaves it alone. Write-back of files that only the
  sandbox changed is unchanged and silent.
- Every sandbox now has a name of its own, shown in the `NAME` column of
  `box ps` and `box top` in place of the binary: the last component of the
  working directory it was started in, with `-2`, `-3`, ... appended while
  a running sandbox already has that name (`box` and `box-2` for two
  sandboxes started in `~/dev/box`). A name argument to `box ps` and
  `box top` matches either the sandbox name or the binary. bwrap's
  `argv[0]` carries the name (`box:<name>`), and on macOS `BOX_NAME` holds
  it with the binary's base name in the new `BOX_BINARY`. Overlay layers,
  the podman store, private `/tmp` directories, logs and the systemd scope
  stay keyed by the binary, as do `box clean` and `box df`.
- `box doctor` no longer suggests reinstalling an `unshare-userns-restrict`
  AppArmor profile when Ubuntu's user namespace restriction keeps `unshare`
  from creating the DNS namespace: no package ships one (Ubuntu 26.04 has
  a profile for `bwrap` only). It now prints a drop-in profile for
  `/usr/bin/unshare` to install instead, with the sysctl as the alternative.

## [0.5.0] - 2026-10-17

### Added

- `box doctor` checks the host for everything `box` needs and says how to
  fix what is missing: `bwrap` (or `sandbox-exec`) and its version, that a
  sandbox can be created and an overlay mounted inside one, `unshare` for
  DNS, the systemd user manager and its delegated controllers for limits,
  the configuration file and profiles, the state and tmp directories, the
  binaries the configuration has sections for, and `podman` with an OCI
  runtime and `ssh` with the configured keys where a profile enables them.
  Each check ends in `ok`, `warn` or `FAIL` with a fix suggestion for the
  latter two, including the distribution's install command; the command
  exits non-zero when any check failed. On a terminal the statuses are
  marked and colored (`NO_COLOR` turns the colors off).

- Profiles: a `<name>.toml` file in `~/.config/box/profile.d/` (next to
  `box.toml`) applies on top of the configuration when `box` is started in a
  directory under one of its `paths`. The rest of the file holds the same
  `[default]` and `[binaries.<name>]` sections as `box.toml`, merged by the
  same rules; of several matching profiles the one with the deepest path
  wins. `box config` names the profiles that apply in the current
  directory, `box show` prints them, `box info` lists the profiles found,
  and `box clean`, `box df` and `box top` consider the `log_dir` of every
  profile. `box init` creates the directory when it is missing, also when
  `box.toml` already exists.

- `rw_parent = true` also mounts the parent of the working directory
  read-write, for checkouts that build against their siblings. It is
  ignored with a warning when that parent is `/` or the home directory.

## [0.4.0] - 2026-10-16

### Added

- DNS inside sandboxes, on by default (`dns = true`): a sandbox behind the
  proxy gets a `/etc/resolv.conf` naming `127.0.0.1`, where `box` serves
  DNS over UDP and TCP and relays every query through the proxy to the
  nameservers of the host's `/etc/resolv.conf`; a query the host cannot
  answer gets SERVFAIL rather than a timeout. Since nothing inside a
  `bwrap` sandbox may bind port 53, `box run` now creates the network
  namespace itself: it execs `unshare -U -n --keep-caps` (util-linux),
  then `box _netns`, which binds the listeners, drops every capability
  and execs `bwrap` with `--share-net`, so the PID, the proxy and the
  sandboxed program's lack of capabilities stay as they were. `box show`
  prints the whole chain, `box info` reports whether DNS is available,
  and where the namespaces cannot be set up `box` warns and runs without
  DNS. Answers are not filtered by `allow_hosts`; `dns = false` leaves a
  sandbox without a resolver.

- Resource limits, on by default: every sandbox runs in a transient
  systemd scope (a cgroup of its own, `box-<name>-<pid>.scope`) with
  `cpus = 1` and `memory = "2G"` (swap excluded, `MemorySwapMax=0`), and
  every tmpfs mount is capped at `tmpfs_size = "1G"` with `bwrap --size`.
  `box` asks the systemd user manager for the scope over D-Bus with its own
  PID, the way `systemd-run --user --scope` does, before replacing itself
  with `bwrap`, so the limits cover the sandboxed program, everything it
  starts and the proxy, and reads them back from cgroupfs to warn about a
  controller that is not delegated. Without a reachable user manager it
  warns and runs without limits. `box show` prints the equivalent
  `systemd-run` prefix, `box info` reports the limits mechanism, and
  `box config` the values. Set a limit to `0` or `""` to lift it.

- `podman = true` sets a sandbox up for rootless podman and buildah, so an
  agent can build and run containers from inside: user namespaces stay
  enabled, `/sys` is mounted read-only, a per-binary directory
  `~/.local/state/box/podman/<name>` with the image store is bound in, a
  private tmpfs serves as `XDG_RUNTIME_DIR`, the `box _forward` wrapper
  starts podman's pause process in a single-UID user namespace of its own
  (so podman joins it quietly instead of failing to map `/etc/subuid`
  ranges, which `bwrap` cannot provide, and logging an error about it),
  an empty file is bound over `/etc/subuid` and `/etc/subgid` so podman
  falls back to its single-UID mapping without that pause process,
  and a `containers.conf` and `storage.conf` written into that directory
  make containers share the sandbox's network namespace (and so its
  proxy), disable cgroups and ignore chown errors in image layers.
  The built-in `[binaries.podman]` section enables it for
  `box run podman ...`; the defaults allow the Docker Hub, quay.io and
  ghcr.io registries. `box df` lists the store and `box clean` removes it.

- `ssh = true` lets a sandbox open SSH connections to `allow_hosts`, which
  it could not before: the proxy now refuses `CONNECT` requests to port 22
  unless the sandbox has it on, and with it `box` binds a generated
  `ssh_config` into the sandbox (over `/etc/ssh/ssh_config` with the host's
  content appended, `Include` lines commented out since ssh refuses files
  that appear owned by `nobody` inside; or at `~/.ssh/config` on a host
  without one) whose `ProxyCommand`, `box _connect %h %p`, tunnels the
  connection through the proxy's unix socket. `ssh_key` names a host
  private key to mount read-only at its own path and use as the identity,
  `ssh_user` the user name to log in as, and a copy of the host's
  `~/.ssh/known_hosts` is placed inside. Off by default; ignored with a
  warning on macOS and in a sandbox without network.

- `box run` refuses to start a command whose entry in a persistent overlay
  layer is a symlink to a path the host lacks, instead of failing inside the
  sandbox with `exec: "claude": executable file not found in $PATH`. That
  is what remains after claude updates itself inside: the new version
  lands in the tmpfs `$HOME` and vanishes, the repointed `~/.local/bin/claude`
  symlink persists in the layer. The error names the version the symlink
  points at and the one on the host and suggests `claude update` on the
  host (or `box clean <name>` to drop the layer). An entry removed inside
  is reported the same way. When a command still fails to exec inside, the
  forwarder's error now names the dangling symlink and its target.

[Full changelog]: https://github.com/deric/box/compare/v0.3.0...v0.4.0

## [0.3.0] - 2026-10-15

### Added

- The built-in `claude` profile mounts the directories Claude Code loads
  skills from: `~/.claude/skills` (personal and claude.ai-synced skills),
  `~/.claude/plugins` (installed plugins and marketplaces) and the
  cross-agent `~/.agents/skills` are persistent overlays on Linux, so the
  host's skills are visible inside and skills or plugins installed in the
  sandbox survive across runs in claude's layer under `~/.local/state/box`
  without touching the host directories. On macOS `~/.agents/skills` is
  readable; `~/.claude` was already read-write there.

- `box top` is split in two: below the sandboxes it lists the connections
  the proxy denied them, grouped by target host with the number of
  attempts, the time of the last one and the sandboxes that made them, most
  denied first. The summary line carries the total. The proxy logs are read
  from the `log_dir` of the configuration, so `box top` takes `-c` like the
  other commands.

[Full changelog]: https://github.com/deric/box/compare/v0.2.0...v0.3.0

## [0.2.0] - 2026-10-07

### Added

- `box top` watches the running sandboxes in a full-screen terminal view
  that refreshes every second (`-i` sets the interval): one row per sandbox
  with its process count, CPU usage rate, resident memory (also as a share
  of physical memory), uptime, directory and command. Keys sort by CPU,
  memory, processes, uptime or name, reverse the order, change the interval
  and quit.

- `box df` prints the disk usage of each binary's sandboxes on the host:
  persistent overlay layers, private `/tmp` directories and proxy logs, with
  a total; a name argument limits it to one binary and `-a` lists every
  layer, directory and log separately.

- `box info` prints the version, platform, isolation mechanism (`bwrap` or
  `sandbox-exec`, with path and version), config and state locations and the
  number of running sandboxes per binary.

- `log_dir` sets the directory the proxy writes its denied-request logs to
  (default `/tmp/box`); `box info` shows it and `box clean` removes stale logs
  from it.

- `~/.local/bin` is mounted as a persistent overlay by default on Linux (and
  read-only on macOS): tools installed inside the sandbox, e.g. with
  `go install` or `pip install --user`, land in the per-binary upper layer
  and survive across runs without touching the host directory.

### Changed

- The minimum Go version to build is 1.24 (was 1.27); `golang.org/x/sys` is
  pinned to v0.41.0, the newest release that still supports Go 1.24.

- The proxy log is named `/tmp/box/<name>-<pid>-<dir>.log`, with `<dir>` the
  working directory minus its leading slash and the other slashes replaced by
  dashes, so logs of one binary started from different directories can be
  told apart.

### Fixed

- `go install github.com/deric/box/cmd/box@latest` failed because `go.mod`
  declared the module path as `box`; it is now `github.com/deric/box`.

[Full changes]: https://github.com/deric/box/compare/v0.1.1...v0.2.0
[0.2.0]: https://github.com/deric/box/releases/tag/v0.2.0

## [0.1.1] - 2026-10-06

### Fixed

- Release workflow failed to resolve `dagger/dagger-for-github@v8` because no
  floating `v8` tag exists; the action is now pinned to `v8.4.1`.

### Added

- `box run` runs a program inside a [bubblewrap](https://github.com/containers/bubblewrap)
  sandbox on Linux or a `sandbox-exec` (Seatbelt) sandbox on macOS.
- `box init`, `box show`, `box config`, `box clean` and `box ps` subcommands.
- Per-binary profiles in `~/.config/box/box.toml`, with a built-in profile for `claude`
  that exposes its config, `projects/` and credentials.
- Network isolation: the sandbox gets an empty network namespace and reaches the
  outside only through an allowlisting HTTP(S) proxy on the host (`allow_hosts`).
- Private `/tmp` per sandbox and overlay layers for writable host directories.
- Cleared environment with an explicit `pass_env` list and `BOX_SANDBOX=1` marker.
- User namespace creation is disabled inside the sandbox.
- Packaging with nfpm and a Dagger-based release pipeline.

[0.1.1]: https://github.com/deric/box/releases/tag/v0.1.1
[Unreleased]: https://github.com/deric/box/compare/v0.4.0...HEAD
