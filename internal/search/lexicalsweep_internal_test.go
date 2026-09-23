package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// A SWEEP THAT INDEXED SOMETHING TAKES ONE ORPHAN STEP, NOT A LAP.
//
// Such a sweep reports work whatever the orphan walk finds, and its caller comes
// straight back, so a full orphan lap there buys no promise the next sweep
// would not keep — and during a first build it would be a lap of the whole
// index per batch of twenty. Only the sweep about to report nothing has to
// have looked at every row.
func TestABusySweepTakesOneOrphanStep(t *testing.T) {
	t.Parallel()
	db := openInternalStore(t)
	// More index rows than one step reads, so a lap is several steps.
	var docs []capDoc
	for i := range ScanBatch + ScanBatch/2 {
		docs = append(docs, capDoc{id: fmt.Sprintf("p%05d", i), length: 10,
			terms: map[string]int{"word": 1}})
	}
	writeCapCorpus(t, db, docs)
	src := &countedSource{stale: Doc{Source: string(SourcePage), ID: "zzz",
		Container: "ENG", Title: "New", Body: "a page saved just now", Version: 1}}
	x := NewIndexerOver(db, []LexicalSource{src})

	worked, err := x.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !worked {
		t.Fatal("a sweep that had a page to index reported nothing done")
	}
	if src.checks != 1 {
		t.Fatalf("a sweep that indexed a page ran %d orphan checks over an index "+
			"a lap of which is two — it lapped where one step was all its "+
			"answer needed", src.checks)
	}

	// AND A QUIET SWEEP DOES LAP, which is the control: without it the
	// count above would pass on a walk that never lapped at all. The first
	// quiet sweep finishes the lap the busy one started; the second starts
	// one from the beginning and has to see it through.
	for range 2 {
		src.checks = 0
		if worked, err := x.Sweep(t.Context()); err != nil || worked {
			t.Fatalf("a sweep with nothing to do found work (%v) or failed: %v",
				worked, err)
		}
	}
	if src.checks != 2 {
		t.Fatalf("a quiet sweep from the start of the index ran %d orphan "+
			"check(s) over an index a lap of which is two", src.checks)
	}
}

// EACH CORPUS IS ANNOUNCED BUILT ON ITS OWN FIRST LAP, AND ONCE.
//
// `lexical_index_built` marks the moment a search stops saying "still
// building", and a knowledge search waits on the pages' lap alone while a work
// search waits on the work items'. So the line is per corpus: announced only
// when the whole index had lapped, it would name a moment neither search
// observes, and stay silent while one corpus served and the other built.
func TestEachCorpusIsAnnouncedBuiltOnItsOwnLap(t *testing.T) {
	t.Parallel()
	db := openInternalStore(t)
	x := NewIndexerOver(db, []LexicalSource{PageSource{}, stuckSource{}})
	announced := map[string]bool{}

	if got := x.newlyBuilt(announced); len(got) != 0 {
		t.Fatalf("before any lap %v was announced built", got)
	}
	// The pages come first, so their lap finishes before the sweep reaches
	// the corpus that never does — which then fails every sweep.
	for sweeps := 0; !x.ReadyFor(string(SourcePage)); sweeps++ {
		if sweeps == 100 {
			t.Fatal("the page corpus never finished its first lap")
		}
		if _, err := x.Sweep(t.Context()); err != nil && !errors.Is(err, errStuck) {
			t.Fatalf("sweep: %v", err)
		}
	}
	if got := x.newlyBuilt(announced); !slices.Equal(got, []string{string(SourcePage)}) {
		t.Errorf("with the pages built and the other corpus not, %v was announced, "+
			"want the pages alone", got)
	}
	if got := x.newlyBuilt(announced); len(got) != 0 {
		t.Errorf("the pages were announced a second time: %v", got)
	}
	// AND ITS PENDING COUNT IS ITS OWN, read without the corpus that cannot
	// be read at all — the line would otherwise carry an error for a corpus
	// it is not about.
	if n, err := x.Pending(t.Context(), string(SourcePage)); err != nil || n != 0 {
		t.Errorf("the built pages report %d pending (%v), want 0", n, err)
	}
	if _, err := x.Pending(t.Context()); !errors.Is(err, errStuck) {
		t.Errorf("the whole index's pending count read past a corpus it cannot "+
			"count: %v", err)
	}
}

// ONE CORPUS'S ORPHANS DO NOT HIDE ANOTHER CORPUS'S MISSING DOCUMENTS.
//
// Pending is two counts and a subtraction, and an index row whose source is
// gone offsets a document with no row. Within one corpus that is the price of
// not joining across estates; across corpora it is a wrong answer, so the
// subtraction is clamped per source before anything is summed.
func TestPendingIsClampedPerCorpus(t *testing.T) {
	t.Parallel()
	db := openInternalStore(t)
	// A PAGE ROW WITH NO PAGE BEHIND IT: an orphan the next sweep removes.
	writeCapCorpus(t, db, []capDoc{{id: "p.gone", length: 3,
		terms: map[string]int{"word": 1}}})
	// AND A WORK ITEM THE INDEX HAS NO ROW FOR.
	if _, err := db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO tracker_tasks (id, key, project_key, root_id, type, title,
		                           status, status_group, rank, document,
		                           version, created_at, updated_at)
		VALUES ('t.1', 'ENG-1', 'ENG', 't.1', 'task', 'Rotate the key',
		        'todo', 'not_started', 'a0', json_object('body', ''), 1, 0, 0)`); err != nil {
		t.Fatalf("insert a work item: %v", err)
	}
	x := NewIndexerOver(db, []LexicalSource{PageSource{}, TaskSource{}})

	for _, tc := range []struct {
		sources []string
		want    int
	}{
		{nil, 1},
		{[]string{string(SourceTask)}, 1},
		{[]string{string(SourcePage)}, 0},
		{[]string{"a-corpus-this-build-has-never-heard-of"}, 0},
	} {
		n, err := x.Pending(t.Context(), tc.sources...)
		if err != nil {
			t.Fatalf("Pending(%v): %v", tc.sources, err)
		}
		if n != tc.want {
			t.Errorf("Pending(%v) = %d, want %d", tc.sources, n, tc.want)
		}
	}
}

// errStuck is what [stuckSource] answers every read with.
var errStuck = errors.New("this corpus cannot be read")

// stuckSource is a corpus this node cannot read, so its first lap never
// finishes.
type stuckSource struct{}

func (stuckSource) Source() string { return string(SourceTask) }

func (stuckSource) Versions(context.Context, *sql.Tx, string, int) ([]DocVersion, error) {
	return nil, errStuck
}

func (stuckSource) Fetch(context.Context, *sql.Tx, []string) ([]Doc, error) {
	return nil, errStuck
}

func (stuckSource) Live(context.Context, *sql.Tx, []string) (map[string]bool, error) {
	return nil, errStuck
}

func (stuckSource) Count(context.Context, *sql.Tx) (int, error) { return 0, errStuck }

// countedSource offers one page to index, calls every indexed id live, and
// counts the existence checks it is asked.
type countedSource struct {
	stale  Doc
	checks int
}

func (*countedSource) Source() string { return string(SourcePage) }

func (c *countedSource) Versions(_ context.Context, _ *sql.Tx, after string,
	_ int) ([]DocVersion, error) {

	if after >= c.stale.ID {
		return nil, nil
	}
	return []DocVersion{{ID: c.stale.ID, Version: c.stale.Version}}, nil
}

func (c *countedSource) Fetch(context.Context, *sql.Tx, []string) ([]Doc, error) {
	return []Doc{c.stale}, nil
}

func (c *countedSource) Live(_ context.Context, _ *sql.Tx, ids []string) (map[string]bool, error) {
	c.checks++
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (*countedSource) Count(context.Context, *sql.Tx) (int, error) { return 1, nil }
