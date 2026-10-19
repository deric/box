package main

import (
	"testing"

	"github.com/deric/box/internal/procs"
)

func TestUniqueName(t *testing.T) {
	running := []procs.Sandbox{{Name: "box"}, {Name: "box-2"}, {Name: "api-3"}}
	for _, tc := range []struct{ base, want string }{
		{"box", "box-3"},
		{"api", "api"},
		{"box-2", "box-2-2"},
		{"other", "other"},
	} {
		if got := uniqueName(tc.base, running); got != tc.want {
			t.Errorf("uniqueName(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
	if got := uniqueName("box", nil); got != "box" {
		t.Errorf("uniqueName with nothing running = %q", got)
	}
}
