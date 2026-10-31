package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// codeEvent is a code-host event a seat's day counts, as the event store's
// writer files it: its actor, its tags — the agent id among them — and its
// payload.
type codeEvent interface {
	events.Payload
	Actor() string
}

// codeRecord is ev as node records it at at, under id.
func codeRecord(t *testing.T, id string, at time.Time, ev codeEvent) store.EventRecord {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: ev.EventType(), Actor: ev.Actor(), Time: at, Category: "system",
		Tags: store.ExtractTags(payload), Payload: payload}
}

// openedBy is change opened by owner, whose agent id is agent ("" for a
// person).
func openedBy(change, owner, kind, agent string) types.ChangeOpened {
	return types.ChangeOpened{RepoID: "repo-web", RepoName: "web", ChangeID: change, Number: 1,
		Target: "refs/heads/main", Title: "Retry the upload", Owner: owner, OwnerKind: kind, Agent: agent}
}

// reviewOf is review given by reviewer (agent id reviewerAgent) of a change
// owned by owner (ownerAgent), raising findings.
func reviewOf(review, reviewer, reviewerAgent, owner, ownerAgent string, findings int64) types.ReviewSubmitted {
	kind := func(agent string) string {
		if agent == "" {
			return "person"
		}
		return "agent"
	}
	return types.ReviewSubmitted{RepoID: "repo-web", RepoName: "web", ChangeID: "change-" + owner, Number: 1,
		ReviewID: review, Revision: 1, Reviewer: reviewer, ReviewerKind: kind(reviewerAgent), Agent: reviewerAgent,
		Verdict: "request_changes", Findings: findings, Owner: owner, OwnerKind: kind(ownerAgent),
		OwnerAgent: ownerAgent}
}

// codeOf is every seat's code counts a derived day holds, by agent id.
func codeOf(t *testing.T, db *store.DB, w store.UsageWindow) map[string]store.UsageCode {
	t.Helper()
	day, err := db.UsageForDay(t.Context(), w)
	if err != nil {
		t.Fatalf("derive the day: %v", err)
	}
	out := map[string]store.UsageCode{}
	for _, s := range day.Seats {
		if s.Code != (store.UsageCode{}) {
			out[s.AgentID] = s.Code
		}
	}
	return out
}

// A SEAT'S DAY COUNTS WHAT IT OPENED AND REVIEWED: dev's two openings and qa's
// two reviews, each counted once — an opening or a review announced twice is
// still one, since the day counts the change and the review rather than their
// records — on the seat the announcement names by its agent id; a person's
// opening counts on no seat, and an opening an hour outside the window on
// another day. And an opening landing moves the day's fingerprint, which is
// what makes the publisher derive the day again.
//
// Mutation: count the records rather than the changes (drop the inner GROUP BY
// item) — dev opened three, red; leave the event types out of the fingerprint
// — the mark does not move, red.
func TestASeatsDayCountsWhatItOpenedAndReviewed(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }

	before, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	for i, rec := range []store.EventRecord{
		codeRecord(t, "o-1", at(1), openedBy("change-1", "dev", "agent", "a-dev")),
		codeRecord(t, "o-2", at(2), openedBy("change-2", "dev", "agent", "a-dev")),
		// THE SAME OPENING ANNOUNCED TWICE is one change.
		codeRecord(t, "o-3", at(3), openedBy("change-2", "dev", "agent", "a-dev")),
		codeRecord(t, "o-4", at(4), openedBy("change-3", "founder", "person", "")),
		codeRecord(t, "o-5", day.Add(-time.Hour), openedBy("change-0", "dev", "agent", "a-dev")),
		codeRecord(t, "r-1", at(5), reviewOf("review-1", "qa", "a-qa", "dev", "a-dev", 0)),
		codeRecord(t, "r-2", at(6), reviewOf("review-2", "qa", "a-qa", "dev", "a-dev", 0)),
		codeRecord(t, "r-3", at(7), reviewOf("review-2", "qa", "a-qa", "dev", "a-dev", 0)),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	after, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Errorf("the day's fingerprint did not move when changes opened and reviews landed: %+v", after)
	}
	want := map[string]store.UsageCode{
		"a-dev": {ChangesOpened: 2},
		"a-qa":  {ReviewsGiven: 2},
	}
	if got := codeOf(t, db, w); len(got) != len(want) || got["a-dev"] != want["a-dev"] || got["a-qa"] != want["a-qa"] {
		t.Fatalf("the day counts %+v, want %+v: each opening and review once, on the seat that made it", got, want)
	}
}

// FINDINGS ARE COUNTED ON THE OWNER: a review's findings land on the seat that
// owns the change it judged — never on the reviewer, whose day counts the
// review — whoever reviewed, a person included; a review of a person's change
// counts on its reviewer and on no owner; and a review that raised none adds
// no findings to anybody.
//
// Mutation: count a review's findings on its reviewer (the third branch over
// agent_id) — qa received findings it raised, red; count them per record
// rather than per review — dev received eleven, red.
func TestFindingsAreCountedOnTheOwner(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	for i, rec := range []store.EventRecord{
		codeRecord(t, "r-1", at(1), reviewOf("review-1", "qa", "a-qa", "dev", "a-dev", 4)),
		// THE SAME REVIEW ANNOUNCED TWICE raised its findings once.
		codeRecord(t, "r-2", at(2), reviewOf("review-1", "qa", "a-qa", "dev", "a-dev", 4)),
		codeRecord(t, "r-3", at(3), reviewOf("review-2", "founder", "", "dev", "a-dev", 3)),
		codeRecord(t, "r-4", at(4), reviewOf("review-3", "qa", "a-qa", "founder", "", 5)),
		codeRecord(t, "r-5", at(5), reviewOf("review-4", "dev", "a-dev", "qa", "a-qa", 0)),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	want := map[string]store.UsageCode{
		"a-dev": {ReviewsGiven: 1, FindingsReceived: 7},
		"a-qa":  {ReviewsGiven: 2},
	}
	if got := codeOf(t, db, w); len(got) != len(want) || got["a-dev"] != want["a-dev"] || got["a-qa"] != want["a-qa"] {
		t.Fatalf("the day counts %+v, want %+v: every finding on the change's owner, a person's review's "+
			"included, and none on a reviewer or a person", got, want)
	}
}
