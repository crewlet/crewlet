package objstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

// refusingBatch is the twin deleting in batches, but refusing the names in refuse
// name by name, as S3 answers a key it would not remove.
type refusingBatch struct {
	*memobj.Backend
	refuse map[string]bool
}

func (r *refusingBatch) DeleteBatch(ctx context.Context, names []string) error {
	failed := map[string]error{}
	var rest []string
	for _, name := range names {
		if r.refuse[name] {
			failed[name] = errors.New("AccessDenied")
			continue
		}
		rest = append(rest, name)
	}
	if err := r.Backend.DeleteBatch(ctx, rest); err != nil {
		return err
	}
	if len(failed) > 0 {
		return &objstore.DeleteBatchError{Failed: failed}
	}
	return nil
}

// A BATCH OF KEYS IS DELETED IN ONE BATCH DELETION, and the count it answers is
// what the store says it removed: every key, all but those it refused by name,
// and none — with [objstore.ErrNoBatch] — on a backend that has no such verb,
// which removes nothing, so its caller can delete each key instead.
func TestABatchOfKeysIsCountedAsTheStoreAnswered(t *testing.T) {
	t.Parallel()
	put := func(s *objstore.Store, n int) []objstore.Key {
		t.Helper()
		var keys []objstore.Key
		for i := range n {
			o, err := s.Put(t.Context(), strings.NewReader(strings.Repeat("x", i)), 1<<20, objstore.PutMeta{})
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, o.Key)
		}
		return keys
	}
	held := func(s *objstore.Store, k objstore.Key) bool {
		t.Helper()
		_, err := s.Stat(t.Context(), k)
		if errors.Is(err, objstore.ErrNotFound) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}

	t.Run("every key", func(t *testing.T) {
		t.Parallel()
		s := newStore(t, memobj.New())
		keys := put(s, 5)
		if !s.BatchDeletes() {
			t.Fatal("the twin deletes in batches, and the store says it does not")
		}
		if n, err := s.DeleteKeys(t.Context(), keys[:4]); n != 4 || err != nil {
			t.Fatalf("DeleteKeys = %d, %v; want all 4", n, err)
		}
		for _, k := range keys[:4] {
			if held(s, k) {
				t.Fatalf("%s survived a batch deletion that counted it", k)
			}
		}
		if !held(s, keys[4]) {
			t.Fatal("a key the batch was not handed was deleted")
		}
	})
	t.Run("some refused", func(t *testing.T) {
		t.Parallel()
		mem := memobj.New()
		r := &refusingBatch{Backend: mem, refuse: map[string]bool{}}
		s := newStore(t, r)
		keys := put(s, 4)
		r.refuse[keys[1].Name()] = true
		n, err := s.DeleteKeys(t.Context(), keys)
		var refused *objstore.DeleteBatchError
		if n != 3 || !errors.As(err, &refused) || refused.Failed[keys[1].Name()] == nil {
			t.Fatalf("DeleteKeys = %d, %v; want 3, naming the key refused", n, err)
		}
		if !held(s, keys[1]) || held(s, keys[0]) {
			t.Fatal("the batch removed the refused key, or kept one it counted")
		}
	})
	t.Run("no such verb", func(t *testing.T) {
		t.Parallel()
		s := newStore(t, byName{memobj.New()})
		keys := put(s, 2)
		if s.BatchDeletes() {
			t.Fatal("a backend with no batch verb is said to have one")
		}
		if n, err := s.DeleteKeys(t.Context(), keys); n != 0 || !errors.Is(err, objstore.ErrNoBatch) {
			t.Fatalf("DeleteKeys on a backend with no batch verb = %d, %v; want none, ErrNoBatch", n, err)
		}
		if !held(s, keys[0]) || !held(s, keys[1]) {
			t.Fatal("a batch refused for want of the verb deleted something")
		}
	})
	t.Run("past the batch", func(t *testing.T) {
		t.Parallel()
		s := newStore(t, memobj.New())
		keys := put(s, 1)
		over := make([]objstore.Key, objstore.MaxDeleteBatch+1)
		for i := range over {
			over[i] = keys[0]
		}
		if n, err := s.DeleteKeys(t.Context(), over); n != 0 || err == nil || errors.Is(err, objstore.ErrNoBatch) {
			t.Fatalf("DeleteKeys of %d keys = %d, %v; want it refused, and not as a missing verb", len(over), n, err)
		}
		if !held(s, keys[0]) {
			t.Fatal("a batch refused for its size deleted something")
		}
	})
}
