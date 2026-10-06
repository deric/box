# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `box info` prints the version, platform, isolation mechanism (`bwrap` or
  `sandbox-exec`, with path and version), config and state locations and the
  number of running sandboxes per binary.

### Changed

- The proxy log is named `/tmp/box/<name>-<pid>-<dir>.log`, with `<dir>` the
  working directory and slashes replaced by dashes, so logs of one binary
  started from different directories can be told apart.

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

[Unreleased]: https://github.com/deric/box/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/deric/box/releases/tag/v0.1.1
