package sandbox

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncDir is where the host originals of sync_files are bound inside the
// sandbox, one file per entry named by its index.
const SyncDir = "/run/box/sync"

// SaveDir is where a host directory (Plan.SyncSave) is bound inside the
// sandbox for the forwarder to save sandbox copies that conflict with a
// host original changed meanwhile, so they outlive the sandbox for a merge
// by hand.
const SaveDir = "/run/box/saved"

// SyncFile pairs a sandbox copy of a host file (Path, the same path as on the
// host) with the host original bound read-write at Mount. The copy is written
// through to the original when the sandboxed command exits (see WriteBack).
type SyncFile struct {
	Path  string
	Mount string
}

// Arg renders the pair for the forwarder's -w flag.
func (s SyncFile) Arg() string { return s.Path + "=" + s.Mount }

// ParseSyncFile parses the -w flag format, path=mount.
func ParseSyncFile(arg string) (SyncFile, error) {
	p, m, ok := strings.Cut(arg, "=")
	if !ok || p == "" || m == "" {
		return SyncFile{}, fmt.Errorf("sync file %q: want path=mount", arg)
	}
	return SyncFile{Path: p, Mount: m}, nil
}

// Syncer remembers what the host originals looked like when the sandbox
// started so that WriteBack can tell whether they changed underneath it.
type Syncer struct {
	files []SyncFile
	start map[string][32]byte // digest of each Mount at Start
}

// NewSyncer records the current content of each file's host original.
func NewSyncer(files []SyncFile) *Syncer {
	s := &Syncer{files: files, start: map[string][32]byte{}}
	for _, f := range files {
		if data, err := os.ReadFile(f.Mount); err == nil {
			s.start[f.Mount] = sha256.Sum256(data)
		}
	}
	return s
}

// WriteBack copies each sandbox copy over its host original in place, so the
// original keeps its inode, mode and owner. Copies whose content equals the
// original are left alone. An original that was modified on the host while
// the sandbox ran is not touched either: it is returned as a Conflict for
// the caller to resolve, by overwriting it after all or by saving the copy
// aside for a merge by hand.
func (s *Syncer) WriteBack() (conflicts []Conflict, err error) {
	for _, f := range s.files {
		data, rerr := os.ReadFile(f.Path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // removed inside; keep the host original
			}
			err = joinErr(err, fmt.Errorf("%s: %w", f.Path, rerr))
			continue
		}
		host, rerr := os.ReadFile(f.Mount)
		if rerr != nil {
			err = joinErr(err, fmt.Errorf("%s: %w", f.Mount, rerr))
			continue
		}
		if bytes.Equal(data, host) {
			continue
		}
		if start, ok := s.start[f.Mount]; ok && sha256.Sum256(host) != start {
			conflicts = append(conflicts, Conflict{File: f, Host: host, Copy: data})
			continue
		}
		if werr := writeInPlace(f.Mount, data); werr != nil {
			err = joinErr(err, fmt.Errorf("writing back %s: %w", f.Path, werr))
		}
	}
	return conflicts, err
}

// Conflict is a sync file whose host original changed while the sandbox
// ran and whose sandbox copy differs from it: neither version can be kept
// without losing the other.
type Conflict struct {
	File SyncFile
	Host []byte // the original as it is on the host now
	Copy []byte // the sandbox's version
}

// Overwrite writes the sandbox copy over the host original in place.
func (c Conflict) Overwrite() error {
	if err := writeInPlace(c.File.Mount, c.Copy); err != nil {
		return fmt.Errorf("writing back %s: %w", c.File.Path, err)
	}
	return nil
}

// Save writes the sandbox copy into dir under SavedName and returns the
// name, leaving the host original as it is.
func (c Conflict) Save(dir string, now time.Time) (string, error) {
	name := SavedName(c.File.Path, now)
	if err := os.WriteFile(filepath.Join(dir, name), c.Copy, 0o600); err != nil {
		return "", fmt.Errorf("saving the sandbox copy of %s: %w", c.File.Path, err)
	}
	return name, nil
}

// SavedName is the file name a saved copy of path gets: its base name with
// the time appended, so copies of the same file from different runs stay
// apart (.claude.json.20261019-153000).
func SavedName(path string, now time.Time) string {
	return filepath.Base(path) + "." + now.Format("20060102-150405")
}

// writeInPlace truncates and rewrites path without replacing it: the file is
// a bind mount, so it cannot be renamed over.
func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func joinErr(a, b error) error {
	if a == nil {
		return b
	}
	return fmt.Errorf("%w; %w", a, b)
}
