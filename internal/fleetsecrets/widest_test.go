package fleetsecrets_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WIDEST CREDENTIAL'S RECORD FITS ITS CLASS'S BOUND, written as a writer
// writes it and stored as both backends store it (coord.EncodeSecret), and is
// the 88,651 bytes coord.MaxSecretRecordBytes states: a 64 KiB value sealed
// under a 255-byte key id, a 255-byte name, an author and a credential of a
// login's 64 bytes each, the longest kind and source, a 128-byte operation id,
// the write-only mark and the instant to the nanosecond. Nothing the record
// keeps is unbounded.
//
// Mutation: lift secrets.MaxNameBytes to 16 KiB and write a name that long,
// or lower coord.MaxSecretRecordBytes to 80 KiB, and this is red.
func TestTheWidestSecretRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, strings.Repeat("k", secrets.MaxStreamKeyIDBytes)))
	longest := func(words []string) string {
		return slices.MaxFunc(words, func(a, b string) int { return len(a) - len(b) })
	}
	var kinds, sources []string
	for _, k := range iam.ActorKinds {
		kinds = append(kinds, string(k))
	}
	for _, source := range secrets.Sources {
		sources = append(sources, string(source))
	}
	name := strings.Repeat("A", secrets.MaxNameBytes)
	by := secrets.Author{Name: strings.Repeat("a", iam.MaxLogin), Kind: longest(kinds),
		OperatorID: strings.Repeat("o", iam.MaxLogin)}
	at := time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.UTC)
	created, err := s.Create(t.Context(), name, strings.Repeat("v", secrets.MaxValueBytes), by,
		secrets.Source(longest(sources)), at, strings.Repeat("i", statelog.MaxCallerOpIDBytes))
	if err != nil || !created {
		t.Fatalf("write the widest record = %t, %v", created, err)
	}
	rec, found, err := fleet.Secret(t.Context(), name)
	if err != nil || !found {
		t.Fatalf("read the widest record back = %t, %v", found, err)
	}
	// A create-only retry of a write-only row keeps the mark beside its
	// operation id (Store.Reclaim), so the widest carries both.
	rec.WriteOnly, rec.Version = true, 0
	stored, err := coord.EncodeSecret(rec)
	if err != nil {
		t.Fatalf("store the widest record: %v, want it within %d bytes", err, coord.MaxSecretRecordBytes)
	}
	const stated = 88_651
	if len(stored) != stated {
		t.Errorf("the widest record is stored as %d bytes; MaxSecretRecordBytes's arithmetic states %d",
			len(stored), stated)
	}
}
