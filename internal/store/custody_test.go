package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// custodyBatch is a batch of two of a stateless node's events, each naming a
// party so a release can be seen to take the party index with it.
func custodyBatch(id string, at time.Time) store.CustodyBatch {
	rec := func(n string) store.EventRecord {
		return store.EventRecord{
			ID: id + "-" + n, Type: "agent_phase_completed", Source: "engine",
			Category: "agent", Time: at, Actor: "swe-on-stateless",
			Payload: []byte(`{}`),
		}
	}
	return store.CustodyBatch{ID: id, Origin: "seats-1", Records: []store.EventRecord{rec("a"), rec("b")}}
}

func holds(t *testing.T, log *store.EventLog, id string) bool {
	t.Helper()
	_, err := log.ByID(t.Context(), id)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		return false
	}
	t.Fatalf("read %s: %v", id, err)
	return false
}

func related(t *testing.T, log *store.EventLog) int {
	t.Helper()
	rows, err := log.List(t.Context(), store.ListQuery{RelatedAgent: "swe-on-stateless", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(rows)
}

func unsettled(t *testing.T, log *store.EventLog, before time.Time) []string {
	t.Helper()
	got, err := log.UnsettledCustody(t.Context(), before, 10)
	if err != nil {
		t.Fatalf("unsettled: %v", err)
	}
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	return ids
}

// A BATCH IS WRITTEN WITH THE QUESTION IT RAISES, and settled either way in one
// step: kept, its rows stay and the question goes; not kept, its rows go with
// it — party index included, or a filter by party would find rows the log no
// longer holds.
func TestACustodyBatchIsWrittenUnsettledAndSettledEitherWay(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Add(-time.Minute)
	for _, kept := range []bool{true, false} {
		log := open(t).Events()
		b := custodyBatch("batch-1", at)
		if err := log.WriteCustody(t.Context(), b, at); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !holds(t, log, "batch-1-a") || !holds(t, log, "batch-1-b") {
			t.Fatal("a written batch's rows are not in the log")
		}
		if got := unsettled(t, log, time.Now()); len(got) != 1 || got[0] != "batch-1" {
			t.Fatalf("unsettled = %v, want the batch just written", got)
		}

		if err := log.SettleCustody(t.Context(), "batch-1", kept); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if got := unsettled(t, log, time.Now()); len(got) != 0 {
			t.Errorf("kept=%v: the batch is still unsettled: %v", kept, got)
		}
		if got := holds(t, log, "batch-1-a") && holds(t, log, "batch-1-b"); got != kept {
			t.Errorf("kept=%v: the batch's rows present = %v", kept, got)
		}
		if n := related(t, log); (n == 2) != kept || (!kept && n != 0) {
			t.Errorf("kept=%v: %d row(s) found by party, want %d", kept, n,
				map[bool]int{true: 2, false: 0}[kept])
		}
	}
}

// WRITTEN TWICE IS WRITTEN ONCE: the group delivers a batch again whose
// acknowledgement was lost, and to the same node too. Its rows are not
// doubled, and the question keeps the instant the batch was first written.
func TestACustodyBatchWrittenAgainIsOneBatch(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	first := time.Now().UTC().Add(-time.Hour)
	b := custodyBatch("batch-2", first)
	for _, at := range []time.Time{first, first.Add(30 * time.Minute)} {
		if err := log.WriteCustody(t.Context(), b, at); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if n := related(t, log); n != 2 {
		t.Errorf("a batch written twice holds %d rows, want 2", n)
	}
	got, err := log.UnsettledCustody(t.Context(), time.Now(), 10)
	if err != nil || len(got) != 1 || !got[0].WrittenAt.Equal(first.Truncate(time.Microsecond)) {
		t.Fatalf("unsettled = (%+v, %v), want one batch first written at %v", got, err, first)
	}
}

// ONLY WHAT WAS WRITTEN BEFORE THE INSTANT ASKED: a batch still on its way to
// its claim is not somebody else's to settle.
func TestUnsettledCustodyListsOnlyWhatIsOlderThanAsked(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	for _, c := range []struct {
		id string
		at time.Time
	}{{"old", now.Add(-time.Hour)}, {"new", now}} {
		if err := log.WriteCustody(t.Context(), custodyBatch(c.id, c.at), c.at); err != nil {
			t.Fatalf("write %s: %v", c.id, err)
		}
	}
	if got := unsettled(t, log, now.Add(-time.Minute)); len(got) != 1 || got[0] != "old" {
		t.Fatalf("unsettled before a minute ago = %v, want [old]", got)
	}
}

// A BATCH THAT CANNOT BE STORED WHOLE IS NOT STORED AT ALL: written in part and
// recorded whole, a release would leave the rest behind.
func TestACustodyBatchWithAnUnstorableRecordIsRefusedWhole(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	b := custodyBatch("batch-3", time.Now().UTC())
	b.Records[1].ID = ""
	if err := log.WriteCustody(t.Context(), b, time.Now()); !errors.Is(err, store.ErrIncompleteRecord) {
		t.Fatalf("write = %v, want ErrIncompleteRecord", err)
	}
	if holds(t, log, "batch-3-a") || len(unsettled(t, log, time.Now().Add(time.Hour))) != 0 {
		t.Error("part of a refused batch was written")
	}
}

// SETTLING A BATCH THIS NODE NEVER WROTE is nothing — it was settled already, or
// another node wrote it — and never a deletion of somebody's rows.
func TestSettlingABatchNeverWrittenHereDoesNothing(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	if err := log.WriteCustody(t.Context(), custodyBatch("mine", time.Now().UTC()), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := log.SettleCustody(t.Context(), "not-mine", false); err != nil {
		t.Fatalf("settle an unknown batch: %v", err)
	}
	if !holds(t, log, "mine-a") {
		t.Error("settling an unknown batch deleted another batch's rows")
	}
}
