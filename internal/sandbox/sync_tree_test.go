package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// treeFixture lays out a host directory with a file, a nested file and a
// symlink, and makes the sandbox copy and the base copy of it the way
// makeSyncCopies does.
func treeFixture(t *testing.T) (f SyncFile, dir string) {
	t.Helper()
	dir = t.TempDir()
	host := filepath.Join(dir, "host")
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(host, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(host, p), []byte("host "+p+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(host, "link")); err != nil {
		t.Fatal(err)
	}
	f = SyncFile{Path: filepath.Join(dir, "copy"), Mount: host, Base: filepath.Join(dir, "base")}
	for _, dst := range []string{f.Path, f.Base} {
		if err := cloneTree(host, dst); err != nil {
			t.Fatal(err)
		}
	}
	return f, dir
}

func read(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func TestCopyTreeKeepsTimes(t *testing.T) {
	f, _ := treeFixture(t)
	// Directories are not compared by time (a clone gives them a new one).
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		h, err := os.Lstat(filepath.Join(f.Mount, p))
		if err != nil {
			t.Fatal(err)
		}
		c, err := os.Lstat(filepath.Join(f.Base, p))
		if err != nil {
			t.Fatal(err)
		}
		if !h.ModTime().Equal(c.ModTime()) || h.Mode() != c.Mode() || h.Size() != c.Size() {
			t.Errorf("%s: copy %v %v %d, host %v %v %d", p, c.ModTime(), c.Mode(), c.Size(), h.ModTime(), h.Mode(), h.Size())
		}
	}
	if target, err := os.Readlink(filepath.Join(f.Path, "link")); err != nil || target != "a.txt" {
		t.Errorf("link = %q, %v", target, err)
	}
}

func TestSyncerWriteBackTree(t *testing.T) {
	f, _ := treeFixture(t)
	s := NewSyncer([]SyncFile{f})

	// Nothing changed: nothing is written, nothing is reported.
	if conflicts, err := s.WriteBack(); err != nil || len(conflicts) != 0 || len(s.Summary()) != 0 {
		t.Fatalf("WriteBack() = %v, %v, summary %v", conflicts, err, s.Summary())
	}

	// Inside: a.txt modified, sub/ removed with its file, new/c.txt
	// added, the link retargeted. Meanwhile the host grew a file of its
	// own, which must survive untouched.
	if err := os.WriteFile(filepath.Join(f.Path, "a.txt"), []byte("sandbox a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.Path, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.Path, "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Path, "new/c.txt"), []byte("c\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new/c.txt", filepath.Join(f.Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Mount, "hostonly.txt"), []byte("host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflicts, err := s.WriteBack()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("WriteBack() = %v, %v", conflicts, err)
	}
	if got := read(t, filepath.Join(f.Mount, "a.txt")); got != "sandbox a\n" {
		t.Errorf("a.txt = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(f.Mount, "sub/b.txt")); !os.IsNotExist(err) {
		t.Errorf("sub/b.txt should be removed on the host: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.Mount, "sub")); !os.IsNotExist(err) {
		t.Errorf("the emptied sub directory should be removed: %v", err)
	}
	if got := read(t, filepath.Join(f.Mount, "new/c.txt")); got != "c\n" {
		t.Errorf("new/c.txt = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(f.Mount, "new/c.txt")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("new/c.txt mode = %v, %v, want 0640", fi, err)
	}
	if target, _ := os.Readlink(filepath.Join(f.Mount, "link")); target != "new/c.txt" {
		t.Errorf("link = %q, want retargeted", target)
	}
	if got := read(t, filepath.Join(f.Mount, "hostonly.txt")); got != "host\n" {
		t.Errorf("hostonly.txt = %q, must be left alone", got)
	}
	summary := s.Summary()
	if len(summary) != 1 || !strings.HasPrefix(summary[0], f.Mount+": 4 files written back") {
		t.Errorf("Summary() = %q", summary)
	}
}

func TestSyncerWriteBackTreeConflicts(t *testing.T) {
	f, dir := treeFixture(t)
	s := NewSyncer([]SyncFile{f})

	// Both sides changed a.txt; the host removed sub/b.txt which the
	// sandbox changed; the sandbox removed the link which the host
	// retargeted; the host added hostnew.txt which the sandbox added too.
	write := func(root, p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(f.Path, "a.txt", "sandbox a\n")
	write(f.Mount, "a.txt", "host a\n")
	write(f.Path, "sub/b.txt", "sandbox b\n")
	if err := os.Remove(filepath.Join(f.Mount, "sub/b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Mount, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/b.txt", filepath.Join(f.Mount, "link")); err != nil {
		t.Fatal(err)
	}
	write(f.Path, "hostnew.txt", "sandbox\n")
	write(f.Mount, "hostnew.txt", "host\n")
	// The same change on both sides is no conflict.
	write(f.Path, "same.txt", "same\n")
	write(f.Mount, "same.txt", "same\n")

	conflicts, err := s.WriteBack()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range conflicts {
		names = append(names, strings.TrimPrefix(c.File.Mount, f.Mount+"/"))
	}
	slices.Sort(names)
	if want := []string{"a.txt", "hostnew.txt", "link", "sub/b.txt"}; !slices.Equal(names, want) {
		t.Fatalf("conflicts = %v, want %v", names, want)
	}
	if got := read(t, filepath.Join(f.Mount, "a.txt")); got != "host a\n" {
		t.Errorf("a.txt = %q, must be left alone on conflict", got)
	}
	byName := map[string]Conflict{}
	for _, c := range conflicts {
		byName[strings.TrimPrefix(c.File.Mount, f.Mount+"/")] = c
	}
	if c := byName["a.txt"]; string(c.Host) != "host a\n" || string(c.Copy) != "sandbox a\n" || c.File.Name() != c.File.Mount || c.Removed || c.Gone {
		t.Errorf("a.txt conflict = %+v", c)
	}
	if c := byName["sub/b.txt"]; !c.Gone || string(c.Copy) != "sandbox b\n" {
		t.Errorf("sub/b.txt conflict = %+v", c)
	}
	if c := byName["link"]; !c.Removed || string(c.Host) != "sub/b.txt" || !c.Link {
		t.Errorf("link conflict = %+v", c)
	}

	// Resolving: overwrite a.txt and sub/b.txt, remove the link, keep
	// hostnew.txt and save its copy.
	for _, n := range []string{"a.txt", "sub/b.txt", "link"} {
		if err := byName[n].Overwrite(); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if got := read(t, filepath.Join(f.Mount, "a.txt")); got != "sandbox a\n" {
		t.Errorf("a.txt = %q after Overwrite", got)
	}
	if got := read(t, filepath.Join(f.Mount, "sub/b.txt")); got != "sandbox b\n" {
		t.Errorf("sub/b.txt = %q after Overwrite", got)
	}
	if _, err := os.Lstat(filepath.Join(f.Mount, "link")); !os.IsNotExist(err) {
		t.Errorf("link should be removed after Overwrite: %v", err)
	}
	saveDir := filepath.Join(dir, "saved")
	if err := os.Mkdir(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	name, err := byName["hostnew.txt"].Save(saveDir, at)
	if err != nil || name != "hostnew.txt.20261025-120000" {
		t.Fatalf("Save() = %q, %v", name, err)
	}
	if got := read(t, filepath.Join(saveDir, name)); got != "sandbox\n" {
		t.Errorf("saved = %q", got)
	}
	if name, err := byName["link"].Save(saveDir, at); err != nil || name != "" {
		t.Errorf("Save() of a removed copy = %q, %v, want nothing saved", name, err)
	}
}

func TestSyncerWriteBackTreeSkipsNestedEntries(t *testing.T) {
	// ~/.claude.json synced into the copy of ~/.claude (see syncCopyName)
	// is an entry of its own: the directory's write-back leaves it out.
	f, dir := treeFixture(t)
	hostJSON := filepath.Join(dir, "host.json")
	if err := os.WriteFile(hostJSON, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	j := SyncFile{Path: filepath.Join(f.Path, ".claude.json"), Mount: hostJSON, Base: filepath.Join(f.Base, ".claude.json")}
	for _, dst := range []string{j.Path, j.Base} {
		if err := cloneTree(hostJSON, dst); err != nil {
			t.Fatal(err)
		}
	}
	s := NewSyncer([]SyncFile{f, j})
	if err := os.WriteFile(j.Path, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	conflicts, err := s.WriteBack()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("WriteBack() = %v, %v", conflicts, err)
	}
	if got := read(t, hostJSON); got != `{"a":2}` {
		t.Errorf("host.json = %q, want the copy written back", got)
	}
	if _, err := os.Lstat(filepath.Join(f.Mount, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("the nested entry must not be written into the directory: %v", err)
	}
	if len(s.Summary()) != 0 {
		t.Errorf("Summary() = %q, want nothing for the directory", s.Summary())
	}

	// The host file edited meanwhile is a conflict named by its host path.
	if err := os.WriteFile(hostJSON, []byte(`{"host":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.Path, []byte(`{"a":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	conflicts, err = s.WriteBack()
	if err != nil || len(conflicts) != 1 || conflicts[0].File.Name() != hostJSON {
		t.Fatalf("WriteBack() = %+v, %v, want one conflict on %s", conflicts, err, hostJSON)
	}
}

func TestParseSyncFileWithBase(t *testing.T) {
	sf, err := ParseSyncFile("/tmp/box/claude-1.sync/copy/.claude=/Users/u/.claude=/tmp/box/claude-1.sync/base/.claude")
	if err != nil || sf.Path != "/tmp/box/claude-1.sync/copy/.claude" || sf.Mount != "/Users/u/.claude" || sf.Base != "/tmp/box/claude-1.sync/base/.claude" {
		t.Errorf("ParseSyncFile = %+v, %v", sf, err)
	}
	if sf.Arg() != "/tmp/box/claude-1.sync/copy/.claude=/Users/u/.claude=/tmp/box/claude-1.sync/base/.claude" {
		t.Errorf("Arg() = %q", sf.Arg())
	}
	if sf.Name() != "/Users/u/.claude" {
		t.Errorf("Name() = %q, want the host path", sf.Name())
	}
	if (SyncFile{Path: "/h/.claude.json", Mount: "/run/box/sync/0"}).Name() != "/h/.claude.json" {
		t.Error("Name() of a Linux entry should be the sandbox path")
	}
}

func TestMakeSyncCopies(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	syncDir := filepath.Join(dir, "box", "claude-1.sync")
	p := &Plan{SyncDir: syncDir, Sync: []SyncFile{
		// Deliberately nested-first: the directory must still be cloned
		// before the file that goes inside it.
		{Path: filepath.Join(syncDir, "copy/.claude/.claude.json"), Mount: filepath.Join(home, ".claude.json"), Base: filepath.Join(syncDir, "base/.claude/.claude.json")},
		{Path: filepath.Join(syncDir, "copy/.claude"), Mount: filepath.Join(home, ".claude"), Base: filepath.Join(syncDir, "base/.claude")},
	}}
	if err := p.makeSyncCopies(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"copy/.claude/settings.json", "base/.claude/settings.json", "copy/.claude/.claude.json", "base/.claude/.claude.json"} {
		if _, err := os.Stat(filepath.Join(syncDir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(syncDir, "copy/.claude/projects")); err != nil || !fi.IsDir() {
		t.Errorf("copy/.claude/projects: %v", err)
	}
	if fi, err := os.Stat(syncDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("sync dir mode = %v, %v, want 0700", fi, err)
	}
	// Running again (a reused PID) starts from a fresh directory.
	if err := os.WriteFile(filepath.Join(syncDir, "copy/.claude/stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.makeSyncCopies(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "copy/.claude/stale")); !os.IsNotExist(err) {
		t.Errorf("stale copy should be gone: %v", err)
	}
	if (&Plan{}).makeSyncCopies() != nil {
		t.Error("no SyncDir: nothing to do")
	}
}
