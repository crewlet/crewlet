package objstore_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A LIMIT IS SAID IN THE UNIT A PERSON READS IT IN: whole GiB and MiB as such,
// and anything else in bytes rather than a rounded figure that is not the
// limit.
//
// Mutation: drop either arm and its row reads in bytes.
func TestASizeIsSaidInThePersonsUnit(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{
		1 << 30:       "1 GiB",
		100 << 20:     "100 MiB",
		1<<20 + 1:     "1048577 bytes",
		512:           "512 bytes",
		3 << 30:       "3 GiB",
		(1 << 30) / 2: "512 MiB",
	} {
		if got := objstore.SizeWords(n); got != want {
			t.Errorf("SizeWords(%d) = %q, want %q", n, got, want)
		}
	}
}
