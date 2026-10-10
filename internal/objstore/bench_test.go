package objstore_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// BenchmarkHashOf is the floor under every verified byte: a put hashes each
// byte it stores once and a read each byte it hands out once, so one stream's
// verified throughput is at most this CPU's SHA-256 rate, whatever the
// backend.
func BenchmarkHashOf(b *testing.B) {
	data := make([]byte, objstore.MiB)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		objstore.HashOf(data)
	}
}
