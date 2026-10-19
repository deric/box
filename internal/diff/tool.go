package diff

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Tool returns the path to colordiff(1) if installed, otherwise diff(1).
func Tool() (string, error) {
	if p, err := exec.LookPath("colordiff"); err == nil {
		return p, nil
	}
	p, err := exec.LookPath("diff")
	if err != nil {
		return "", fmt.Errorf("diff tool not found: %w", err)
	}
	return p, nil
}

// Write prints the unified diff of a and b to w, headed by the two names.
// It runs Tool when one is installed, for its colors and the output users
// know, and falls back to Unified when there is none or it fails. Nothing
// is written when the texts are equal.
func Write(w io.Writer, nameA, nameB string, a, b []byte) error {
	if bytes.Equal(a, b) {
		return nil
	}
	if tool, err := Tool(); err == nil {
		if out, err := run(tool, nameA, nameB, a, b); err == nil {
			_, err = w.Write(out)
			return err
		}
	}
	_, err := io.WriteString(w, Unified(nameA, nameB, a, b))
	return err
}

// run invokes tool -u on a, handed over as a temporary file, and b on its
// standard input.
func run(tool, nameA, nameB string, a, b []byte) ([]byte, error) {
	f, err := os.CreateTemp("", "box-diff-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(a); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	cmd := exec.Command(tool, "-u", "-L", nameA, "-L", nameB, f.Name(), "-")
	cmd.Stdin = bytes.NewReader(b)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		// diff exits 1 when the inputs differ; anything else is a failure.
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("%s failed: %w: %s", tool, err, strings.TrimSpace(stderr.String()))
		}
	}
	return out.Bytes(), nil
}
