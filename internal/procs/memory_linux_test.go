package procs

import "testing"

func TestParseMemInfo(t *testing.T) {
	data := "MemTotal:       16303220 kB\nMemFree:         1234567 kB\n"
	if got, want := parseMemInfo([]byte(data)), uint64(16303220*1024); got != want {
		t.Errorf("parseMemInfo = %d, want %d", got, want)
	}
	if got := parseMemInfo([]byte("MemFree: 1 kB\n")); got != 0 {
		t.Errorf("parseMemInfo without MemTotal = %d, want 0", got)
	}
}
