package store_test

import (
	"maps"
	"slices"
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

// outcomeRecord is one notification outcome as a stateless node's custody
// batch carries it.
func outcomeRecord(id, kind, app string, at time.Time) store.EventRecord {
	tags := map[string]string{}
	if app != "" {
		tags["notification_source"] = app
	}
	return store.EventRecord{ID: id, Type: kind, Source: "engine", Category: "notification",
		Time: at, Summary: kind, Tags: tags, Payload: []byte(`{}`)}
}

// rowIDs is the ids of some outcome rows, sorted.
func rowIDs(rows []store.UnsettledRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}

// A ROW THIS NODE DOES NOT YET KNOW IT KEEPS IS NAMED, NOT COUNTED.
//
// A stateless node's outcomes reach the data nodes in custody batches, and a
// batch whose claim failed is written by a second keeper before the first has
// learned it is not its own: until the first settles it, two logs hold the same
// row, and a fleet summing their counts counted it twice. So the count is of
// the rows this node keeps, and the window's rows of a batch it has written
// and not settled are named beside it by identity, type and app — once each,
// and only those the window and the count would hold. Settled as kept, they
// are counted like any other; settled as another node's, they are gone.
//
// Mutation: count the unsettled rows in the maps as well as naming them, and
// gitlab's drop is counted while it is named; name rows outside the window, or
// with no app, and the list holds them.
func TestAnUnsettledCustodyRowIsNamedRatherThanCounted(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	since := at.Add(-2 * time.Hour)
	outcome(t, log, "own", "notification_skipped", "gitlab", at.Add(-time.Minute))
	kept := store.CustodyBatch{ID: "batch-kept", Origin: "seats-1", Records: []store.EventRecord{
		outcomeRecord("c-skip", "notification_skipped", " gitlab ", at.Add(-2*time.Minute)),
		outcomeRecord("c-merge", "notifications_coalesced", "slack", at.Add(-2*time.Minute)),
		outcomeRecord("c-early", "notification_skipped", "gitlab", since.Add(-time.Second)),
		outcomeRecord("c-untagged", "notification_skipped", "", at.Add(-2*time.Minute)),
	}}
	released := store.CustodyBatch{ID: "batch-released", Origin: "seats-1", Records: []store.EventRecord{
		outcomeRecord("r-skip", "notification_skipped", "jira", at.Add(-3*time.Minute)),
	}}
	for _, b := range []store.CustodyBatch{kept, released} {
		if err := log.WriteCustody(t.Context(), b, at); err != nil {
			t.Fatal(err)
		}
	}
	count := func() store.NotificationOutcomes {
		t.Helper()
		got, err := log.NotificationOutcomes(t.Context(), store.OutcomeQuery{Since: since, At: at})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := count()
	if want := map[string]int{"gitlab": 1}; !maps.Equal(got.Skipped, want) || len(got.Coalesced) != 0 {
		t.Errorf("while unsettled: skipped %v, coalesced %v — want this node's own drop alone",
			got.Skipped, got.Coalesced)
	}
	if ids := rowIDs(got.Unsettled); !slices.Equal(ids, []string{"c-merge", "c-skip", "r-skip"}) {
		t.Errorf("unsettled %v, want the window's three batch rows that name an app", ids)
	}
	for _, r := range got.Unsettled {
		want := map[string]store.UnsettledRow{
			"c-skip":  {Type: "notification_skipped", App: "gitlab"},
			"c-merge": {Type: "notifications_coalesced", App: "slack"},
			"r-skip":  {Type: "notification_skipped", App: "jira"},
		}[r.ID]
		if r.Type != want.Type || r.App != want.App || r.Time.IsZero() {
			t.Errorf("unsettled %s is %+v, want its type %s, its app %s trimmed and its instant",
				r.ID, r, want.Type, want.App)
		}
	}

	if err := log.SettleCustody(t.Context(), kept.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := log.SettleCustody(t.Context(), released.ID, false); err != nil {
		t.Fatal(err)
	}
	got = count()
	if want := map[string]int{"gitlab": 2}; !maps.Equal(got.Skipped, want) {
		t.Errorf("once settled: skipped %v, want this node's own drop and the batch it keeps", got.Skipped)
	}
	if want := map[string]int{"slack": 1}; !maps.Equal(got.Coalesced, want) {
		t.Errorf("once settled: coalesced %v, want the kept batch's merge", got.Coalesced)
	}
	if len(got.Unsettled) != 0 {
		t.Errorf("once settled: unsettled %v, want none", rowIDs(got.Unsettled))
	}
}

// A NODE SAYS WHICH NAMED ROWS IT KEEPS: held in its log, and not as a row of
// a batch it has written and not settled. It is the other half of the count
// above — a row one node names unsettled may be kept by the batch's keeper,
// whose count already holds it — so it answers by the log's own identity,
// time and id together, and a row with the right id at another instant, or one
// it does not hold at all, is not kept.
//
// Mutation: answer every row the log holds, unsettled or not, and the row of
// the batch in flight is answered as kept; match by id alone, and the row at
// another instant is.
func TestKeptRowsAreTheRowsThisNodeKeeps(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	outcome(t, log, "own", "notification_skipped", "gitlab", at)
	for _, b := range []store.CustodyBatch{
		{ID: "batch-kept", Origin: "seats-1", Records: []store.EventRecord{
			outcomeRecord("kept", "notification_skipped", "gitlab", at)}},
		{ID: "batch-open", Origin: "seats-1", Records: []store.EventRecord{
			outcomeRecord("open", "notification_skipped", "gitlab", at)}},
	} {
		if err := log.WriteCustody(t.Context(), b, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.SettleCustody(t.Context(), "batch-kept", true); err != nil {
		t.Fatal(err)
	}
	got, err := log.KeptRows(t.Context(), []store.UnsettledRow{
		{Time: at, ID: "own"}, {Time: at, ID: "kept"}, {Time: at, ID: "open"},
		{Time: at, ID: "absent"}, {Time: at.Add(time.Microsecond), ID: "own"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := rowIDs(got); !slices.Equal(ids, []string{"kept", "own"}) {
		t.Errorf("kept %v, want this node's own row and the batch it settled as its own", ids)
	}
	if none, err := log.KeptRows(t.Context(), nil); err != nil || none == nil || len(none) != 0 {
		t.Errorf("naming nothing answered %v (%v), want an empty list", none, err)
	}
}
