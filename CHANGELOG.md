# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/deric/box/compare/v0.2.0...HEAD

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
