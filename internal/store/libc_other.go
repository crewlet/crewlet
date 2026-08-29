//go:build !linux

package store

// builtForMusl is meaningless off linux — darwin has one C library and the
// question does not arise — but the constant exists on every platform so that
// [libcAdvice] is one function rather than one per GOOS.
const builtForMusl = false

// runningOnMusl is always false off linux: musl is a linux C library, so the
// mismatch this reports cannot happen here.
func runningOnMusl() bool { return false }
