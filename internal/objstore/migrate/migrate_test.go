package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/migrate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// source is one domain naming objects, held in memory.
type source struct {
	mu         sync.Mutex
	named      map[objstore.Key]collect.Reference
	incomplete bool
	barriers   int
}

func (s *source) Name() string { return "tracker" }

func (s *source) Barrier(context.Context) (statelog.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.barriers++
	return statelog.Position{}, nil
}

func (s *source) Referenced(context.Context, []objstore.Key, statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	return nil, false, errors.New("the migration never asks which keys are named")
}

func (s *source) Each(_ context.Context, _ statelog.Position, _ objstore.Key, visit func(collect.Reference) error) (bool, error) {
	s.mu.Lock()
	refs := slices.Collect(maps.Values(s.named))
	s.mu.Unlock()
	for _, r := range refs {
		if err := visit(r); err != nil {
			return false, err
		}
	}
	return !s.incomplete, nil
}

// target is the twin, counting puts and able to fail them.
type target struct {
	*memobj.Backend
	puts    atomic.Int64
	failing atomic.Bool
}

func (t *target) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	t.puts.Add(1)
	if t.failing.Load() {
		return errors.New("the bucket stopped answering")
	}
	return t.Backend.Put(ctx, name, r, m)
}

type fixture struct {
	t      *testing.T
	src    *source
	from   *memobj.Backend
	store  *objstore.Store
	target *target
}

func newFixture(t *testing.T, files int) *fixture {
	t.Helper()
	from := memobj.New()
	store, err := objstore.NewStore(from)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, src: &source{named: map[objstore.Key]collect.Reference{}}, from: from,
		store: store, target: &target{Backend: memobj.New()}}
	for i := range files {
		o, err := store.Put(t.Context(), strings.NewReader(fmt.Sprintf("file %d", i)), 1<<20, objstore.PutMeta{})
		if err != nil {
			t.Fatal(err)
		}
		f.src.named[o.Key] = collect.Reference{Object: o, NamedBy: fmt.Sprintf("ENG/f%02d.md", i)}
	}
	return f
}

func (f *fixture) run(pin bool) (migrate.Report, error) {
	return migrate.Run(f.t.Context(), migrate.Options{Source: f.store, Target: f.target,
		References: collect.References{f.src}, Pin: pin})
}

// EVERY REFERENCED OBJECT IS COPIED, VERIFIED, AND A SECOND PASS COPIES
// NOTHING: the target is the record of what was copied, so a pass that
// stopped part of the way through resumes by running again.
func TestMigrateCopiesEveryReferencedObjectAndIsResumable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 12)
	f.target.failing.Store(true)
	if _, err := f.run(true); err == nil {
		t.Fatal("a pass whose target refused every put succeeded")
	}
	f.target.failing.Store(false)
	r, err := f.run(true)
	if err != nil || r.Copied != 12 || r.Present != 0 || !r.Done() {
		t.Fatalf("the first whole pass = %+v, %v; want twelve copied", r, err)
	}
	for k, ref := range f.src.named {
		info, err := f.target.Stat(t.Context(), k.Name())
		if err != nil || info.Size != ref.Object.Size {
			t.Fatalf("the target holds %s as %+v, %v", k, info, err)
		}
	}
	puts := f.target.puts.Load()
	again, err := f.run(true)
	if err != nil || again.Present != 12 || again.Copied != 0 || !again.Done() {
		t.Fatalf("a second pass = %+v, %v; want every object present", again, err)
	}
	if f.target.puts.Load() != puts {
		t.Fatalf("a pass over a target holding everything put %d objects", f.target.puts.Load()-puts)
	}
	if f.src.barriers != 3 {
		t.Fatalf("three pinned passes took %d barriers", f.src.barriers)
	}
	if _, err := f.run(false); err != nil || f.src.barriers != 3 {
		t.Fatalf("an unpinned pass took a barrier (%d) or failed: %v", f.src.barriers, err)
	}
}

// AN OBJECT THE TARGET HOLDS WRONG IS COPIED AGAIN, never counted present.
func TestAnObjectTheTargetHoldsWrongIsCopiedAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 3)
	if _, err := f.run(true); err != nil {
		t.Fatal(err)
	}
	var bent objstore.Key
	for k := range f.src.named {
		bent = k
		break
	}
	f.target.Corrupt(bent.Name(), []byte("short"))
	r, err := f.run(true)
	if err != nil || r.Copied != 1 || r.Present != 2 {
		t.Fatalf("a pass over one bent object = %+v, %v; want it copied again", r, err)
	}
}

// WHAT THE SOURCE CANNOT GIVE BACK IS NAMED AND NEVER STORED: a missing
// object and a damaged one reach the report by file, the damaged one's
// failed read leaves nothing in the target, and the pass still finishes.
func TestWhatTheSourceCannotGiveBackIsNamedAndNotStored(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 4)
	keys := slices.SortedFunc(maps.Keys(f.src.named), objstore.Key.Compare)
	if err := f.store.Delete(t.Context(), keys[0]); err != nil {
		t.Fatal(err)
	}
	f.from.Corrupt(keys[1].Name(), []byte("file X"))
	r, err := f.run(true)
	if err != nil || r.Unreadable != 2 || r.Copied != 2 || !r.Done() {
		t.Fatalf("the pass = %+v, %v; want two copied and two unreadable", r, err)
	}
	if _, err := f.target.Stat(t.Context(), keys[1].Name()); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("a damaged source object reached the target: %v", err)
	}
	damaged := 0
	for _, file := range r.Files {
		if file.Damaged {
			damaged++
		}
	}
	if len(r.Files) != 2 || damaged != 1 {
		t.Fatalf("the report names %+v, want one missing and one damaged file", r.Files)
	}
}

// AN INCOMPLETE ESTATE IS NEVER DONE, whatever was copied: a record this node
// could not apply may name an object the walk never saw.
func TestAnIncompleteEstateIsNeverDone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 2)
	f.src.incomplete = true
	r, err := f.run(true)
	if err != nil || r.Copied != 2 || r.Done() {
		t.Fatalf("a pass over an incomplete estate = %+v, %v; want copies and not done", r, err)
	}
}
