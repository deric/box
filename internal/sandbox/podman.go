package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Rootless podman inside a bwrap sandbox.
//
// Podman re-executes itself inside a user namespace of its own, so a
// profile with podman = true keeps user namespaces enabled (config.Resolve
// clears disable_userns). bwrap's own user namespace maps a single UID, so
// the setuid newuidmap/newgidmap helpers cannot map the user's subordinate
// ID ranges in there; an empty file is bound over /etc/subuid and
// /etc/subgid so that podman falls back to its single-mapping mode instead
// of failing on them, and the image store is told to ignore chown errors,
// which leaves every file in an image owned by root. Everything podman
// keeps lives in a per-binary directory <state>/podman/<name>, bound into
// the sandbox at the same path: the image store, the generated
// containers.conf and storage.conf (written before each run, pointed at by
// CONTAINERS_CONF_OVERRIDE and CONTAINERS_STORAGE_CONF) and the empty subid
// file. The host's own image store is not shared, as it was created with a
// different ID mapping. Podman's runtime state (pause process, locks) goes
// to a fresh tmpfs at /run/user/<uid>, which XDG_RUNTIME_DIR points at. The
// generated containers.conf makes containers and build steps share the
// sandbox's network namespace, where box's proxy is reachable and the proxy
// variables are inherited, and disables cgroups, which are not delegated to
// the sandbox. /sys is mounted read-only because the OCI runtime bind-mounts
// it into containers (a user namespace cannot mount sysfs of its own).

// PodmanRoot returns the host directory under which binary-specific podman
// state is kept: <stateDir>/podman.
func PodmanRoot(stateDir string) string { return filepath.Join(stateDir, "podman") }

// PodmanDir returns the host directory holding binary name's podman state,
// bound read-write into the sandbox at the same path: the image store in
// its storage subdirectory next to the generated configuration files.
func PodmanDir(stateDir, name string) string { return filepath.Join(PodmanRoot(stateDir), name) }

// Files box writes into PodmanDir before each run.
const (
	podmanContainersConfFile = "containers.conf"
	podmanStorageConfFile    = "storage.conf"
	podmanSubidFile          = "subid" // empty; bound over /etc/subuid and /etc/subgid
)

// RuntimeDir returns the XDG_RUNTIME_DIR used inside a podman sandbox, a
// tmpfs private to it.
func RuntimeDir(uid int) string { return "/run/user/" + strconv.Itoa(uid) }

// podmanEnv returns the variables that point podman at its sandbox
// configuration in dir (see PodmanDir), as key/value pairs in a fixed order.
func podmanEnv(uid int, dir string) [][2]string {
	return [][2]string{
		{"XDG_RUNTIME_DIR", RuntimeDir(uid)},
		{"CONTAINERS_CONF_OVERRIDE", filepath.Join(dir, podmanContainersConfFile)},
		{"CONTAINERS_STORAGE_CONF", filepath.Join(dir, podmanStorageConfFile)},
	}
}

// podmanContainersConf is loaded after every other containers.conf.
const podmanContainersConf = `# Written by box for rootless podman inside the sandbox.

[containers]
# The sandbox has no network beyond loopback and box's proxy; containers and
# build steps share it and inherit the proxy variables (http_proxy = true).
netns = "host"
# No cgroup is delegated to the sandbox.
cgroups = "disabled"
pids_limit = 0
# journald is not reachable from inside.
log_driver = "k8s-file"

[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
`

// podmanStorageConf returns the storage.conf for an image store at graphroot.
func podmanStorageConf(graphroot string) string {
	return `# Written by box for rootless podman inside the sandbox.

[storage]
driver = "overlay"
graphroot = ` + strconv.Quote(graphroot) + `

[storage.options]
# The sandbox's user namespace maps a single UID, so files owned by other
# users cannot be created; they end up owned by root instead of failing.
ignore_chown_errors = "true"
`
}

// podmanMounts returns the mounts a podman-enabled sandbox needs beyond the
// profile's own and records the state directory to create and the
// configuration files to write into it.
func podmanMounts(opts *Options, plan *Plan) ([]*mount, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("podman requires a state directory")
	}
	dir := PodmanDir(opts.StateDir, plan.Name)
	plan.Dirs = append(plan.Dirs, dir)
	if plan.Writes == nil {
		plan.Writes = map[string]string{}
	}
	plan.Writes[filepath.Join(dir, podmanContainersConfFile)] = podmanContainersConf
	plan.Writes[filepath.Join(dir, podmanStorageConfFile)] = podmanStorageConf(filepath.Join(dir, "storage"))
	plan.Writes[filepath.Join(dir, podmanSubidFile)] = ""
	mounts := []*mount{
		{kind: kindRW, src: dir, dest: dir, visible: true},
		{kind: kindRO, src: "/sys", dest: "/sys", visible: true},
		{kind: kindTmpfs, dest: RuntimeDir(os.Getuid()), mode: 0o700},
	}
	// Hide the subordinate ID ranges, which cannot be used inside. Only an
	// existing file can be bound over; a host without them has none to hide.
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			mounts = append(mounts, &mount{kind: kindRO, src: filepath.Join(dir, podmanSubidFile), dest: f, visible: true})
		}
	}
	return mounts, nil
}
