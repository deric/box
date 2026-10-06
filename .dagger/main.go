// Build and package the box binary.
//
// Cross-compiles box and wraps it into tar.gz archives (Linux and macOS) and
// deb/rpm packages (Linux, built with nfpm from nfpm.yaml). The usual entry
// point is `dagger call package --version=v1.2.3 export --path=dist`; `check`
// installs the deb and rpm into fresh containers and runs `box version`.
package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dagger/box/internal/dagger"
)

const (
	goImage   = "golang:1.27-alpine"
	nfpmImage = "goreleaser/nfpm:v2.47.0"
	baseImage = "alpine:3.22"
)

var (
	architectures = []string{"amd64", "arm64"}
	semverCore    = regexp.MustCompile(`^(\d+\.\d+\.\d+)(.*)$`)
	nonAlnum      = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

type Box struct {
	// +private
	Source *dagger.Directory
}

func New(
	// Project source directory.
	// +defaultPath="/"
	// +ignore=[".git", ".dagger", ".github", ".task", "box", "dist"]
	source *dagger.Directory,
) *Box {
	return &Box{Source: source}
}

// Build compiles a static box binary for the given OS and architecture.
func (m *Box) Build(
	// Version string baked into `box version` (e.g. the git tag).
	// +default="dev"
	version string,
	// Target operating system: linux or darwin.
	// +default="linux"
	os string,
	// Target architecture: amd64 or arm64.
	// +default="amd64"
	arch string,
) *dagger.File {
	return dag.Container().
		From(goImage).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("box-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("box-go-build")).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithEnvVariable("GOCACHE", "/root/.cache/go-build").
		WithEnvVariable("CGO_ENABLED", "0").
		WithEnvVariable("GOOS", os).
		WithEnvVariable("GOARCH", arch).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithExec([]string{"go", "mod", "download"}).
		WithExec([]string{
			"go", "build", "-trimpath",
			"-ldflags", "-s -w -X main.version=" + version,
			"-o", "/out/box", "./cmd/box",
		}).
		File("/out/box")
}

// Tarball builds box and archives it with README.md and LICENSE as
// box_<version>_<os>_<arch>.tar.gz.
func (m *Box) Tarball(
	// Version string baked into the binary and used in the file name.
	// +default="dev"
	version string,
	// Target operating system: linux or darwin.
	// +default="linux"
	os string,
	// Target architecture: amd64 or arm64.
	// +default="amd64"
	arch string,
) *dagger.File {
	name := artifactName(version, os, arch) + ".tar.gz"
	return dag.Container().
		From(baseImage).
		WithMountedDirectory("/src", m.stage(version, os, arch)).
		WithWorkdir("/src").
		WithDirectory("/out", dag.Directory()).
		WithExec([]string{"tar", "-czf", "/out/" + name, "box", "README.md", "LICENSE"}).
		File("/out/" + name)
}

// Deb builds a Debian package (depends on bubblewrap) for the given architecture.
func (m *Box) Deb(
	// Version string baked into the binary and the package metadata.
	// +default="dev"
	version string,
	// Target architecture: amd64 or arm64.
	// +default="amd64"
	arch string,
) *dagger.File {
	return m.nfpm(version, arch, "deb")
}

// Rpm builds an RPM package (depends on bubblewrap) for the given architecture.
func (m *Box) Rpm(
	// Version string baked into the binary and the package metadata.
	// +default="dev"
	version string,
	// Target architecture: amd64 or arm64.
	// +default="amd64"
	arch string,
) *dagger.File {
	return m.nfpm(version, arch, "rpm")
}

// Package builds every release artifact into one directory: tar.gz archives
// for linux and darwin, deb and rpm packages for linux (amd64 and arm64 each)
// and a SHA256SUMS file covering them all.
func (m *Box) Package(
	// Version string baked into the binaries, packages and file names.
	// +default="dev"
	version string,
) *dagger.Directory {
	var files []*dagger.File
	for _, arch := range architectures {
		files = append(files,
			m.Tarball(version, "linux", arch),
			m.Tarball(version, "darwin", arch),
			m.Deb(version, arch),
			m.Rpm(version, arch),
		)
	}
	dist := dag.Directory().WithFiles("/", files)

	sums := dag.Container().
		From(baseImage).
		WithMountedDirectory("/dist", dist).
		WithWorkdir("/dist").
		WithDirectory("/out", dag.Directory()).
		WithExec([]string{"sh", "-c", "sha256sum * > /out/SHA256SUMS"}).
		File("/out/SHA256SUMS")

	return dist.WithFile("SHA256SUMS", sums)
}

// Check installs the deb into Debian and the rpm into Fedora, and runs
// `box version` in each; returns the combined output.
func (m *Box) Check(
	ctx context.Context,
	// Version string baked into the binary and the package metadata.
	// +default="dev"
	version string,
) (string, error) {
	deb, err := dag.Container().
		From("debian:bookworm-slim").
		WithFile("/tmp/box.deb", m.Deb(version, "amd64")).
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends", "/tmp/box.deb"}).
		WithExec([]string{"box", "version"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("deb: %w", err)
	}

	rpm, err := dag.Container().
		From("fedora:42").
		WithFile("/tmp/box.rpm", m.Rpm(version, "amd64")).
		WithExec([]string{"dnf", "install", "-y", "/tmp/box.rpm"}).
		WithExec([]string{"box", "version"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("rpm: %w", err)
	}

	return "deb: " + strings.TrimSpace(deb) + "\nrpm: " + strings.TrimSpace(rpm) + "\n", nil
}

// nfpm runs nfpm with the repository's nfpm.yaml against a staged build.
func (m *Box) nfpm(version, arch, packager string) *dagger.File {
	name := artifactName(version, "linux", arch) + "." + packager
	return dag.Container().
		From(nfpmImage).
		WithMountedDirectory("/src", m.stage(version, "linux", arch).
			WithFile("nfpm.yaml", m.Source.File("nfpm.yaml"))).
		WithWorkdir("/src").
		WithDirectory("/out", dag.Directory()).
		WithEnvVariable("VERSION", packageVersion(version)).
		WithEnvVariable("ARCH", arch).
		WithExec([]string{
			"/usr/bin/nfpm", "package",
			"--config", "nfpm.yaml",
			"--packager", packager,
			"--target", "/out/" + name,
		}).
		File("/out/" + name)
}

// stage is the set of files that goes into every artifact.
func (m *Box) stage(version, os, arch string) *dagger.Directory {
	return dag.Directory().
		WithFile("box", m.Build(version, os, arch)).
		WithFile("README.md", m.Source.File("README.md")).
		WithFile("LICENSE", m.Source.File("LICENSE"))
}

// artifactName is the file name stem shared by all artifacts of one build.
func artifactName(version, os, arch string) string {
	return fmt.Sprintf("box_%s_%s_%s", strings.TrimPrefix(version, "v"), os, arch)
}

// packageVersion turns a version as produced by `git describe` into one deb
// and rpm accept: a semver core, optionally followed by a prerelease made of
// dot-separated alphanumeric identifiers (nfpm renders it as 1.2.3~pre).
// Versions without a semver core, such as a bare commit hash or "dev", become
// a prerelease of 0.0.0 so they sort before any tagged release.
//
//	v1.2.3            -> 1.2.3
//	v1.2.3-rc1        -> 1.2.3-rc1
//	v1.2.3-4-gabc-dirty -> 1.2.3-4.gabc.dirty
//	c585844           -> 0.0.0-c585844
func packageVersion(version string) string {
	v := strings.TrimPrefix(version, "v")
	core, rest := "0.0.0", v
	if m := semverCore.FindStringSubmatch(v); m != nil {
		core, rest = m[1], m[2]
	}
	rest = strings.Trim(nonAlnum.ReplaceAllString(rest, "."), ".")
	if rest == "" {
		return core
	}
	return core + "-" + rest
}
