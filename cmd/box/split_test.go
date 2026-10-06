package main

import "testing"

func TestSplitEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  bool
		bin  string
		pid  int
		ok   bool
	}{
		{"claude-1234", false, "claude", 1234, true},
		{"claude-code-1234", false, "claude-code", 1234, true},
		{"claude-1234-home-deric-dev-box", true, "claude", 1234, true},
		{"claude-code-1234-home-deric-dev-42", true, "claude-code", 1234, true},
		{"tool-7-", true, "tool", 7, true},
		{"claude-abc", false, "", 0, false},
		{"claude-home-deric", true, "", 0, false},
		{"-1234", false, "", 0, false},
	} {
		bin, pid, ok := splitEntry(tc.name, tc.log)
		if bin != tc.bin || pid != tc.pid || ok != tc.ok {
			t.Errorf("splitEntry(%q, %v) = %q, %d, %v; want %q, %d, %v", tc.name, tc.log, bin, pid, ok, tc.bin, tc.pid, tc.ok)
		}
	}
}
