package eventfan

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE MERGE IS KEYED ON THE STORE'S OWN IDENTITY, (event_time, event_id).
//
// Every merge in this package stitches rows read twice — a turn's oldest rows
// forward and its newest back, a page and its trace siblings, several nodes'
// shares — and the join has to agree with the table about what "the same row"
// is. The events table's PRIMARY KEY is the pair, and its schema says the id
// alone is not unique in as many words; [store.EventLog.ByID] reads it as
// "take the newest match" for that reason.
//
// Keyed on the id alone, two distinct rows sharing one id collapse to one: the
// reader loses a row outright, and because `truncated` is `total > len(rows)`
// the page then reports a gap in the middle of a turn it is holding whole.
func TestTheMergeKeepsTwoRowsThatShareAnId(t *testing.T) {
	t.Parallel()
	at := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
		return parsed
	}
	// One id, two times — which the primary key permits and a redelivered
	// wake with a derived id actually writes.
	head := []store.EventRecord{
		{ID: "ev-1", Time: at("2026-09-13T10:00:00Z")},
		{ID: "ev-2", Time: at("2026-09-13T10:00:01Z")},
	}
	tail := []store.EventRecord{
		{ID: "ev-1", Time: at("2026-09-13T10:05:00Z")}, // same id, later row
		{ID: "ev-9", Time: at("2026-09-13T10:05:01Z")},
	}

	got := union(head, tail)
	if len(got) != 4 {
		t.Fatalf("merged to %d rows, want 4 — a row sharing an id with another "+
			"was dropped, which is a row the reader loses and a gap the page invents", len(got))
	}

	// The genuine duplicate — the SAME row read by both halves — still
	// collapses, which is the whole reason the merge exists.
	same := union(head, []store.EventRecord{
		{ID: "ev-2", Time: at("2026-09-13T10:00:01Z")},
	})
	if len(same) != 2 {
		t.Errorf("a row present in both reads merged to %d rows, want 2", len(same))
	}
}

// An empty tail is the ordinary case and must not disturb the head.
func TestTheMergeLeavesTheHeadAloneWithNothingToAdd(t *testing.T) {
	t.Parallel()
	head := []store.EventRecord{{ID: "ev-1", Time: time.Unix(0, 0).UTC()}}
	if got := union(head, nil); len(got) != 1 || got[0].ID != "ev-1" {
		t.Errorf("merge with no tail = %v, want the head unchanged", got)
	}
}
