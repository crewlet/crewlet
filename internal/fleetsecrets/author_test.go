package fleetsecrets_test

import (
	"errors"
	"strings"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHO A WRITE NAMES IS BOUNDED BY EVERY WRITE, an operator's and the engine's,
// before anything is sealed: the author and the credential at a login's width,
// the kind one of iam's, and a create's operation id at statelog's bound. Each
// is a field the record keeps, so past it a value at its bound carries the
// record past coord.MaxSecretRecordBytes, refused by the store in words that
// name neither the field nor its writer (ADR-0175).
//
// Mutation: drop CheckAuthor from checkWrite, and every write lands, or
// checkOpID from Create, and the long id does.
func TestAProvenanceNoRecordKeepsIsRefusedByEveryWrite(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	s := fleetsecrets.New(fleet, ring(t, "k1"))
	wide := strings.Repeat("o", iam.MaxLogin+1)
	for what, by := range map[string]secrets.Author{
		"an author past a login":     {Name: wide, Kind: "operator"},
		"a credential past a login":  {Name: "sam", Kind: "operator", OperatorID: wide},
		"no kind":                    {Name: "sam"},
		"a kind iam does not define": {Name: "sam", Kind: "robot"},
	} {
		for _, w := range []struct {
			via   string
			write func() error
		}{
			{"a set", func() error { return s.Set(t.Context(), "TOKEN", "v", by, secrets.SourceAPI, clock) }},
			{"a write-only set", func() error {
				return s.SetWriteOnly(t.Context(), "TOKEN", "v", by, secrets.SourceAPI, clock)
			}},
			{"a create", func() error {
				_, err := s.Create(t.Context(), "TOKEN", "v", by, secrets.SourceAPI, clock, "op")
				return err
			}},
			{"a reclaim", func() error {
				_, err := s.Reclaim(t.Context(), "TOKEN", "v", by, secrets.SourceAPI, clock, "op", clock)
				return err
			}},
			{"an engine's create", func() error {
				_, err := s.Estate().Create(t.Context(), "iam/key", "v", by, secrets.SourceIAM, clock)
				return err
			}},
			{"an engine's swap", func() error {
				_, err := s.Estate().Swap(t.Context(), "iam/key", "v", 1, by, secrets.SourceIAM, clock)
				return err
			}},
		} {
			if err := w.write(); !errors.Is(err, fleetsecrets.ErrInvalidAuthor) {
				t.Errorf("%s by %s = %v, want fleetsecrets.ErrInvalidAuthor", w.via, what, err)
			}
		}
	}
	longOp := strings.Repeat("i", statelog.MaxCallerOpIDBytes+1)
	if _, err := s.Create(t.Context(), "TOKEN", "v", sam, secrets.SourceAPI, clock, longOp); err == nil {
		t.Errorf("a create under a %d-byte operation id was written", len(longOp))
	}
	if rows, err := fleet.SecretValues(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("the refused writes left %d rows (%v), want none", len(rows), err)
	}
}
