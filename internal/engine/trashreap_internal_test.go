package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// trashCompanyDoc is a company with one declared knowledge space, so a page
// can be written, trashed and reaped.
const trashCompanyDoc = `
name: Nimbus
units:
  - name: Docs
    space: DOCS
    roles:
      - name: SWE
        handle: swe
`

// A DATA NODE WITH A KNOWLEDGE BASE ARMS THE REAPER, and the pass it runs
// purges a page through the domain's own purge record once the page has been
// in the trash past the term, and not a moment before.
//
// The whole wire, end to end: the source the engine adapts from its own write
// side, the store's listing and its purge, the term the engine passes, and the
// report the fleet keeps. A pass at today's clock finds the page inside its
// term and leaves it readable; a pass a term and two slots later purges it.
//
// Mutation: drop the pages source from [Engine.trashSources] and the node arms
// no reaper and the pass purges nothing.
func TestADataNodeReapsAPageOnlyOnceItsTermHasRunOut(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, trashCompanyDoc))
	e.trashReaper.mu.Lock()
	armed := e.trashReaper.loop != nil
	e.trashReaper.mu.Unlock()
	if !armed {
		t.Fatal("a publishing data node with a knowledge base armed no trash reaper")
	}
	if !slices.Contains(e.duties.list(), trashreap.Name) {
		t.Errorf("the reaper's duty is not among the duties the stop gives back")
	}

	kb := e.PagesStore()
	if kb == nil {
		t.Fatal("the premise: the node writes its knowledge base")
	}
	jane := pages.Actor{Handle: "jane", Kind: pages.AuthorHuman}
	page := createEventually(t, kb, jane, pages.NewPage{
		Container: "DOCS", Title: "Superseded runbook", Body: "old steps",
	})
	if _, err := kb.Trash(t.Context(), jane, page); err != nil {
		t.Fatalf("trash the page: %v", err)
	}

	fleet := coordmemory.NewFleet()
	reap := func(at time.Time) trashreap.SourceReport {
		t.Helper()
		r, err := trashreap.New(trashreap.Deps{
			Sources: e.trashSources(), Term: artifacts.TrashRetention,
			Reports: fleet, Node: e.id, Now: func() time.Time { return at },
		})
		if err != nil {
			t.Fatalf("build the reaper: %v", err)
		}
		report, err := r.Pass(t.Context())
		if err != nil {
			t.Fatalf("pass at %s: %v", at, err)
		}
		if len(report.Sources) != 1 || report.Sources[0].Source != "pages" {
			t.Fatalf("the pass reaped %+v, want the knowledge base alone", report.Sources)
		}
		return report.Sources[0]
	}

	if got := reap(time.Now()); got.Purged != 0 || got.Failed != 0 {
		t.Fatalf("a pass inside the term reports %+v", got)
	}
	if _, err := e.Pages().Get(t.Context(), page,
		statelog.Freshness{Level: statelog.ReadSession}); err != nil {
		t.Fatalf("a page inside its term no longer reads: %v", err)
	}

	later := time.Now().Add(artifacts.TrashRetention + 2*trashreap.PassInterval)
	if got := reap(later); got.Purged != 1 || got.Failed != 0 {
		t.Fatalf("a pass past the term reports %+v, want the page purged", got)
	}
	if _, err := e.Pages().Get(t.Context(), page,
		statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, pages.ErrNotFound) {
		t.Errorf("the page past its term still reads: %v", err)
	}
	if last, found, err := trashreap.LastReport(t.Context(), fleet); err != nil || !found ||
		last.Node != e.id {
		t.Errorf("the fleet's report is %+v (found %v, %v), want this node's pass", last,
			found, err)
	}
}

// createEventually writes a page, waiting out the moments after boot before
// the chart's space reaches this node's copy.
func createEventually(t *testing.T, kb *pages.Store, actor pages.Actor,
	in pages.NewPage) string {

	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		written, err := kb.Create(t.Context(), actor, in)
		if err == nil {
			return written.Page.ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("create %q: %v", in.Title, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// THE REAPER NEVER STARTS ONCE IT HAS BEEN STOPPED: the native half that arms
// it may be brought up by an apply as late as the teardown, and only the
// stop's own flag keeps a late start from running a duty nothing ends.
//
// Mutation: drop the stopped check from [Engine.startTrashReaper] and the
// loop starts on a node that has stopped.
func TestTheTrashReaperNeverStartsOnceStopped(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, trashCompanyDoc))
	e.Stop(context.Background())
	e.startTrashReaper(t.Context())
	e.trashReaper.mu.Lock()
	started := e.trashReaper.loop != nil
	e.trashReaper.mu.Unlock()
	if started {
		e.stopTrashReaper()
		t.Fatal("the reaper started on a node that had stopped it")
	}
}

// A PURGE IS COUNTED ONLY WHEN IT IS DURABLE. The state log answers an
// unknown outcome with no error, and a source that read that as a purge would
// report an item gone that may still be in the trash; a refusal naming the
// domain's not-eligible sentinels is a skip, and anything else a failure.
//
// Mutation: answer nil for [statelog.OutcomeUnknown] and the unknown case
// below reads as a purge.
func TestAReapAnswerCountsOnlyADurablePurge(t *testing.T) {
	t.Parallel()
	gone := errors.New("the item is gone")
	refused := errors.New("the store did not answer")
	for name, c := range map[string]struct {
		result      statelog.Result
		err         error
		wantNil     bool
		notEligible bool
	}{
		"applied":  {result: statelog.Result{Outcome: statelog.OutcomeApplied}, wantNil: true},
		"pending":  {result: statelog.Result{Outcome: statelog.OutcomePending}, wantNil: true},
		"unknown":  {result: statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op"}},
		"unheard":  {result: statelog.Result{Outcome: "sideways"}},
		"gone":     {err: fmt.Errorf("purge it: %w", gone), notEligible: true},
		"not ours": {err: refused},
	} {
		err := reapAnswer(c.result, c.err, gone)
		switch {
		case c.wantNil && err != nil:
			t.Errorf("%s: answered %v, want a purge", name, err)
		case !c.wantNil && err == nil:
			t.Errorf("%s: answered a purge", name)
		case err != nil && errors.Is(err, trashreap.ErrNotEligible) != c.notEligible:
			t.Errorf("%s: answered %v, not-eligible %v", name, err, c.notEligible)
		}
	}
}
