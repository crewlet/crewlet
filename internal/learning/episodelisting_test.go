package learning_test

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// A LISTING READS THE ROW RECENT READS, BUT FOR ITS TWO OPENINGS AND ITS VECTOR.
//
// The listing's statement is derived from the row's own column list rather than
// spelled again, so every column the row holds is read — and this holds the
// derivation to it: a listing with an opening wider than every text is, column
// for column, what Recent returns, less the vector no listing draws; and with a
// narrow one the ask and the account are their openings, each whole size said.
func TestAListingReadsTheRowButForItsOpeningsAndItsVector(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	long := ep("e1", "cto", base)
	long.Ask = strings.Repeat("what the turn was asked, ", 40)
	long.PlanSummary = strings.Repeat("what it did, ", 40)
	long.ToolSequence, long.SkillsUsed = []string{"read", "page"}, []string{"triage"}
	long.WorkKey, long.ConversationKey = "wk-e1", "slack:C1"
	long.Embedding = []float32{0.5, 0.5}
	folded := ep("c1", "cto", base.Add(time.Hour))
	folded.Kind, folded.Count, folded.SuccessRate = learning.KindCompacted, 4, 0.75
	folded.CommonTaskPattern, folded.NotablePatterns = "triaging pages", "one escalated"
	folded.ExemplarTurnIDs, folded.SubjectsInvolved = []string{"turn-x"}, []string{"sre"}
	folded.WorkKey, folded.Embedding = "wk-c1", nil
	for _, e := range []learning.Episode{long, folded} {
		if _, err := store.Append(t.Context(), e); err != nil {
			t.Fatalf("append %s: %v", e.ID, err)
		}
	}
	recent, err := store.Recent(t.Context(), "cto", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	wide, err := store.Listing(t.Context(), "cto", 10, 1<<20, 1<<20)
	if err != nil || len(wide) != len(recent) {
		t.Fatalf("Listing = %d rows, %v; want Recent's %d", len(wide), err, len(recent))
	}
	for i, listed := range wide {
		want := recent[i]
		want.Embedding, want.EmbeddingModel = nil, ""
		if !reflect.DeepEqual(listed.Episode, want) {
			t.Errorf("listed %s = %+v, want Recent's row %+v less its vector", want.ID,
				listed.Episode, want)
		}
		if listed.AskBytes != len(want.Ask) || listed.PlanSummaryBytes != len(want.PlanSummary) {
			t.Errorf("listed %s names %d and %d bytes, want %d and %d", want.ID,
				listed.AskBytes, listed.PlanSummaryBytes, len(want.Ask), len(want.PlanSummary))
		}
	}

	narrow, err := store.Listing(t.Context(), "cto", 10, 32, 1<<20)
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	for _, listed := range narrow {
		if listed.ID != "e1" {
			continue
		}
		if listed.Ask != long.Ask[:32] || listed.AskBytes != len(long.Ask) ||
			listed.PlanSummary != long.PlanSummary[:32] ||
			listed.PlanSummaryBytes != len(long.PlanSummary) {
			t.Fatalf("a narrow listing read %q (%d) and %q (%d), want the 32-byte openings "+
				"and the whole sizes", listed.Ask, listed.AskBytes, listed.PlanSummary,
				listed.PlanSummaryBytes)
		}
	}
	if whole, found, err := store.Get(t.Context(), "cto", "e1"); err != nil || !found ||
		whole.Ask != long.Ask || whole.PlanSummary != long.PlanSummary {
		t.Fatalf("Get = found %v, %v; want the row whole", found, err)
	}
	if _, found, err := store.Get(t.Context(), "ceo", "e1"); err != nil || found {
		t.Fatalf("another seat's read of e1 found it (%v, %v)", found, err)
	}
}

// A LISTED OPENING NEVER ENDS INSIDE A MARKER. An episode's ask and account are
// redacted on the turn's completed record, so a stored text holds markers, and
// an opening cut inside one shows its head as the turn's own words. With a key's
// marker and a bearer token's run of markers in the text and the opening at
// every offset of it, each opening is the text up to the opening, moved back to
// where the markers it fell inside begin, and its whole size is the text's.
//
// Mutation: cut at the opening alone, or read the text only the opening deep,
// and a cut inside a marker keeps it in part.
func TestAListedOpeningNeverEndsInsideAMarker(t *testing.T) {
	t.Parallel()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	text := redact.Secrets("deploy key " + sample["anthropic-key"] + " then Authorization: Bearer " +
		sample["jwt"] + " and the words after it")
	if strings.Count(text, redact.Marker) < 2 {
		t.Fatalf("the redacted text %q holds fewer than two markers", text)
	}
	store := episodes(t)
	e := ep("e1", "cto", base)
	e.Ask, e.PlanSummary = text, text
	if _, err := store.Append(t.Context(), e); err != nil {
		t.Fatalf("append: %v", err)
	}
	for opening := 1; opening <= len(text); opening++ {
		want := text[:opening]
		if begin, _, inside := redact.MarkersAround(text, opening); inside {
			want = text[:begin]
		}
		listed, err := store.Listing(t.Context(), "cto", 1, opening, 1<<20)
		if err != nil || len(listed) != 1 {
			t.Fatalf("Listing at %d = %d rows, %v; want one", opening, len(listed), err)
		}
		got := listed[0]
		for name, shown := range map[string]string{"ask": got.Ask, "account": got.PlanSummary} {
			if shown != want {
				t.Errorf("an opening of %d lists the %s as %q, want %q", opening, name, shown, want)
			}
			for _, words := range redact.OwnWords(shown) {
				if strings.Contains(words, "REDACTED") {
					t.Errorf("an opening of %d lists the %s with a marker in part: %q", opening, name, shown)
					break
				}
			}
		}
		if got.AskBytes != len(text) || got.PlanSummaryBytes != len(text) {
			t.Errorf("an opening of %d names %d and %d bytes, want the stored %d", opening,
				got.AskBytes, got.PlanSummaryBytes, len(text))
		}
	}
}

// A LISTING ENDS BEFORE THE ROW THAT WOULD PASS ITS BYTES, AND ALWAYS HOLDS ONE.
// The openings bound two texts; the label, the lists and a compacted row's
// patterns are bounded by nothing, so fifty rows could pass the reply they are
// sent in. A listing counts every text a row holds and stops before the row
// that would take it past its bound, newest first; a seat whose newest row
// alone is past it is still shown that row.
//
// Mutations, each red: ignore maxBytes; drop the first-row rule; count only the
// two opened texts.
func TestAListingEndsBeforeTheRowThatPassesItsBytes(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	label := strings.Repeat("l", 1000)
	for i := range 4 {
		e := ep("e"+strconv.Itoa(i), "cto", base.Add(time.Duration(i)*time.Minute))
		e.TaskSummary = label
		if _, err := store.Append(t.Context(), e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	for _, c := range []struct {
		maxBytes int
		want     []string
	}{
		{2500, []string{"e3", "e2"}},
		{1, []string{"e3"}},
		{1 << 20, []string{"e3", "e2", "e1", "e0"}},
	} {
		listed, err := store.Listing(t.Context(), "cto", 10, 32, c.maxBytes)
		if err != nil {
			t.Fatalf("Listing(%d): %v", c.maxBytes, err)
		}
		var ids []string
		for _, l := range listed {
			ids = append(ids, l.ID)
		}
		if !slices.Equal(ids, c.want) {
			t.Errorf("a listing bounded to %d bytes of 1000-byte labels held %v, want %v",
				c.maxBytes, ids, c.want)
		}
	}
	if _, err := store.Listing(t.Context(), "cto", 10, 32, 0); err == nil {
		t.Error("a listing with no byte bound was read, want it refused")
	}
}
