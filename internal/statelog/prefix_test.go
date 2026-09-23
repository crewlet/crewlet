package statelog_test

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHECKPOINT PASSES A RETAINED RECORD, AND THE APPLIED PREFIX DOES NOT.
//
// A node that retains a record — a version this build cannot read — moves its
// checkpoint past it and goes on applying what the retained record does not
// cover. Asked "have you applied everything up to here?" it must say no: a
// reader deciding from an absent row (nobody owns this key, this session has
// ended) would otherwise read the retained record's rows as rows no record
// wrote. So the prefix a snapshot reports is settled at the checkpoint and
// APPLIED only below the earliest record retained.
//
// And coverage is a question about EVERY retained record's scope, not the
// earliest one's: the earliest here is about `object/b`, and a later one about
// `object/d` must still be found by a probe over `object/d`.
func TestAPrefixIsAppliedOnlyBelowTheEarliestRetainedRecord(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 9)) // above this build
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))
	h.fetch.offer(4, env(4, "edit", "b", "op-4", 1)) // on the rows 2 left stale
	h.fetch.offer(5, env(5, "edit", "d", "op-5", 9)) // above this build again
	if err := h.run(5); err != nil {
		t.Fatalf("run: %v", err)
	}

	var (
		prefix               statelog.Prefix
		onB, onC, onD, onAll bool
		atB, atD             statelog.Deferral
		emptyErr             error
	)
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		if prefix, err = statelog.PrefixIn(t.Context(), tx, probeDomain{}); err != nil {
			return err
		}
		probe := func(path string) (statelog.Deferral, bool) {
			d, hit, err := statelog.DeferredIn(t.Context(), tx, probeDomain{},
				statelog.ScopeSet{Paths: []string{path}})
			if err != nil {
				t.Fatalf("DeferredIn(%s): %v", path, err)
			}
			return d, hit
		}
		atB, onB = probe("object/b")
		_, onC = probe("object/c")
		atD, onD = probe("object/d")
		_, onAll = probe("object")
		_, _, emptyErr = statelog.DeferredIn(t.Context(), tx, probeDomain{},
			statelog.ScopeSet{})
		return nil
	}); err != nil {
		t.Fatalf("read the prefix: %v", err)
	}

	if prefix.Settled.Seq != 5 {
		t.Fatalf("the settled checkpoint is %d, want 5 — the runner consumed "+
			"every record", prefix.Settled.Seq)
	}
	if !prefix.Retains || prefix.Retained.Position.Seq != 2 {
		t.Fatalf("the prefix reports retained %+v (retains %v), want the "+
			"earliest retained record at 2", prefix.Retained, prefix.Retains)
	}
	if got := prefix.Applied().Seq; got != 1 {
		t.Fatalf("the applied prefix is %d, want 1 — everything from the "+
			"earliest retained record up is settled and not applied", got)
	}
	if !slices.Contains(prefix.Retained.Scope.Paths, "object/b") {
		t.Errorf("the retained record reports scope %v, want its own declared "+
			"one — a Deferral with no scope says it is about nothing",
			prefix.Retained.Scope.Paths)
	}
	if held, ok := h.runner.Deferred(); !ok || !slices.Contains(held.Scope.Paths, "object/b") {
		t.Errorf("the runner's deferral carries scope %v (held %v), want "+
			"object/b — every Deferral promises its declared scope", held.Scope.Paths, ok)
	}

	if !onB || atB.Position.Seq != 2 {
		t.Errorf("a probe over object/b = (%+v, %v), want the record at 2", atB, onB)
	}
	if onC {
		t.Error("a probe over object/c found a retained record, and none is " +
			"about it — every read of c would refuse for nothing")
	}
	if !onD || atD.Position.Seq != 5 {
		t.Errorf("a probe over object/d = (%+v, %v), want the LATER retained "+
			"record at 5 — a probe that asked only the earliest record reads "+
			"it as covering nothing about d", atD, onD)
	}
	if !onAll {
		t.Error("a probe over the container found nothing beneath it")
	}
	if emptyErr == nil {
		t.Error("a probe naming no objects answered \"not covered\", which " +
			"is the one answer nothing can give it")
	}
}

// NOTHING RETAINED, AND THE TWO PREFIXES ARE ONE.
func TestAPrefixWithNothingRetainedIsAppliedAtItsCheckpoint(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	var prefix statelog.Prefix
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		prefix, err = statelog.PrefixIn(t.Context(), tx, probeDomain{})
		return err
	}); err != nil {
		t.Fatalf("read the prefix: %v", err)
	}
	if prefix.Retains || prefix.Applied() != prefix.Settled || prefix.Settled.Seq != 2 {
		t.Fatalf("with nothing retained the prefix is %+v (applied %v), want "+
			"applied at the checkpoint 2", prefix, prefix.Applied())
	}
}
