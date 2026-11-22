//go:build !linux

package filesystem

// openLargeFile is a no-op outside Linux. Only Linux is supported; this file
// exists so the code still builds on development machines.
const openLargeFile = 0
