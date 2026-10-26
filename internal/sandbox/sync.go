package sandbox

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SyncDir is where the host originals of sync_files are bound inside the
// sandbox on Linux, one file per entry named by its index.
const SyncDir = "/run/box/sync"

// SaveDir is where a host directory (Plan.SyncSave) is bound inside the
// sandbox for the forwarder to save sandbox copies that conflict with a
// host original changed meanwhile, so they outlive the sandbox for a merge
// by hand.
const SaveDir = "/run/box/saved"

// SyncFile pairs a sandbox copy of a host file (Path) with the host
// original (Mount). The copy is written through to the original when the
// sandboxed command exits (see WriteBack).
//
// On Linux the copy is at the same path as on the host, placed there by
// bwrap, and the original is bound read-write at Mount. On macOS, where
// nothing can be put at another path, Path is a copy under Plan.SyncDir
// that the program is pointed at, Mount is the original's real path and
// Base is a second, untouched copy taken at the same time: what the
// sandbox changed is what differs between Path and Base, what the host
// changed meanwhile is what differs between Mount and Base. An entry with
// Base may be a directory, written back file by file.
type SyncFile struct {
	Path  string
	Mount string
	Base  string
}

// Name is the path the user knows the file by: the host original's. On
// Linux that is Path, the same path as on the host; on macOS, where Path
// is the copy's own location, it is Mount.
func (s SyncFile) Name() string {
	if s.Base != "" {
		return s.Mount
	}
	return s.Path
}

// Arg renders the entry for the forwarder's -w flag.
func (s SyncFile) Arg() string {
	if s.Base != "" {
		return s.Path + "=" + s.Mount + "=" + s.Base
	}
	return s.Path + "=" + s.Mount
}

// ParseSyncFile parses the -w flag format, path=mount or path=mount=base.
func ParseSyncFile(arg string) (SyncFile, error) {
	p, rest, ok := strings.Cut(arg, "=")
	m, b, _ := strings.Cut(rest, "=")
	if !ok || p == "" || m == "" {
		return SyncFile{}, fmt.Errorf("sync file %q: want path=mount", arg)
	}
	return SyncFile{Path: p, Mount: m, Base: b}, nil
}

// Syncer remembers what the host originals looked like when the sandbox
// started so that WriteBack can tell whether they changed underneath it.
type Syncer struct {
	files []SyncFile
	start map[string][32]byte // digest of each Mount at Start (entries without Base)
	// skip lists, per entry, the paths of the other entries that lie under
	// it (and of their base copies), which are synced on their own.
	skip map[string]map[string]bool
	// trees records, per directory entry, how many files WriteBack changed
	// on the host.
	trees map[string]int
}

// NewSyncer records the current content of each file's host original.
func NewSyncer(files []SyncFile) *Syncer {
	s := &Syncer{files: files, start: map[string][32]byte{}, skip: map[string]map[string]bool{}, trees: map[string]int{}}
	for _, f := range files {
		if f.Base == "" {
			if data, err := os.ReadFile(f.Mount); err == nil {
				s.start[f.Mount] = sha256.Sum256(data)
			}
		}
		s.skip[f.Path] = map[string]bool{}
		for _, o := range files {
			if o.Path == f.Path {
				continue
			}
			if under(o.Path, f.Path) {
				s.skip[f.Path][o.Path] = true
			}
			if o.Base != "" && f.Base != "" && under(o.Base, f.Base) {
				s.skip[f.Path][o.Base] = true
			}
		}
	}
	return s
}

// WriteBack copies each sandbox copy over its host original in place, so the
// original keeps its inode, mode and owner. Copies whose content equals the
// original are left alone. An original that was modified on the host while
// the sandbox ran is not touched either: it is returned as a Conflict for
// the caller to resolve, by overwriting it after all or by saving the copy
// aside for a merge by hand. A directory entry is written back file by
// file, each one a conflict of its own.
func (s *Syncer) WriteBack() (conflicts []Conflict, err error) {
	for _, f := range s.files {
		if fi, lerr := os.Lstat(f.Path); lerr == nil && fi.IsDir() {
			c, n, terr := s.writeBackTree(f)
			conflicts = append(conflicts, c...)
			s.trees[f.Mount] = n
			err = joinErr(err, terr)
			continue
		}
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
		if s.hostChanged(f, host) {
			conflicts = append(conflicts, Conflict{File: f, Host: host, Copy: data})
			continue
		}
		if werr := writeInPlace(f.Mount, data); werr != nil {
			err = joinErr(err, fmt.Errorf("writing back %s: %w", f.Path, werr))
		}
	}
	return conflicts, err
}

// hostChanged reports whether the host original of f, now holding host,
// differs from what it held when the sandbox started.
func (s *Syncer) hostChanged(f SyncFile, host []byte) bool {
	if f.Base != "" {
		base, err := os.ReadFile(f.Base)
		return err != nil || !bytes.Equal(base, host)
	}
	start, ok := s.start[f.Mount]
	return ok && sha256.Sum256(host) != start
}

// Summary describes, one line per directory entry that was written back,
// how many files changed on the host, for the user to be told.
func (s *Syncer) Summary() []string {
	var out []string
	for _, f := range s.files {
		if n := s.trees[f.Mount]; n > 0 {
			out = append(out, fmt.Sprintf("%s: %d files written back from the sandbox copy %s", f.Mount, n, f.Path))
		}
	}
	return out
}

// treeEntry is what matters about a path for telling two trees apart.
type treeEntry struct {
	mode  fs.FileMode
	size  int64
	mtime time.Time
	link  string // symlinks: the target
}

// walkTree lists root recursively by path relative to it, without
// following symlinks, leaving out the paths in skip and what is below them.
func walkTree(root string, skip map[string]bool) (map[string]treeEntry, error) {
	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if skip[p] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := treeEntry{mode: info.Mode(), size: info.Size(), mtime: info.ModTime()}
		if e.mode&fs.ModeSymlink != 0 {
			if e.link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = e
		return nil
	})
	return out, err
}

// content reads what a tree entry holds for comparison: a symlink's
// target, a regular file's data. Directories and special files have none.
func content(path string, e treeEntry) ([]byte, error) {
	switch {
	case e.mode&fs.ModeSymlink != 0:
		return []byte(e.link), nil
	case e.mode.IsRegular():
		return os.ReadFile(path)
	}
	return nil, nil
}

// writeBackTree writes the files the sandbox changed under the copy of a
// directory entry (f.Path) over the host directory (f.Mount): files added
// or modified are written, files removed are removed, each unless the host
// changed the same file meanwhile, which is a Conflict. Files the sandbox
// left alone are not looked at, whatever happened to them on the host, so
// a session of the same program on the host loses nothing. Whether the
// sandbox changed a file is decided against f.Base, the untouched copy:
// by size and modification time first, by content when those differ. n
// counts the host paths written or removed.
func (s *Syncer) writeBackTree(f SyncFile) (conflicts []Conflict, n int, err error) {
	copyT, err := walkTree(f.Path, s.skip[f.Path])
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", f.Path, err)
	}
	baseT, err := walkTree(f.Base, s.skip[f.Path])
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", f.Base, err)
	}
	rels := make([]string, 0, len(copyT))
	for rel := range copyT {
		rels = append(rels, rel)
	}
	sort.Strings(rels) // parents before children
	for _, rel := range rels {
		c := copyT[rel]
		b, inBase := baseT[rel]
		host := filepath.Join(f.Mount, rel)
		if c.mode.IsDir() {
			if !inBase || !b.mode.IsDir() {
				if merr := os.MkdirAll(host, c.mode.Perm()|0o700); merr != nil {
					err = joinErr(err, merr)
				}
			}
			continue
		}
		if !c.mode.IsRegular() && c.mode&fs.ModeSymlink == 0 {
			continue // sockets, pipes: nothing to write back
		}
		changed := !inBase || b.mode.Type() != c.mode.Type() || b.link != c.link
		if !changed && c.mode.IsRegular() && (b.size != c.size || !b.mtime.Equal(c.mtime)) {
			changed = true
		}
		if !changed {
			continue
		}
		data, rerr := content(filepath.Join(f.Path, rel), c)
		if rerr != nil {
			err = joinErr(err, rerr)
			continue
		}
		var base []byte
		if inBase {
			if base, rerr = content(filepath.Join(f.Base, rel), b); rerr != nil {
				err = joinErr(err, rerr)
				continue
			}
			if b.mode.Type() == c.mode.Type() && bytes.Equal(base, data) {
				continue // rewritten with the same content
			}
		}
		hostData, hostMode, hostErr := readHost(host)
		if hostErr == nil && hostMode.Type() == c.mode.Type() && bytes.Equal(hostData, data) {
			continue // the host already has it
		}
		hostChanged := inBase && (hostErr != nil || hostMode.Type() != b.mode.Type() || !bytes.Equal(hostData, base)) ||
			!inBase && hostErr == nil
		if hostChanged {
			conflicts = append(conflicts, Conflict{
				File: SyncFile{Path: filepath.Join(f.Path, rel), Mount: host, Base: filepath.Join(f.Base, rel)},
				Host: hostData, Copy: data, Link: c.mode&fs.ModeSymlink != 0, Gone: hostErr != nil,
			})
			continue
		}
		if werr := writeEntry(host, data, c, hostErr == nil); werr != nil {
			err = joinErr(err, fmt.Errorf("writing back %s: %w", host, werr))
			continue
		}
		n++
	}
	// Removed inside: children before parents, so emptied directories go.
	rels = rels[:0]
	for rel := range baseT {
		if _, ok := copyT[rel]; !ok {
			rels = append(rels, rel)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rels)))
	for _, rel := range rels {
		b := baseT[rel]
		host := filepath.Join(f.Mount, rel)
		if b.mode.IsDir() {
			_ = os.Remove(host) // only when empty
			continue
		}
		hostData, hostMode, hostErr := readHost(host)
		if hostErr != nil {
			continue // gone on the host as well
		}
		base, rerr := content(filepath.Join(f.Base, rel), b)
		if rerr != nil {
			err = joinErr(err, rerr)
			continue
		}
		if hostMode.Type() != b.mode.Type() || !bytes.Equal(hostData, base) {
			conflicts = append(conflicts, Conflict{
				File: SyncFile{Path: filepath.Join(f.Path, rel), Mount: host, Base: filepath.Join(f.Base, rel)},
				Host: hostData, Link: hostMode&fs.ModeSymlink != 0, Removed: true,
			})
			continue
		}
		if werr := os.Remove(host); werr != nil {
			err = joinErr(err, werr)
			continue
		}
		n++
	}
	return conflicts, n, err
}

// readHost reads a host path the way content does, with its mode; a
// missing path is an error.
func readHost(path string) ([]byte, fs.FileMode, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	e := treeEntry{mode: fi.Mode()}
	if e.mode&fs.ModeSymlink != 0 {
		if e.link, err = os.Readlink(path); err != nil {
			return nil, 0, err
		}
	}
	data, err := content(path, e)
	return data, fi.Mode(), err
}

// writeEntry puts data at host as the entry c describes: a symlink, or a
// regular file written in place when the host has one (exists) and created
// with the copy's permissions otherwise.
func writeEntry(host string, data []byte, c treeEntry, exists bool) error {
	if err := os.MkdirAll(filepath.Dir(host), 0o700); err != nil {
		return err
	}
	if c.mode&fs.ModeSymlink != 0 {
		if exists {
			if err := os.Remove(host); err != nil {
				return err
			}
		}
		return os.Symlink(string(data), host)
	}
	if exists {
		if fi, err := os.Lstat(host); err == nil && fi.Mode().IsRegular() {
			return writeInPlace(host, data)
		}
		if err := os.Remove(host); err != nil {
			return err
		}
	}
	return os.WriteFile(host, data, c.mode.Perm())
}

// Conflict is a sync file whose host original changed while the sandbox
// ran and whose sandbox copy differs from it: neither version can be kept
// without losing the other.
type Conflict struct {
	File    SyncFile
	Host    []byte // the original as it is on the host now (a symlink: its target)
	Copy    []byte // the sandbox's version
	Link    bool   // the sandbox's version is a symlink to Copy
	Removed bool   // the sandbox removed its copy; Copy is empty
	Gone    bool   // the host removed the original; Host is empty
}

// Overwrite writes the sandbox copy over the host original in place, or
// removes the original when the sandbox removed its copy.
func (c Conflict) Overwrite() error {
	switch {
	case c.Removed:
		if err := os.Remove(c.File.Mount); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", c.File.Mount, err)
		}
	case c.Link || c.Gone:
		e := treeEntry{mode: 0o600}
		if c.Link {
			e.mode = fs.ModeSymlink
		}
		if err := writeEntry(c.File.Mount, c.Copy, e, !c.Gone); err != nil {
			return fmt.Errorf("writing back %s: %w", c.File.Path, err)
		}
	default:
		if err := writeInPlace(c.File.Mount, c.Copy); err != nil {
			return fmt.Errorf("writing back %s: %w", c.File.Path, err)
		}
	}
	return nil
}

// Save writes the sandbox copy into dir under SavedName and returns the
// name, leaving the host original as it is. A removed copy has nothing to
// save; the name is then empty.
func (c Conflict) Save(dir string, now time.Time) (string, error) {
	if c.Removed {
		return "", nil
	}
	name := SavedName(c.File.Name(), now)
	if err := os.WriteFile(filepath.Join(dir, name), c.Copy, 0o600); err != nil {
		return "", fmt.Errorf("saving the sandbox copy of %s: %w", c.File.Name(), err)
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

// copyTree copies src, a file, symlink or directory tree, to dst, keeping
// permissions and modification times (which tell the copies apart later).
func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case fi.IsDir():
		if err := os.Mkdir(dst, fi.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	case fi.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
	return nil // sockets, pipes: nothing to copy
}

func joinErr(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return fmt.Errorf("%w; %w", a, b)
}
