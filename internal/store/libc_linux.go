//go:build linux

package store

import "path/filepath"

// muslLoaders is where musl installs its dynamic loader. The name carries the
// architecture (ld-musl-x86_64.so.1, ld-musl-aarch64.so.1), so it is matched
// rather than listed — the engine would otherwise have to know every
// architecture musl names differently from Go.
const muslLoaders = "/lib/ld-musl-*.so.1"

// runningOnMusl reports whether this host looks like a musl system.
//
// EVIDENCE, NOT A VERDICT, and it is only ever consulted after a load has
// already failed — see [libcAdvice]. A distribution can carry musl alongside
// glibc as an ordinary package, so a true here does not prove that glibc is
// absent, and refusing to start on it would break a working deployment to
// prevent a hypothetical one. Used as a hint on an error path, a false
// positive costs a sentence.
func runningOnMusl() bool {
	matches, err := filepath.Glob(muslLoaders)
	return err == nil && len(matches) > 0
}
