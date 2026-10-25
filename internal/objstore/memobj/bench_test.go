package memobj_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
)

// BenchmarkBackend measures the twin through the suite every backend runs.
//
// Its figures are the store's own cost with no transport under it — the
// counting and the hashing — PLUS THE TWIN'S, which no backend pays and which
// is not small: a put grows its copy of the body through io.ReadAll, buffer
// after buffer, and hashes the whole object a second time for the digest it
// keeps apart from the bytes ([memobj.Backend.Corrupt] changes one and not the
// other), and a get copies the whole range it answers before it streams a
// byte. So they are NOT A FLOOR under any backend's — measured, the S3 fake
// over https put a mebibyte faster than the twin, and over http read one
// faster, on a CPU with SHA instructions and on one without. What every
// verified byte does sit above is internal/objstore's BenchmarkHashOf.
func BenchmarkBackend(b *testing.B) {
	objstoretest.Bench(b, func(*testing.B) objstoretest.BenchBackend {
		return objstoretest.BenchBackend{Backend: memobj.New()}
	})
}
