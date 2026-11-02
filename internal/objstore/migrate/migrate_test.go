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
	named      map[objstore.Key]objstore.Reference
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

// Required is the audit's walk: every object a Required row of the source
// names, which a move that walked it in place of [source.Moved] would copy
// alone.
func (s *source) Required(statelog.Position) []objstore.Walk {
	return []objstore.Walk{&walk{s: s, pass: objstore.PassAudit}}
}

// Moved is the move's walk: every object the source names, the derived ones
// among them.
func (s *source) Moved(statelog.Position) []objstore.Walk {
	return []objstore.Walk{&walk{s: s, pass: objstore.PassMove}}
}

// walk is one of a [source]'s walks: every reference its pass reads on its
// first page, in key order — read when the walk is asked, as an estate's is —
// and none after.
type walk struct {
	s    *source
	pass objstore.Pass
	done bool
}

func (w *walk) Next(context.Context) ([]objstore.Reference, bool, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if w.done {
		return nil, !w.s.incomplete, nil
	}
	w.done = true
	var refs []objstore.Reference
	for _, ref := range slices.SortedFunc(maps.Values(w.s.named), func(a, b objstore.Reference) int {
		return a.Object.Key.Compare(b.Object.Key)
	}) {
		if w.pass.Reads(ref.Standing) {
			refs = append(refs, ref)
		}
	}
	return refs, !w.s.incomplete, nil
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
	f := &fixture{t: t, src: &source{named: map[objstore.Key]objstore.Reference{}}, from: from,
		store: store, target: &target{Backend: memobj.New()}}
	for i := range files {
		f.name(objstore.Required, fmt.Sprintf("ENG/f%02d.md", i), fmt.Sprintf("file %d", i))
	}
	return f
}

// name stores body in the source and records a row of standing s naming it,
// owned by namedBy.
func (f *fixture) name(s objstore.Standing, namedBy, body string) objstore.Object {
	f.t.Helper()
	o, err := f.store.Put(f.t.Context(), strings.NewReader(body), 1<<20, objstore.PutMeta{})
	if err != nil {
		f.t.Fatal(err)
	}
	f.src.named[o.Key] = objstore.Reference{Object: o, NamedBy: namedBy, Standing: s}
	return o
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

// A STORE MOVE COPIES WHAT A DERIVED ROW NAMES, VERIFIED, as it copies a
// file (ADR-0074): an object the engine can build again is one every node of
// the moved fleet would otherwise build again, at a cost that grows with what
// it was built from, where the copy costs its bytes once. It is counted apart
// from the files, and a second pass finds it present and copies nothing.
func TestAStoreMoveCopiesADerivedObjectVerified(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 3)
	var derived []objstore.Object
	for i := range 2 {
		derived = append(derived, f.name(objstore.Derived, fmt.Sprintf("repo/layer/%d", i),
			fmt.Sprintf("an index the engine builds again, %d", i)))
	}
	r, err := f.run(true)
	want := migrate.Counts{Referenced: 2, Copied: 2, CopiedBytes: derived[0].Size + derived[1].Size}
	if err != nil || r.Referenced != 3 || r.Copied != 3 || r.Derived != want || !r.Done() {
		t.Fatalf("the pass = %+v, %v; want the three files and the two derived objects "+
			"copied, each counted as what names it", r, err)
	}
	for _, o := range derived {
		info, err := f.target.Stat(t.Context(), o.Key.Name())
		if err != nil || info.Size != o.Size {
			t.Fatalf("the target holds derived object %s as %+v, %v", o.Key, info, err)
		}
	}
	puts := f.target.puts.Load()
	again, err := f.run(true)
	if err != nil || again.Derived != (migrate.Counts{Referenced: 2, Present: 2}) || !again.Done() {
		t.Fatalf("a second pass = %+v, %v; want both derived objects present", again, err)
	}
	if f.target.puts.Load() != puts {
		t.Fatalf("a pass over a target holding everything put %d objects", f.target.puts.Load()-puts)
	}
}

// A DERIVED OBJECT THE SOURCE CANNOT GIVE BACK HOLDS NOTHING UP: one lost
// from the source and one it holds damaged are counted apart and left behind
// — the engine builds each again, so nothing is lost — never named as a file
// no copy can bring back, and the pass is done; the damaged one's failed read
// leaves nothing in the target, as a file's does.
func TestADerivedObjectTheSourceCannotGiveBackHoldsNothingUp(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 2)
	lost := f.name(objstore.Derived, "repo/layer/lost", "an index the store lost")
	bent := f.name(objstore.Derived, "repo/layer/bent", "an index the store holds damaged")
	f.name(objstore.Derived, "repo/layer/kept", "an index the store holds whole")
	if err := f.store.Delete(t.Context(), lost.Key); err != nil {
		t.Fatal(err)
	}
	f.from.Corrupt(bent.Key.Name(), []byte("an index the store holds DAMAGED"))
	r, err := f.run(true)
	if err != nil || r.Unreadable != 0 || len(r.Files) != 0 || r.Copied != 2 ||
		r.Derived.Unreadable != 2 || r.Derived.Copied != 1 || !r.Done() {
		t.Fatalf("the pass = %+v, %v; want both files and the whole derived object copied, the "+
			"other two counted apart, no file named, and the pass done", r, err)
	}
	if _, err := f.target.Stat(t.Context(), bent.Key.Name()); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("a damaged derived object reached the target: %v", err)
	}
}

// A PASS IS DONE ONLY ONCE EVERY OBJECT IT COUNTED HAS SETTLED — a file's or
// a derived one's — present, copied or unreadable, from an estate read whole.
func TestAReportIsDoneOnlyOnceEveryObjectSettled(t *testing.T) {
	t.Parallel()
	settled := migrate.Counts{Referenced: 3, Present: 1, Copied: 1, Unreadable: 1}
	for name, c := range map[string]struct {
		r    migrate.Report
		done bool
	}{
		"everything settled":      {migrate.Report{Counts: settled, Derived: settled, Complete: true}, true},
		"a file unsettled":        {migrate.Report{Counts: migrate.Counts{Referenced: 1}, Derived: settled, Complete: true}, false},
		"a derived one unsettled": {migrate.Report{Counts: settled, Derived: migrate.Counts{Referenced: 1}, Complete: true}, false},
		"an estate read in part":  {migrate.Report{Counts: settled, Derived: settled}, false},
	} {
		if got := c.r.Done(); got != c.done {
			t.Errorf("%s: Done() = %v, want %v", name, got, c.done)
		}
	}
}
