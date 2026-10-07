//go:build !linux && !darwin

package procs

// TotalMemory returns the amount of physical memory in bytes, or 0 when it
// cannot be determined.
func TotalMemory() uint64 { return 0 }
