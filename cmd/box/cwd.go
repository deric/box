package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// cwdRefusal says why box run refuses to start in cwd, or returns "" when
// it may. The working directory is mounted read-write, so running in the
// root directory, in the home directory or in one of its ancestors
// (/home, /Users, ...) would hand the sandbox the whole host or the whole
// home directory, which is what the sandbox exists to withhold. home may
// be empty when it is unknown; then only the root directory is refused.
func cwdRefusal(cwd, home string) string {
	cwd = filepath.Clean(cwd)
	if cwd == filepath.Dir(cwd) {
		return "it is the root directory"
	}
	if home == "" {
		return ""
	}
	home = filepath.Clean(home)
	switch {
	case cwd == home:
		return "it is the home directory"
	case strings.HasPrefix(home, cwd+string(filepath.Separator)):
		return fmt.Sprintf("it contains the home directory %s", home)
	}
	return ""
}
