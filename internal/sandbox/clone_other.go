//go:build !darwin

package sandbox

// cloneTree copies src (a file or a directory tree) to dst, which must not
// exist. Only macOS can clone it; elsewhere it is copied file by file.
func cloneTree(src, dst string) error {
	return copyTree(src, dst)
}
