//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileDeniesCapabilities(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	restrict := write("restrict", "profile bwrap /usr/bin/bwrap {\n  allow capability,\n  allow userns,\n}\nprofile unpriv_bwrap {\n  allow userns,\n  audit deny capability,\n}\n")
	permissive := write("permissive", "profile bwrap /usr/bin/bwrap flags=(unconfined) {\n  userns,\n  # audit deny capability, is a comment, not a rule\n}\n")
	if !profileDeniesCapabilities(restrict) {
		t.Error("restrict: deny capability not seen")
	}
	if profileDeniesCapabilities(permissive) {
		t.Error("permissive: deny capability seen")
	}
	if profileDeniesCapabilities(filepath.Join(dir, "missing")) {
		t.Error("missing file: deny capability seen")
	}
}
