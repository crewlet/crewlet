package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// gateRecord is ev — a record of the merge gate's, which names no actor of its
// own — as node records it at at, under id.
func gateRecord(t *testing.T, id string, at time.Time, ev events.Payload) store.EventRecord {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: ev.EventType(), Time: at, Category: "system",
		Tags: store.ExtractTags(payload), Payload: payload}
}

// failedCheck is attempt of check name at oid of a change owned by owner
// (ownerAgent, "" for a person), ended in state — named owner as its writer
// names one only on an ending that fails the head.
func failedCheck(name string, attempt int, oid, state, owner, ownerAgent string) types.CodeCheckFinished {
	c := types.CodeCheckFinished{RepoID: "repo-web", RepoName: "web", OID: oid, Name: name, Attempt: attempt,
		Purpose: "head", ChangeID: "change-" + owner, Target: "refs/heads/main", State: state, Reason: "exit",
		Placement: "container", Required: true}
	if state == "failed" {
		c.Owner, c.OwnerAgent = owner, ownerAgent
		c.OwnerKind = "agent"
		if ownerAgent == "" {
			c.OwnerKind = "person"
		}
	}
	return c
}

// erroredPastRequeue is the last attempt the home's requeue runs of check name
// at oid of a change owned by owner (ownerAgent), errored runner_lost — an
// ending that fails the head, so its writer names the owner.
func erroredPastRequeue(name, oid, owner, ownerAgent string) types.CodeCheckFinished {
	c := failedCheck(name, 3, oid, "errored", owner, ownerAgent)
	c.Reason, c.Owner, c.OwnerKind, c.OwnerAgent = "runner_lost", owner, "agent", ownerAgent
	return c
}

// correctedToPass is [erroredPastRequeue]'s attempt ended passed by its slow
// runner after all: a correction naming the ending it replaced, and no owner,
// since it fails no head.
func correctedToPass(name, oid string) types.CodeCheckFinished {
	c := failedCheck(name, 3, oid, "passed", "", "")
	c.Replaces = "errored"
	return c
}

// ejected is entry of a change owned by owner (ownerAgent), queued by
// enqueuer, put out of main's queue.
func ejected(entry, owner, ownerAgent, enqueuer string) types.CodeQueueEjected {
	kind := "agent"
	if ownerAgent == "" {
		kind = "person"
	}
	return types.CodeQueueEjected{RepoID: "repo-web", RepoName: "web", EntryID: entry, ChangeID: "change-" + owner,
		Number: 1, Target: "refs/heads/main", Reason: "check_failed", Checks: []string{"unit"},
		EnqueuedBy: enqueuer, EnqueuedByKind: "agent", Owner: owner, OwnerKind: kind, OwnerAgent: ownerAgent}
}

// A FAILED CHECK COUNTS ON ITS OWNER'S DAY: three checks that failed the heads
// of dev's changes count three on dev's day — once each by their attempt, so
// an ending announced twice is still one, and an attempt errored past the
// home's requeue that a slow runner's exit then corrects to a pass STAYS
// counted, since a day's figures only grow (the CodeCheckFinished type's doc)
// — and a passing check and a failure of a person's change count on nobody; a
// failure outside the window counts on another day. A failed check landing
// moves the day's fingerprint, which is what makes the publisher derive the
// day again.
//
// Mutation: count the records rather than the attempts (key the item by the
// event's id) — dev failed four, red; leave code_check_finished out of
// usageEventTypes — the fingerprint does not move, red.
func TestAFailedCheckCountsOnItsOwnersDay(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	before, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatalf("fingerprint the empty day: %v", err)
	}
	for i, rec := range []store.EventRecord{
		gateRecord(t, "c-1", at(1), failedCheck("unit", 1, "oid-b", "failed", "dev", "a-dev")),
		// THE SAME ATTEMPT ANNOUNCED AGAIN failed once.
		gateRecord(t, "c-2", at(2), failedCheck("unit", 1, "oid-b", "failed", "dev", "a-dev")),
		gateRecord(t, "c-3", at(3), failedCheck("lint", 1, "oid-b", "failed", "dev", "a-dev")),
		gateRecord(t, "c-4", at(4), failedCheck("unit", 2, "oid-b", "passed", "dev", "a-dev")),
		gateRecord(t, "c-5", at(5), failedCheck("unit", 1, "oid-c", "failed", "founder", "")),
		gateRecord(t, "c-6", at(26), failedCheck("e2e", 1, "oid-b", "failed", "dev", "a-dev")),
		// AN ATTEMPT ERRORED PAST THE REQUEUE, its owner named, then corrected
		// to a pass naming none: counted once, and kept.
		gateRecord(t, "c-7", at(6), erroredPastRequeue("e2e", "oid-b", "dev", "a-dev")),
		gateRecord(t, "c-8", at(7), correctedToPass("e2e", "oid-b")),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if got := codeOf(t, db, w); len(got) != 1 || got["a-dev"] != (store.UsageCode{ChecksFailed: 3}) {
		t.Fatalf("the day counts %+v, want three failed checks on dev's day alone: each attempt once, a "+
			"corrected one kept, none for a pass, a person's change or another day", got)
	}
	after, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatalf("fingerprint the day: %v", err)
	}
	if after == before {
		t.Fatal("failed checks landed and the day's fingerprint did not move: the publisher would never " +
			"derive it again")
	}
}

// AN EJECTION COUNTS ON ITS OWNER'S DAY: dev's change put out of its queue
// twice counts two ejections on dev's day — never on qa's, who queued it —
// each once by its entry, so an ejection announced twice is still one; an
// ejection of a person's change counts on nobody.
//
// Mutation: count an ejection on its enqueuer (read the enqueuer's seat) — qa
// is counted, red; count the records rather than the entries — dev was
// ejected three times, red.
func TestAnEjectionCountsOnItsOwnersDay(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	for i, rec := range []store.EventRecord{
		gateRecord(t, "e-1", at(1), ejected("entry-1", "dev", "a-dev", "qa")),
		gateRecord(t, "e-2", at(2), ejected("entry-1", "dev", "a-dev", "qa")),
		gateRecord(t, "e-3", at(3), ejected("entry-2", "dev", "a-dev", "qa")),
		gateRecord(t, "e-4", at(4), ejected("entry-3", "founder", "", "qa")),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if got := codeOf(t, db, w); len(got) != 1 || got["a-dev"] != (store.UsageCode{Ejections: 2}) {
		t.Fatalf("the day counts %+v, want two ejections on dev's day alone: each entry once, none on its "+
			"enqueuer or for a person's change", got)
	}
}
