package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A COUNT IS OVER THE WINDOW ASKED FOR, not over a page of it.
//
// The rows here outnumber any page a caller would read, and the window starts
// part-way through them: a count folded from the newest page would report the
// page size, and one folded from a page read without the window would count
// rows from before it.
func TestATallyCountsTheWholeWindowByTypeAndTag(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-time.Hour)

	outcome := func(i int, typ, source string, at time.Time) {
		t.Helper()
		if err := log.Append(t.Context(), store.EventRecord{
			ID: fmt.Sprintf("n-%04d", i), Type: typ, Source: "engine",
			Category: "notification", Time: at,
			Tags: map[string]string{"notification_source": source},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	const inside = 450
	for i := range inside {
		typ, source := "notification_skipped", "gitlab"
		if i%3 == 0 {
			typ, source = "notifications_coalesced", "github"
		}
		outcome(i, typ, source, since.Add(time.Duration(i)*time.Second))
	}
	// Before the window: must not be counted.
	outcome(inside, "notification_skipped", "gitlab", since.Add(-time.Minute))
	// A row carrying no tag at all: counted, under an empty tag value.
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "untagged", Type: "notification_skipped", Source: "engine",
		Category: "notification", Time: since.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := log.Tally(t.Context(), store.TallyQuery{
		ListQuery: store.ListQuery{Category: "notification", Since: since},
		Tag:       "notification_source",
	})
	if err != nil {
		t.Fatalf("Tally: %v", err)
	}
	counts := map[[2]string]int{}
	for _, g := range got {
		if g.Source != "engine" {
			t.Errorf("a group names source %q, want the rows' own", g.Source)
		}
		counts[[2]string{g.Type, g.Tag}] = g.Count
	}
	want := map[[2]string]int{
		{"notifications_coalesced", "github"}: inside / 3,
		{"notification_skipped", "gitlab"}:    inside - inside/3,
		{"notification_skipped", ""}:          1,
	}
	if len(counts) != len(want) {
		t.Fatalf("tally = %v, want %v", counts, want)
	}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("tally[%v] = %d, want %d — the whole window, and nothing before it",
				k, counts[k], n)
		}
	}

	// AND IT AGREES WITH THE LISTING OF THE SAME FILTERS, which is the
	// property the shared predicate buys: walked to its end, the listing
	// holds exactly as many rows as the groups count.
	total := 0
	for _, g := range got {
		total += g.Count
	}
	rows := 0
	q := store.ListQuery{Category: "notification", Since: since, Limit: 400}
	for {
		page, err := log.List(t.Context(), q)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page) == 0 {
			break
		}
		rows += len(page)
		last := page[len(page)-1]
		q.Before = &store.Cursor{Time: last.Time, ID: last.ID}
	}
	if total != rows {
		t.Errorf("the tally counts %d events and the listing of the same filters "+
			"holds %d", total, rows)
	}
}

// THE SPAN EACH GROUP COVERS travels with it, so a caller can say what its count
// is a count OF — the question a bare number beside another bare number cannot
// answer.
func TestATallyGroupCarriesItsOldestAndNewest(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	first := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	for i := range 3 {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: fmt.Sprintf("w-%d", i), Type: "webhook_received", Source: "gitlab",
			Category: "webhook", Time: first.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := log.Tally(t.Context(), store.TallyQuery{
		ListQuery: store.ListQuery{Category: "webhook"},
	})
	if err != nil {
		t.Fatalf("Tally: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %+v, want one per (type, source)", got)
	}
	g := got[0]
	if g.Count != 3 || !g.Oldest.Equal(first) || !g.Newest.Equal(first.Add(2*time.Minute)) {
		t.Errorf("group = %+v, want 3 rows from %s to %s", g, first, first.Add(2*time.Minute))
	}
	if g.Tag != "" {
		t.Errorf("no tag was asked for and the group carries %q", g.Tag)
	}
}

// WHAT A TALLY REFUSES, and why each refusal names itself.
func TestATallyRefusesWhatItCannotCountHonestly(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	// The related filter adds trace siblings the predicate alone does not
	// match, so its count would disagree with its own listing.
	if _, err := log.Tally(t.Context(), store.TallyQuery{
		ListQuery: store.ListQuery{RelatedAgent: "pm"},
	}); !errors.Is(err, store.ErrTallyRelated) {
		t.Errorf("a related-agent tally answered %v, want ErrTallyRelated", err)
	}
	// A key with path syntax in it names a different key than it spells.
	for _, key := range []string{"a.b", "a[0]", `a"b`, "Notification_Source"} {
		if _, err := log.Tally(t.Context(), store.TallyQuery{Tag: key}); !errors.Is(err, store.ErrTallyTag) {
			t.Errorf("tag %q answered %v, want ErrTallyTag", key, err)
		}
	}
	// And nothing matching is an allocated empty answer, not nil.
	got, err := log.Tally(t.Context(), store.TallyQuery{
		ListQuery: store.ListQuery{Category: "nothing-writes-this"},
	})
	if err != nil {
		t.Fatalf("Tally: %v", err)
	}
	if got == nil {
		t.Error("an empty tally answered nil, which serializes as null")
	}
}
