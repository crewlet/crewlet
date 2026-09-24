package pages_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A REANCHOR'S RECORD ON THIS LOG IS CLAIMED ONCE, AND ITS APPLY IS THE AUDIT
// ROW.
//
// The knowledge base's reanchor writes no audit row of its own: the record's
// apply writes it on every node that applies the new stream, so the record has
// to land once per generation. The first node to publish holds the generation;
// the same node re-running a transition that crashed after its publish — with
// its applier halted, so not past its own record — finds that record and
// appends nothing; and a second node deriving the same number is refused,
// naming the holder, before a checkpoint of its own could move.
//
// Mutation: publish rather than claim, and the re-run is refused `behind` — it
// cannot tell the record on the subject from a rival's.
func TestAGenerationRecordIsClaimedOnceAndAppliedAsTheAuditRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	actor := pages.Actor{Handle: "ops", Kind: pages.AuthorOperator, OperatorID: "ops"}
	live := time.Date(2031, 4, 2, 3, 0, 0, 123456789, time.UTC)
	in := statelog.ReanchorInputs{StreamCreatedAt: live, Highest: 412}

	if err := r.store.PublishGeneration(t.Context(), actor, "node-a", 1, in); err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if err := r.store.PublishGeneration(t.Context(), actor, "node-a", 1, in); err != nil {
		t.Fatalf("the same node's re-run: %v — its own record on the subject is "+
			"its claim, whether or not this node has applied it", err)
	}
	holder := statelog.GenerationOpID(1, live, "node-a")
	var elsewhere *statelog.ClaimedElsewhere
	if err := r.store.PublishGeneration(t.Context(), actor, "node-b", 1, in); !errors.As(err, &elsewhere) ||
		elsewhere.Holder != holder {
		t.Fatalf("a second node's claim = %v, want it refused naming %q", err, holder)
	}
	if got, err := r.log.End(t.Context()); err != nil || got != end {
		t.Errorf("the log ends at %d (%v) after the re-run and the rival, want %d",
			got, err, end)
	}

	r.drain()
	var by, record string
	var created, prev int64
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT by, new_stream_created_at, prev_last_seq_seen, record_id
			FROM pages_log_generations WHERE generation = 1`).Scan(
			&by, &created, &prev, &record)
	}); err != nil {
		t.Fatalf("read generation 1's audit row: %v", err)
	}
	switch {
	case by != actor.Name():
		t.Errorf("the audit row names %q as who re-anchored, want %q", by, actor.Name())
	case !store.DecodeTime(created).Equal(live.Truncate(time.Microsecond)):
		t.Errorf("the audit row names the new stream as created at %s, want %s",
			store.DecodeTime(created), live)
	case prev != 412:
		t.Errorf("the audit row records %d as the old stream's head, want 412", prev)
	case record != holder:
		t.Errorf("the audit row names record %q, want %q", record, holder)
	}
}
