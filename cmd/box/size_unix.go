//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// allocated returns the disk space info's file occupies: its allocated
// blocks, which is less than its length for sparse files and more for small
// ones, as du reports it.
func allocated(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}
