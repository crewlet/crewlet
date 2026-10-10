package queries_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WINDOW A PAGE'S READERS COVER IS THE EVENT LOG'S, in whole company days:
// the two answer how far back the engine can say what its agents did, and a
// reader list over a window the turns behind it have left would name reads
// nothing can explain any more.
func TestAPagesReadersCoverTheEventLogsWindow(t *testing.T) {
	t.Parallel()
	if got := time.Duration(queries.PageReadsMaxDays) * 24 * time.Hour; got != store.EventHistory {
		t.Errorf("a page's readers cover %d days (%s), and the event log keeps %s",
			queries.PageReadsMaxDays, got, store.EventHistory)
	}
}

// pageWire asks for the page as caller and decodes the answer as the wire
// carries it.
func pageWire(t *testing.T, f *spendFixture, stub *stubPages, member bool) map[string]any {
	t.Helper()
	sources := f.sources()
	sources.Pages = stub
	caller := asAdmin("ops")
	if member {
		caller = asMember("ops")
	}
	got, err := registryOver(t, sources).Answer(t.Context(), "page", map[string]any{"id": readPage}, caller)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return decoded
}

// WHO READ A PAGE IS A MEMBER'S TO SEE, PER SEAT AND NOTHING ABOUT HOW. Every
// node's reads of the page, every way of reading summed, one row per seat with
// its count and its newest read — and none of `page_reads`' turn, unit of work,
// way of reading or search text, which are what the machine processed
// (ADR-0031). A tool skill OFFERED in a phase's catalogue is not a read.
func TestAPageNamesItsReadersPerSeatAndNothingAboutHow(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	at := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	f.reads("node-a", "2026-09-24", "lead", 0,
		read("search", 2, at, "run-a", "how do we deploy"), read("get_page", 1, at.Add(time.Hour), "run-b", ""))
	f.reads("node-b", "2026-09-25", "lead", 0, read("prefetch", 4, at.Add(2*time.Hour), "run-c", "deploy"))
	// THE NEWEST READ IS NOT THE LAST ROW: rows arrive by node, and node-b's
	// read of sre's is the older one.
	f.reads("node-a", "2026-09-25", "sre", 0, read("get_page", 1, at.Add(-time.Hour), "run-d", ""))
	f.reads("node-b", "2026-09-25", "sre", 0,
		read("search", 1, at.Add(-3*time.Hour), "run-f", "rollback"),
		read("skill_injected", 9, at.Add(3*time.Hour), "run-d", ""))
	// Outside the window: a read the event log no longer explains.
	f.reads("node-a", "2026-08-01", "sre", 0, read("get_page", 50, at.Add(-60*24*time.Hour), "run-e", ""))

	stub := &stubPages{detail: pages.Detail{Page: pages.Page{ID: readPage, Title: "Deploying"}}}
	readers, _ := pageWire(t, f, stub, true)["readers"].(map[string]any)
	got, _ := json.Marshal(readers)
	want := `{"complete":true,"days":30,"seats":[` +
		`{"handle":"lead","last_at":"2026-09-24T05:00:00Z","reads":7},` +
		`{"handle":"sre","last_at":"2026-09-24T02:00:00Z","reads":2}],` +
		`"since":"2026-08-27","total":2,"until":"2026-09-25"}`
	if string(got) != want {
		t.Errorf("a member's page readers are\n\t%s\nwant\n\t%s", got, want)
	}
	for _, machine := range []string{"run-", "how do we deploy", "via", "prefetch", "work_key"} {
		if strings.Contains(string(got), machine) {
			t.Errorf("a page's readers carry %q, which is how the machine read it: %s", machine, got)
		}
	}
	// THE SAME FOR AN ADMIN: who read a page is not a different fact by reach.
	if admin, _ := json.Marshal(pageWire(t, f, stub, false)["readers"]); string(admin) != want {
		t.Errorf("an admin's page readers are %s, want the member's %s", admin, want)
	}
}

// "NOBODY READ IT" IS AN EMPTY LIST, and a window whose recorded reads were cut
// by the per-seat-day cap anywhere says the list may be short rather than
// presenting it as whole.
func TestAnUnreadPageSaysSoAndAShortListSaysItMayBe(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	stub := &stubPages{detail: pages.Detail{Page: pages.Page{ID: readPage, Title: "Deploying"}}}
	readers := pageWire(t, f, stub, true)["readers"].(map[string]any)
	if seats, ok := readers["seats"].([]any); !ok || len(seats) != 0 || readers["complete"] != true {
		t.Errorf("an unread page's readers = %v, want an empty list, complete", readers)
	}
	f.reads("node-a", "2026-09-25", "lead", 3, read("get_page", 1,
		time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC), "run-a", ""))
	if readers := pageWire(t, f, stub, true)["readers"].(map[string]any); readers["complete"] != false {
		t.Errorf("a window whose reads were cut reads complete: %v", readers)
	}
}
