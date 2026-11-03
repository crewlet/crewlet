package objstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

// byName is a backend that keeps no order: the twin with its ranged listing
// hidden, as the broker's object store has none.
type byName struct{ objstore.Backend }

// A KEY'S ORDER IS ITS SPELLING'S: Compare sorts keys as their canonical
// spellings sort as bytes — how a bucket lists the names they are stored
// under and how a table's key column sorts them — so a range of keys and a
// range of names are one range.
func TestAKeysOrderIsItsSpellings(t *testing.T) {
	t.Parallel()
	var keys []objstore.Key
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i := range 500 {
		// SOME MINTED IN ONE MILLISECOND, so the random bits decide.
		keys = append(keys, objstore.KeyAt(at.Add(time.Duration(i%7)*time.Millisecond)))
	}
	byCompare := slices.SortedFunc(slices.Values(keys), objstore.Key.Compare)
	bySpelling := slices.SortedFunc(slices.Values(keys), func(a, b objstore.Key) int {
		return strings.Compare(a.Name(), b.Name())
	})
	if !slices.Equal(byCompare, bySpelling) {
		t.Fatal("keys sorted by Compare are not in the order their names sort in")
	}
	if k := keys[0]; k.Compare(k) != 0 {
		t.Fatal("a key does not compare equal to itself")
	}
}

// A RANGE OF KEYS IS EVERY KEY AFTER ITS START AND THROUGH ITS END, in key
// order — from the first key when the start is zero, past every name outside
// the namespace — and nothing that is not a key.
func TestARangeOfKeysListsTheKeysInIt(t *testing.T) {
	t.Parallel()
	mem := memobj.New()
	s := newStore(t, mem)
	var keys []objstore.Key
	for i := range 9 {
		o, err := s.Put(t.Context(), strings.NewReader(strings.Repeat("x", i)), 1<<20, objstore.PutMeta{})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	slices.SortFunc(keys, objstore.Key.Compare)
	// NAMES THAT ARE NO KEY, before the namespace and inside it.
	for _, name := range []string{"0-before-the-namespace", "files/", "files/" + strings.ToUpper(keys[3].String()),
		"files/not-a-key", "zz-after-it"} {
		if err := mem.Put(t.Context(), name, strings.NewReader("theirs"), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	listKeys := func(after, through objstore.Key) []objstore.Key {
		t.Helper()
		var got []objstore.Key
		if err := s.ListKeys(t.Context(), after, through, func(k objstore.Key, info objstore.Info) error {
			if info.Name != k.Name() {
				t.Errorf("ListKeys handed %s on as %q", k, info.Name)
			}
			got = append(got, k)
			return nil
		}); err != nil {
			t.Fatalf("ListKeys: %v", err)
		}
		return got
	}
	if got := listKeys(objstore.Key{}, keys[len(keys)-1]); !slices.Equal(got, keys) {
		t.Errorf("from the first key = %v, want every key: %v", got, keys)
	}
	if got := listKeys(keys[2], keys[5]); !slices.Equal(got, keys[3:6]) {
		t.Errorf("(keys[2], keys[5]] = %v, want %v", got, keys[3:6])
	}
	if !s.Ranged() {
		t.Error("a store over the twin says it cannot list a range")
	}
}

// A STORE THAT KEEPS NO ORDER IS TOLD SO, and so is one caught breaking it:
// a caller reads an object missing from a range as one the store does not
// hold, so a listing out of order would read objects the store holds as lost.
func TestARangeTheStoreCannotOrderIsUnordered(t *testing.T) {
	t.Parallel()
	s := newStore(t, byName{memobj.New()})
	if s.Ranged() {
		t.Fatal("a store over a backend with no ranged listing says it has one")
	}
	err := s.ListKeys(t.Context(), objstore.Key{}, objstore.KeyAt(time.Now()), func(objstore.Key, objstore.Info) error {
		t.Error("a store with no order listed a key")
		return nil
	})
	if !errors.Is(err, objstore.ErrUnordered) {
		t.Fatalf("ListKeys over a store with no order = %v, want ErrUnordered", err)
	}

	at := time.Now()
	first, second := objstore.KeyAt(at), objstore.KeyAt(at.Add(time.Second))
	for name, lists := range map[string][]string{
		"backwards":         {second.Name(), first.Name()},
		"twice":             {first.Name(), first.Name()},
		"past its end":      {first.Name(), objstore.KeyAt(at.Add(time.Hour)).Name()},
		"before its start":  {"files/"},
		"outside the space": {"a-name-before-the-namespace"},
	} {
		s := newStore(t, scripted{Backend: memobj.New(), names: lists})
		var visited int
		err := s.ListKeys(t.Context(), objstore.Key{}, second, func(objstore.Key, objstore.Info) error {
			visited++
			return nil
		})
		if !errors.Is(err, objstore.ErrUnordered) {
			t.Errorf("a listing %s = %v after %d keys, want ErrUnordered", name, err, visited)
		}
	}
}

// scripted is a ranged backend whose listing hands on names in the order it
// is given, whatever range it was asked for.
type scripted struct {
	objstore.Backend
	names []string
}

func (s scripted) ListRange(_ context.Context, _, _ string, visit func(objstore.Info) error) error {
	for _, n := range s.names {
		if err := visit(objstore.Info{Name: n, Written: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}
