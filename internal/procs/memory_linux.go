package procs

import (
	"os"
	"strconv"
	"strings"
)

// TotalMemory returns the amount of physical memory in bytes, or 0 when it
// cannot be determined.
func TotalMemory() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemInfo(data)
}

// parseMemInfo extracts MemTotal (reported in kB) from /proc/meminfo.
func parseMemInfo(data []byte) uint64 {
	for line := range strings.SplitSeq(string(data), "\n") {
		v, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			return 0
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return 0
		}
		return n * 1024
	}
	return 0
}
