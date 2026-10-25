//go:build !linux

package cache_test

import "testing"

// dropCached answers false: only Linux's posix_fadvise drops a file's cached
// pages on request, so elsewhere a benchmark's cold arm does not run.
func dropCached(*testing.B, string) bool { return false }
