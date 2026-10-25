package objstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// pagedWalk is a walk over references held in memory, handed on page pages at
// a time, counting how often it was asked.
type pagedWalk struct {
	refs  []objstore.Reference
	page  int
	next  int
	asked int
}

func (w *pagedWalk) Next(context.Context) ([]objstore.Reference, bool, error) {
	w.asked++
	end := min(w.next+w.page, len(w.refs))
	out := w.refs[w.next:end]
	w.next = end
	return out, true, nil
}

// keys mints n keys in their own order, a millisecond apart, so a test can
// deal them out to walks and know the order the merge must restore.
func keys(n int) []objstore.Key {
	base := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	out := make([]objstore.Key, n)
	for i := range out {
		out[i] = objstore.KeyAt(base.Add(time.Duration(i) * time.Millisecond))
	}
	slices.SortFunc(out, objstore.Key.Compare)
	return out
}

func ref(k objstore.Key, table string) objstore.Reference {
	return objstore.Reference{Object: objstore.Object{Key: k, Hash: objstore.HashOf([]byte(k.String()))},
		NamedBy: "ENG/" + k.String(), Table: table}
}

// drain takes every reference a merge hands on.
func drain(t *testing.T, m *objstore.Merge) []objstore.Reference {
	t.Helper()
	var out []objstore.Reference
	for {
		r, ok, err := m.Next(t.Context())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, r)
	}
}

// EVERY REFERENCE OF EVERY WALK COMES OUT ONCE, IN ONE KEY ORDER, however the
// keys are dealt across the walks and however their pages are cut — the order
// the audit cuts its ranges from and the backup de-duplicates across.
func TestAMergeHandsOnEveryReferenceInKeyOrder(t *testing.T) {
	t.Parallel()
	all := keys(97)
	walks := []*pagedWalk{{page: 3}, {page: 7}, {page: 50}}
	for i, k := range all {
		w := walks[(i*i+i/3)%len(walks)]
		w.refs = append(w.refs, ref(k, fmt.Sprint("t", (i*i+i/3)%len(walks))))
	}
	var named []objstore.NamedWalk
	for i, w := range walks {
		named = append(named, objstore.NamedWalk{Name: fmt.Sprint("t", i), Walk: w})
	}
	m := objstore.MergeWalks(named...)
	got := drain(t, m)
	if len(got) != len(all) {
		t.Fatalf("the merge handed on %d references of %d", len(got), len(all))
	}
	for i, r := range got {
		if r.Object.Key != all[i] {
			t.Fatalf("reference %d is %s, want %s: the merge is out of key order", i, r.Object.Key, all[i])
		}
	}
	if !m.Complete() {
		t.Error("a merge of walks every page of which was complete reports itself incomplete")
	}
}

// A WALK'S NEXT PAGE IS READ ONLY ONCE ITS PAGE IS SPENT, so a merge holds a
// page of each walk — the walks times a page — and never reads a table ahead
// of what it has handed on.
func TestAMergeReadsAWalksNextPageOnlyWhenItsPageIsSpent(t *testing.T) {
	t.Parallel()
	const page = 10
	all := keys(200)
	a, b := &pagedWalk{page: page}, &pagedWalk{page: page}
	for i, k := range all {
		if i%2 == 0 {
			a.refs = append(a.refs, ref(k, "a"))
		} else {
			b.refs = append(b.refs, ref(k, "b"))
		}
	}
	m := objstore.MergeWalks(objstore.NamedWalk{Name: "a", Walk: a}, objstore.NamedWalk{Name: "b", Walk: b})
	taken := map[string]int{}
	for range 75 {
		r, ok, err := m.Next(t.Context())
		if err != nil || !ok {
			t.Fatalf("Next = %v, %v", ok, err)
		}
		taken[r.Table]++
	}
	for name, w := range map[string]*pagedWalk{"a": a, "b": b} {
		if most := (taken[name]+page-1)/page + 1; w.asked > most {
			t.Errorf("walk %s was asked for %d pages after %d of its references were handed on; "+
				"at most %d — the merge read ahead of what it handed on", name, w.asked, taken[name], most)
		}
	}
}

// A WALK WHOSE KEYS GO BACKWARDS IS REFUSED, naming the walk: the order is
// what every reader of the merge relies on, and a reference out of it would be
// asked about in a range that cannot hold it, or carried twice.
func TestAMergeRefusesAWalkThatGoesBackwards(t *testing.T) {
	t.Parallel()
	ks := keys(2)
	backwards := &pagedWalk{page: 5, refs: []objstore.Reference{ref(ks[1], "a"), ref(ks[0], "a")}}
	m := objstore.MergeWalks(objstore.NamedWalk{Name: "backwards", Walk: backwards})
	var err error
	for {
		var ok bool
		if _, ok, err = m.Next(t.Context()); err != nil || !ok {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("a walk out of key order was merged: %v", err)
	}
}

// TWO WALKS NAMING ONE KEY HAND IT ON ADJACENT, which is what lets the backup
// carry each object once by comparing a reference with the one before it and
// holding nothing else.
func TestEqualKeysFromTwoWalksComeOutAdjacent(t *testing.T) {
	t.Parallel()
	all := keys(30)
	a, b := &pagedWalk{page: 4}, &pagedWalk{page: 6}
	for i, k := range all {
		switch i % 3 {
		case 0:
			a.refs = append(a.refs, ref(k, "a"))
		case 1:
			b.refs = append(b.refs, ref(k, "b"))
		default:
			a.refs = append(a.refs, ref(k, "a"))
			b.refs = append(b.refs, ref(k, "b"))
		}
	}
	got := drain(t, objstore.MergeWalks(objstore.NamedWalk{Name: "a", Walk: a},
		objstore.NamedWalk{Name: "b", Walk: b}))
	seen := map[objstore.Key]int{}
	for i, r := range got {
		if prev, again := seen[r.Object.Key]; again && prev != i-1 {
			t.Fatalf("%s came out at %d and again at %d, not adjacent", r.Object.Key, prev, i)
		}
		seen[r.Object.Key] = i
	}
	if len(got) != len(all)+len(all)/3 {
		t.Fatalf("the merge handed on %d references, want %d", len(got), len(all)+len(all)/3)
	}
}

// walkedTable is a table a walk test declares and creates in a node store.
var walkedTable = objstore.ReferenceTable{Domain: "files", Table: "walked_objects",
	Key: "object", Hash: "hash", Size: "size", Owner: []string{"project", "path"},
	Standing: objstore.Required}

// A READ RUN AGAIN HANDS EACH ROW ON ONCE. store.DB.Read runs its function
// again when the store answers busy, and a page appended to across two runs
// carried the first run's rows twice — a duplicate handed to every reader of
// the walk, and a page longer than a page that never read as the last one.
func TestAWalkWhoseReadRunsTwiceHandsEachRowOnce(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	all := keys(objstore.WalkPage + 3)
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE walked_objects (object TEXT, hash TEXT NOT NULL,
				size INTEGER NOT NULL, project TEXT NOT NULL, path TEXT NOT NULL)`,
			`CREATE INDEX walked_objects_object_idx ON walked_objects (object)`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return err
			}
		}
		for i, k := range all {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO walked_objects
				(object, hash, size, project, path) VALUES (?, ?, ?, ?, ?)`,
				k.String(), string(objstore.HashOf([]byte(k.String()))), i, "ENG", fmt.Sprintf("%04d.md", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	twice := func(ctx context.Context, fn func(*sql.Tx) error) (bool, error) {
		return true, db.Read(ctx, func(tx *sql.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			return fn(tx)
		})
	}
	w := walkedTable.WalkOf(twice)
	var got []objstore.Reference
	pages := 0
	for {
		page, complete, err := w.Next(t.Context())
		if err != nil || !complete {
			t.Fatalf("Next = %v (complete %v)", err, complete)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > objstore.WalkPage {
			t.Fatalf("a page of %d references, past the walk's page of %d", len(page), objstore.WalkPage)
		}
		pages++
		got = append(got, page...)
	}
	if len(got) != len(all) || pages != 2 {
		t.Fatalf("the walk handed on %d references in %d pages; want %d in 2", len(got), pages, len(all))
	}
	for i, r := range got {
		want := fmt.Sprintf("ENG/%04d.md", i)
		if r.Object.Key != all[i] || r.NamedBy != want || r.Table != walkedTable.Table {
			t.Fatalf("reference %d = %+v; want %s named by %s in %s", i, r, all[i], want, walkedTable.Table)
		}
	}
}
