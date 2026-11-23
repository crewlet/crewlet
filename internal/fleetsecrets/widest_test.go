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
// the 89,931 bytes coord.MaxSecretRecordBytes states. Each field is at the
// bound its writer holds it to, in the characters JSON spells longest that
// the bound's grammar admits; the fields below say where each is held.
//
// Mutation: lift secrets.MaxNameBytes to 16 KiB and write a name that long,
// or lower coord.MaxSecretRecordBytes to 80 KiB, and this is red.
func TestTheWidestSecretRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	// THE KEY ID: Tier A holds it to [A-Za-z0-9._-] and MaxStreamKeyIDBytes
	// (config.Secrets), and it rides the envelope and the record's own field.
	// The value: secrets.CheckValue, sealed into base64.
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
	// THE NAME: secrets.CheckName's grammar, which JSON writes as itself.
	name := strings.Repeat("A", secrets.MaxNameBytes)
	// THE AUTHOR AND CREDENTIAL: CheckAuthor's width and no grammar, so
	// written in the character JSON escapes to six bytes; the kind and the
	// source are enumerations.
	escaped := func(n int) string { return strings.Repeat("<", n) }
	by := secrets.Author{Name: escaped(iam.MaxLogin), Kind: longest(kinds),
		OperatorID: escaped(iam.MaxLogin)}
	at := time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.UTC)
	created, err := s.Create(t.Context(), name, strings.Repeat("v", secrets.MaxValueBytes), by,
		secrets.Source(longest(sources)), at, escaped(statelog.MaxCallerOpIDBytes)) // checkOpID
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
	const stated = 89_931
	if len(stored) != stated {
		t.Errorf("the widest record is stored as %d bytes; MaxSecretRecordBytes's arithmetic states %d",
			len(stored), stated)
	}
}
