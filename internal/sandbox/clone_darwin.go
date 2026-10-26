package sandbox

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// cloneTree copies src (a file or a directory tree) to dst, which must not
// exist. On APFS the copy is a clone: a copy-on-write snapshot that takes no
// time and no space until something writes to it, so a large directory
// (sessions, plugins) costs nothing to copy. When the clone is refused (a
// different volume, another filesystem) the tree is copied file by file.
func cloneTree(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cloning %s: %w", src, err)
	}
	return copyTree(src, dst)
}
