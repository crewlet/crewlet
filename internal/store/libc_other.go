//go:build !linux

package store

// runningOnMusl is always false off linux: musl is a linux C library, so the
// mismatch it reports cannot arise here. It exists on every platform so that
// [libcAdvice] is one function rather than one per GOOS.
func runningOnMusl() bool { return false }
