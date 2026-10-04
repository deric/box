# box - A sandbox for running agents

`box` runs a program inside a [bubblewrap](https://github.com/containers/bubblewrap)
(`bwrap`) sandbox on Linux, or a `sandbox-exec` (Seatbelt) sandbox on macOS,
with sensible defaults, configured per binary from `~/.config/box/box.toml`.
See [macOS](#macos) for how the two differ.

## Getting started

```
box init # writes config into ~/.config/box/box.toml
box run claude
```

```sh
box run claude --some-flag      # run claude sandboxed
box show claude                 # print the sandbox command line without running it
box config claude               # print the effective, merged profile
box init                            # write the default ~/.config/box/box.toml
box clean claude                # drop claude's persistent overlay layers
box ps                          # list running sandboxes with process count, CPU, memory
```

## Defaults (Linux)

| Host path                        | Inside the sandbox                                   |
|----------------------------------|------------------------------------------------------|
| `/usr`, `/bin`, `/lib*`, `/etc`  | read-only (merged-/usr symlinks are recreated)       |
| current directory                | read-write                                           |
| `/tmp`                           | read-write                                           |
| `$HOME`                          | empty tmpfs; only listed paths underneath are visible |
| `~/.local/share/mise`            | overlay: host content visible, writes land in a per-binary upper layer under `~/.local/state/box/overlays/` |
| network                          | shared with the host                                 |
| `/proc`, `/dev`                  | fresh, minimal                                       |
| PID, IPC, UTS, cgroup namespaces | unshared; hostname is `box`                          |

Everything is driven by `--unshare-all`; the environment is inherited and
`BOX_SANDBOX=1` is set so programs can detect they are sandboxed.

## Configuration

`box init` writes a commented default file. The `[default]` section applies to
every binary; `[binaries.<name>]` (the binary's base name) overrides it. List
values are **appended** to the defaults, scalar values **replace** them, and
`drop_binds` removes inherited entries from any mount list.

```toml
[default]
ro_binds = ["/bin", "/sbin", "/lib", "?/lib32", "/lib64", "/usr", "/etc", "?/opt"]
rw_binds = ["$PWD", "/tmp"]
tmpfs    = ["$HOME", "/var/tmp"]
overlays = [{ path = "~/.local/share/mise", persist = true }]
network  = true
hostname = "box"
env      = { BOX_SANDBOX = "1" }

[binaries.claude]
rw_binds = ["?~/.claude", "?~/.claude.json"]

[binaries.untrusted-tool]
network    = false
drop_binds = ["/tmp"]
tmpfs      = ["/tmp"]
inherit    = true            # set false to ignore [default] entirely
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
| `drop_binds`      | list              | inherited entries to remove (binary sections only)             |
| `network`         | bool              | share the host network                                         |
| `hostname`        | string            | hostname inside the sandbox; empty keeps the host's            |
| `new_session`     | bool              | `--new-session` (blocks TIOCSTI, breaks shell job control)     |
| `die_with_parent` | bool              | kill the sandbox when `box` dies                               |
| `clear_env`       | bool              | start from an empty environment                                |
| `env`             | table             | variables to set                                               |
| `unset_env`       | list              | variables to unset                                             |
| `bind_binary`     | bool              | mount the resolved executable if no other mount exposes it     |
| `extra_args`      | list              | raw arguments appended to `bwrap` / `sandbox-exec`             |
| `seatbelt_rules`  | list              | raw SBPL rules appended to the macOS profile                   |
| `inherit`         | bool              | `false` ignores `[default]` for this binary                    |

Locations: config at `$BOX_CONFIG`, else `$XDG_CONFIG_HOME/box/box.toml`, else
`~/.config/box/box.toml`; overlay layers at `$XDG_STATE_HOME/box`, else
`~/.local/state/box`.

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
| `network = false`: own empty netns    | no IP traffic at all, loopback included                       |
| Unix sockets under mounted paths      | Unix sockets under exposed paths                              |
| PID / IPC namespaces                  | processes may only inspect and signal their own sandbox; Mach services are limited to a short list (name lookup, logging, and with network: DNS and TLS trust) |
| `hostname`, `new_session`             | not supported (warned and ignored)                            |
| `clear_env`, `env`, `unset_env`       | applied by `box` before starting `sandbox-exec`               |

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
- `bwrap` ≥ 0.10 for overlay mounts (uses `--overlay-src`, `--overlay`, `--tmp-overlay`).
  Overlays need a non-setuid `bwrap` and a kernel with unprivileged overlayfs (≥ 5.11).
- Go 1.26 to build: `go build -o box .`

### Debian / Ubuntu

```
sudo apt install bubblewrap
```

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
task clean          # remove build artifacts
```

## Notes

- `box` replaces itself with `bwrap` (or `sandbox-exec`) via `exec`, so
  signals and the terminal behave as if the program ran directly.
- `box` sets bwrap's `argv[0]` to `box:<name>`, which is how `box ps` finds
  its sandboxes; stats cover the whole process tree under that bwrap.
- Mounts are applied parents-first, so a tmpfs on `$HOME` never hides a bind
  or overlay placed underneath it.
- Sharing the host's `/tmp` exposes anything other processes put there (X11
  sockets, for example). For untrusted programs replace it with a tmpfs as in
  the example above.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
