package search

import (
	"context"
	"database/sql"
	"fmt"
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
