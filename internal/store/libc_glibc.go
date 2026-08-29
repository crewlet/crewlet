//go:build linux && !musl

package store

// builtForMusl is false: this is the glibc linux build. See libc_musl.go for
// what the constant is for and why it is a build-time fact.
const builtForMusl = false
