package fleetsecrets_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A NAME HAS A WIDEST FORM, since a credential's record is stored under it and
// keeps it: a name past secrets.MaxNameBytes is refused as an invalid name by
// every write, an operator's and the engine's, before anything is sealed, and
// a read of one answers absent without asking the store.
//
// Mutation: drop checkNameBytes from secrets.CheckName (or CheckEstateName),
// and the long name is written (coord.MaxSecretRecordBytes holds the record,
// not the name); drop namesNoRow, and the reads ask the store.
func TestANamePastItsBoundIsRefusedByEveryWriteAndNamesNoRow(t *testing.T) {
	t.Parallel()
	counted := &countingFleet{Fleet: coordmem.NewFleet()}
	s := fleetsecrets.New(counted, ring(t, "k1"))
	long := strings.Repeat("A", secrets.MaxNameBytes+1)
	for _, c := range []struct {
		what  string
		write func() error
	}{
		{"a set", func() error { return s.Set(t.Context(), long, "v", sam, secrets.SourceAPI, clock) }},
		{"a write-only set", func() error {
			return s.SetWriteOnly(t.Context(), long, "v", sam, secrets.SourceAPI, clock)
		}},
		{"a create", func() error {
			_, err := s.Create(t.Context(), long, "v", sam, secrets.SourceAPI, clock, "op")
			return err
		}},
		{"an engine's create", func() error {
			_, err := s.Estate().Create(t.Context(), "iam/"+strings.Repeat("k", secrets.MaxNameBytes), "v",
				nodeA, secrets.SourceIAM, clock)
			return err
		}},
	} {
		if err := c.write(); !errors.Is(err, secrets.ErrInvalidName) {
			t.Errorf("%s of a %d-byte name = %v, want secrets.ErrInvalidName", c.what, len(long), err)
		}
	}
	if rows, err := counted.Fleet.SecretValues(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("the refused writes left %d rows (%v), want none", len(rows), err)
	}
	counted.reads.Store(0)
	if _, found, err := s.Describe(t.Context(), long); found || err != nil {
		t.Errorf("describe a long name = %t, %v; want absent", found, err)
	}
	if _, err := s.Get(t.Context(), long); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("get a long name = %v, want secrets.ErrNotFound", err)
	}
	if removed, err := s.Unset(t.Context(), long); removed || err != nil {
		t.Errorf("unset a long name = %t, %v; want nothing removed", removed, err)
	}
	if _, _, found, err := s.OpOf(t.Context(), long); found || err != nil {
		t.Errorf("the op of a long name = %t, %v; want absent", found, err)
	}
	if n := counted.reads.Load(); n != 0 {
		t.Errorf("reading a name no row can have asked the store %d times, want none", n)
	}
	at := strings.Repeat("A", secrets.MaxNameBytes)
	if err := s.Set(t.Context(), at, "v", sam, secrets.SourceAPI, clock); err != nil {
		t.Errorf("a set of a name at its bound = %v, want it written", err)
	}
}

// countingFleet counts the reads and removals of one key it is asked for.
type countingFleet struct {
	coord.Fleet
	reads atomic.Int64
}

func (f *countingFleet) Secret(ctx context.Context, name string) (coord.SecretRecord, bool, error) {
	f.reads.Add(1)
	return f.Fleet.Secret(ctx, name)
}

func (f *countingFleet) DeleteSecret(ctx context.Context, name string) (bool, error) {
	f.reads.Add(1)
	return f.Fleet.DeleteSecret(ctx, name)
}
