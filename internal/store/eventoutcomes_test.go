package store_test

import (
	"maps"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// outcome writes one notification outcome row for a third-party app at an
// instant. An empty app writes no tag at all, which is what a row from before
// the tag existed carries.
func outcome(t *testing.T, log *store.EventLog, id, kind, app string, at time.Time) {
	t.Helper()
	tags := map[string]string{}
	if app != "" {
		tags["notification_source"] = app
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: kind, Source: "engine", Category: "notification", Time: at,
		Summary: kind, Tags: tags, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// THE COUNT IS THE WINDOW'S, EDGE FOR EDGE: `[Since, At)`, half-open like
// every window the log is asked about.
//
// The integrations answer names one window for the deliveries and their
// outcomes, and the outcomes used to be the newest page of notification events
// instead — whose span was its own. So the edges are the contract: a row at
// Since is in, a microsecond under it is not, and a row at At — the instant the
// window was named at — is past it, since a count stated beside a window must
// not count what was written after the window was named. Only the two outcome
// types count, each under the app its tag names — trimmed of any whitespace,
// as the tag was always read, so two paddings of one app are one app — and a
// row with no app is not guessed at.
//
// Mutation: drop either edge from [store.OutcomeQuery]'s statement, make the
// top edge inclusive, or count by `category` rather than by type, and a row
// outside the window or of another type is counted.
func TestNotificationOutcomesCountExactlyTheirWindow(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	since := at.Add(-2 * time.Hour)

	outcome(t, log, "at-since", "notification_skipped", "gitlab", since)
	outcome(t, log, "under-since", "notification_skipped", "gitlab", since.Add(-time.Microsecond))
	outcome(t, log, "inside", "notification_skipped", "gitlab", at.Add(-time.Microsecond))
	outcome(t, log, "at-top", "notification_skipped", "gitlab", at)
	outcome(t, log, "merge", "notifications_coalesced", "slack", at.Add(-time.Hour))
	outcome(t, log, "merge-late", "notifications_coalesced", "slack", at.Add(time.Minute))
	outcome(t, log, "padded", "notification_skipped", " jira ", at.Add(-time.Minute))
	outcome(t, log, "tabbed", "notification_skipped", "jira\t", at.Add(-time.Minute))
	outcome(t, log, "untagged", "notification_skipped", "", at.Add(-time.Minute))
	outcome(t, log, "blank", "notifications_coalesced", "   ", at.Add(-time.Minute))
	// The category's bulk, which names an app too and is not an outcome.
	outcome(t, log, "arrived", "external_notification", "gitlab", at.Add(-time.Minute))

	got, err := log.NotificationOutcomes(t.Context(), store.OutcomeQuery{Since: since, At: at})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"gitlab": 2, "jira": 2}; !maps.Equal(got.Skipped, want) {
		t.Errorf("skipped = %v, want %v — the rows at Since and just under At, and jira's "+
			"two, however the tag was padded", got.Skipped, want)
	}
	if want := map[string]int{"slack": 1}; !maps.Equal(got.Coalesced, want) {
		t.Errorf("coalesced = %v, want %v — the merge inside the window and not the one past it",
			got.Coalesced, want)
	}
}

// AND FLOORED AT THE INSTANT IT IS ASKED AT, like every read of the log: an
// edge below the history floor is held to it, and the floor is `At` −
// [store.EventHistory] rather than this node's own clock's — because a fleet
// asks every node at the ASKER's instant, and a node flooring at its own
// would count a different window from its neighbours. The rows here sit a
// second either side of the floor under an instant an hour old: the one above
// it is under the clock's floor and still counted, and the one below it is
// still on disk (retention keeps a day past the floor) and not.
//
// Mutation: floor the count at the clock rather than at `At`, and the row just
// above the pinned floor is lost; drop the floor, and the row under it is
// counted — with Since unset and with Since below the floor alike.
func TestNotificationOutcomesAreFlooredAtTheirInstant(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	floor := at.Add(-store.EventHistory)

	outcome(t, log, "above", "notification_skipped", "gitlab", floor.Add(time.Second))
	outcome(t, log, "below", "notification_skipped", "gitlab", floor.Add(-time.Second))

	for name, q := range map[string]store.OutcomeQuery{
		"no bottom edge":            {At: at},
		"a bottom edge under floor": {Since: floor.Add(-time.Hour), At: at},
	} {
		got, err := log.NotificationOutcomes(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		if got.Skipped["gitlab"] != 1 {
			t.Errorf("%s: gitlab skipped = %d, want 1 — the row above the floor under the "+
				"instant asked at, and not the one below it", name, got.Skipped["gitlab"])
		}
	}
}

// NOTHING TO COUNT IS TWO EMPTY MAPS, never nil ones: the count is summed
// across a fleet and sent over a wire, and `null` is not a count of nothing.
func TestNoNotificationOutcomesAreEmptyMaps(t *testing.T) {
	t.Parallel()
	got, err := open(t).Events().NotificationOutcomes(t.Context(), store.OutcomeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped == nil || got.Coalesced == nil || len(got.Skipped)+len(got.Coalesced) != 0 {
		t.Fatalf("an empty log counted %+v, want two empty maps", got)
	}
}
