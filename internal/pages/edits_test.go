package pages_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EDIT THAT ALSO RELABELS OR MOVES A PAGE IS A SAVE, IN THE FEED AND IN THE
// WAKE ALIKE.
//
// A save that touched the body and one other field wakes its watchers as
// `saved`, and its history row — what the activity feed's card renders and a
// `kinds=saved` filter matches — must say the same. The applier once took the
// verb from whichever field it handled last, so a body+labels save landed in
// the feed as `labels`, under the body's first line as its excerpt. Two cases,
// a relabel and a move, each of which that rule named instead of the save.
func TestAnEditThatAlsoRelabelsOrMovesAPageIsASaveInTheFeed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	home := r.write(author("jane"), pages.NewPage{Title: "Home", Body: "prose"})
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "v1"})

	relabelled := "v2"
	saved, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{
			BaseVersion: page.Page.Version, Body: &relabelled,
			Labels: &[]string{"ops"},
		})
	if err != nil {
		t.Fatalf("save a body and labels: %v", err)
	}
	wakes := []pages.ChangeKind{r.notifyKind()}
	r.drain()

	moved := "v3"
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{
			BaseVersion: saved.Page.Version, Body: &moved,
			ParentID: &home.Page.ID,
		}); err != nil {
		t.Fatalf("save a body and a parent: %v", err)
	}
	wakes = append(wakes, r.notifyKind())
	r.drain()

	for i, kind := range wakes {
		if kind != pages.ChangeSaved {
			t.Errorf("edit %d woke its watchers as %q, want saved", i+1, kind)
		}
	}
	feed := r.activity(pages.PageActivityQuery{
		Page: page.Page.ID, Kinds: []pages.ChangeKind{pages.ChangeSaved},
	})
	if len(feed.Changes) != 2 {
		kinds := r.activity(pages.PageActivityQuery{Page: page.Page.ID}).Changes
		t.Fatalf("a `saved` filter over the page finds %d changes, want both "+
			"edits; the page's whole feed is %+v", len(feed.Changes), kinds)
	}
}

// notifyKind is the kind the newest record's wake carries.
func (r *roundTrip) notifyKind() pages.ChangeKind {
	r.t.Helper()
	record, err := pages.Decode(r.lastRecord())
	if err != nil {
		r.t.Fatalf("decode the newest record: %v", err)
	}
	if record.Notify == nil {
		r.t.Fatal("the newest record carries no wake")
	}
	return record.Notify.Kind
}

// A SAVE THAT REMOVES EVERY LABEL REMOVES THEM ON EVERY NODE.
//
// The set travels whole, and an EMPTY set is the one a plain slice with
// `omitempty` cannot carry: it is dropped exactly as an untouched field is, so
// the record announced a label change and every applier left the labels where
// they were — the writer's answer said "no labels" and every read said
// otherwise.
func TestASaveThatRemovesEveryLabelRemovesThem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{
		Title: "Runbook", Body: "prose", Labels: []string{"ops", "oncall"},
	})
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: page.Page.Version, Labels: &[]string{}}); err != nil {
		t.Fatalf("save with no labels: %v", err)
	}
	r.drain()

	if got := r.get(page.Page.ID).Page.Labels; len(got) != 0 {
		t.Errorf("the page still carries labels %v after a save that removed "+
			"all of them", got)
	}
	if got := r.list(pages.Filter{Label: "ops"}); len(got.Pages) != 0 {
		t.Errorf("a listing by a removed label still finds %v", titles(got))
	}
}

// A PARENT THAT DOES NOT EXIST, OR THAT WOULD CLOSE A LOOP, IS REFUSED NAMING
// THE FIELD — AND NOTHING IS PUBLISHED.
//
// A page parented under itself or one of its own descendants has a parent
// chain that never reaches the top of its container, and so does everything
// under it; a parent id nothing holds ends the chain at a page nobody can
// open. Both are refused inside the write's own snapshot, where the writer can
// still be told.
func TestAParentThatDoesNotExistOrWouldCloseALoopIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	top := r.write(author("jane"), pages.NewPage{Title: "Top", Body: "prose"})
	middle := r.write(author("jane"), pages.NewPage{
		Title: "Middle", Body: "prose", ParentID: top.Page.ID,
	})
	bottom := r.write(author("jane"), pages.NewPage{
		Title: "Bottom", Body: "prose", ParentID: middle.Page.ID,
	})
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	for name, parent := range map[string]string{
		"itself":           top.Page.ID,
		"its child":        middle.Page.ID,
		"its grandchild":   bottom.Page.ID,
		"a page not there": "no-such-page",
	} {
		_, err := r.store.SavePage(t.Context(), author("jane"), top.Page.ID,
			pages.Save{BaseVersion: top.Page.Version, ParentID: &parent})
		if !errors.Is(err, pages.ErrInvalid) || !strings.Contains(err.Error(), "parent_id") {
			t.Errorf("a save putting Top under %s answered %v, want a refusal "+
				"naming parent_id", name, err)
		}
	}
	if _, err := r.store.Create(t.Context(), author("jane"), pages.NewPage{
		Container: "ENG", Title: "Orphan", Body: "prose", ParentID: "no-such-page",
	}); !errors.Is(err, pages.ErrInvalid) || !strings.Contains(err.Error(), "parent_id") {
		t.Errorf("a create under a page that is not there answered %v, want a "+
			"refusal naming parent_id", err)
	}
	after, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after != before {
		t.Errorf("the refused writes published %d records", after-before)
	}

	// AND THE MOVES THAT ARE NOT LOOPS STILL LAND: a page under a sibling
	// branch, and a page back to the top of its container.
	sibling := r.write(author("jane"), pages.NewPage{Title: "Sibling", Body: "prose"})
	if _, err := r.store.SavePage(t.Context(), author("jane"), bottom.Page.ID,
		pages.Save{BaseVersion: bottom.Page.Version, ParentID: &sibling.Page.ID}); err != nil {
		t.Errorf("moving Bottom under an unrelated page was refused: %v", err)
	}
	r.drain()
	toTop := ""
	if _, err := r.store.SavePage(t.Context(), author("jane"), middle.Page.ID,
		pages.Save{BaseVersion: middle.Page.Version, ParentID: &toTop}); err != nil {
		t.Errorf("moving Middle to the top of its container was refused: %v", err)
	}
}

// A PARENT CHAIN THAT LOOPS STILL ANSWERS, AND NAMES EACH PAGE ON IT ONCE.
//
// A single write cannot close a loop — the test above is that refusal — but
// two can between them: each moves a different page, on its own page's
// subject, decided before the other applied, so neither snapshot holds the
// other's move and the broker orders the two independently. With no depth cap
// the walk's record of what it has visited is the only thing that ends it, so
// this is the test that goes red, by never finishing, if that record goes.
func TestAParentChainThatLoopsNamesEachPageOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	one := r.write(author("jane"), pages.NewPage{Title: "One", Body: "prose"})
	two := r.write(author("jane"), pages.NewPage{Title: "Two", Body: "prose"})

	// NO DRAIN BETWEEN THEM: that is what makes the two decisions
	// concurrent, since each reads this node's applied rows.
	if _, err := r.store.SavePage(t.Context(), author("jane"), one.Page.ID,
		pages.Save{BaseVersion: one.Page.Version, ParentID: &two.Page.ID}); err != nil {
		t.Fatalf("put One under Two: %v", err)
	}
	if _, err := r.store.SavePage(t.Context(), author("jane"), two.Page.ID,
		pages.Save{BaseVersion: two.Page.Version, ParentID: &one.Page.ID}); err != nil {
		t.Fatalf("put Two under One: %v", err)
	}
	r.drain()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	detail, err := r.reader.Get(ctx, one.Page.ID,
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("a page on a loop cannot be read: %v", err)
	}
	if detail.Page.ParentID != two.Page.ID {
		t.Fatalf("One's parent is %q, want Two — the loop this test is about "+
			"never formed", detail.Page.ParentID)
	}
	seen := map[string]int{}
	for _, ancestor := range detail.Ancestors {
		seen[ancestor.ID]++
	}
	if len(detail.Ancestors) != 2 || seen[one.Page.ID] != 1 || seen[two.Page.ID] != 1 {
		t.Errorf("the loop's breadcrumb is %+v, want each of its two pages once",
			detail.Ancestors)
	}
}

// A PATCH THAT NAMES NO FIELD WRITES NOTHING, AND DOES NOT STOP THE LOG.
//
// This build never publishes one, but a peer on another build can — the
// writer that carried an emptied label set as an absent field did exactly
// that — and a rolling upgrade puts that peer's records in front of this
// applier. A refusal would be retried for ever on every node that read it,
// and a history row would need a verb nothing in the record states. So it
// applies as what it says: nothing.
func TestAPatchThatNamesNoFieldWritesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, _, err := h.apply(create("page-1", "ENG", "Deploy", "# v1\n")); err != nil {
		t.Fatalf("create: %v", err)
	}
	heads := h.column(`SELECT document FROM pages_heads WHERE id = ?`, "page-1")
	history := h.count("pages_history")

	rows, gate, err := h.apply(record(pages.PageSubject("page-1"), pages.OpPatch,
		"op-empty", pages.PagePatch{V: pages.DocumentVersion},
		pages.ScopeSet{Subject: true, Container: "ENG"}))
	if err != nil || gate != "" {
		t.Fatalf("an empty patch answered %v (gate %q), want it applied as "+
			"nothing", err, gate)
	}
	if rows != 0 || h.count("pages_history") != history {
		t.Errorf("an empty patch wrote %d rows and left %d history rows where "+
			"there were %d", rows, h.count("pages_history"), history)
	}
	if got := h.column(`SELECT document FROM pages_heads WHERE id = ?`,
		"page-1"); !slices.Equal(got, heads) {
		t.Error("an empty patch rewrote the page's head")
	}
}
