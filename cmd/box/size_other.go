//go:build !unix

package main

import "io/fs"

// allocated returns the disk space info's file occupies; without block
// counts this is its length.
func allocated(info fs.FileInfo) int64 { return info.Size() }
