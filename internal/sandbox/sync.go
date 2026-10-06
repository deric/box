package sandbox

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

// SyncDir is where the host originals of sync_files are bound inside the
// sandbox, one file per entry named by its index.
const SyncDir = "/run/box/sync"

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
// original are left alone. A warning is returned for each original that was
// modified on the host while the sandbox ran: it is overwritten regardless,
// as the sandbox's view is the only one that can be saved at this point.
func (s *Syncer) WriteBack() (warnings []string, err error) {
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
			warnings = append(warnings, fmt.Sprintf("%s changed on the host while the sandbox ran; overwriting", f.Path))
		}
		if werr := writeInPlace(f.Mount, data); werr != nil {
			err = joinErr(err, fmt.Errorf("writing back %s: %w", f.Path, werr))
		}
	}
	return warnings, err
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
