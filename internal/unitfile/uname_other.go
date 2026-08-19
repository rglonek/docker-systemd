//go:build !linux

package unitfile

func kernelRelease() string { return "" }
