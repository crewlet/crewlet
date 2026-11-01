package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
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
	jane := doclet.Actor{Handle: "jane", Kind: doclet.AuthorHuman}
	page := createEventually(t, kb, jane, doclet.NewPage{
		Container: "DOCS", Title: "Superseded runbook", Body: "old steps",
	})
	if _, err := kb.Trash(t.Context(), jane, page); err != nil {
		t.Fatalf("trash the page: %v", err)
	}

	fleet := coordmemory.NewFleet()
	reap := func(at time.Time) trashreap.SourceReport {
		t.Helper()
		return reapSource(t, e, fleet, at, "pages")
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
		statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, doclet.ErrNotFound) {
		t.Errorf("the page past its term still reads: %v", err)
	}
	if last, found, err := trashreap.LastReport(t.Context(), fleet); err != nil || !found ||
		last.Node != e.id {
		t.Errorf("the fleet's report is %+v (found %v, %v), want this node's pass", last,
			found, err)
	}
}

// reapSource runs one pass of the reaper over every source this node arms, at
// at, and answers the named source's part of the report, failing when the pass
// did not visit every source the node offers or failed the named one.
//
// THE NAMED SOURCE'S FAILURE, NOT THE PASS'S. A pass's error joins every
// source's, and on these nodes, whose company no activation has named, the
// assistant's term is unknown and its source's part fails by design
// ([Engine.assistantRetention]); the cases are about the source they name.
func reapSource(t *testing.T, e *Engine, fleet trashreap.Reports, at time.Time,
	source string) trashreap.SourceReport {

	t.Helper()
	sources := e.trashSources()
	r, err := trashreap.New(trashreap.Deps{
		Sources: sources, Term: artifacts.TrashRetention,
		Reports: fleet, Node: e.id, Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("build the reaper: %v", err)
	}
	report, passErr := r.Pass(t.Context())
	var names []string
	for _, src := range sources {
		names = append(names, src.Name())
	}
	// THE ASSISTANT'S TOO: a conversation untouched for its retention is
	// purged through the same reaper, a Termed source of its own.
	if !slices.Equal(names, []string{"pages", artifactsdomain.Domain{}.Name(),
		assistantdomain.Domain{}.Name()}) {
		t.Fatalf("the node arms the sources %v, want the knowledge base's, the "+
			"artifacts' and the assistant's", names)
	}
	for _, got := range report.Sources {
		if got.Source == source {
			if got.Error != "" {
				t.Fatalf("the pass at %s failed %s: %s (the whole pass: %v)", at,
					source, got.Error, passErr)
			}
			return got
		}
	}
	t.Fatalf("the pass reaped %+v and nothing of %s (%v)", report.Sources, source, passErr)
	return trashreap.SourceReport{}
}

// A DATA NODE REAPS AN ARTIFACT ONLY ONCE ITS TERM HAS RUN OUT, through the
// artifacts domain's own guarded purge, as the engine.
//
// Mutation: drop the artifacts source from [Engine.trashSources] and the pass
// visits no artifact.
func TestADataNodeReapsAnArtifactOnlyOnceItsTermHasRunOut(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, trashCompanyDoc))
	w, r := e.ArtifactsWriter(), e.ArtifactsReader()
	if w == nil || r == nil {
		t.Fatal("the premise: every data node runs the artifacts estate")
	}
	jane := artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: "swe", Kind: iam.ActorHuman}, Role: artifacts.RoleOwner,
		Limits: artifactsLimits(), Origin: artifacts.OriginDashboard,
	}}
	content := []byte("# Superseded notes\n")
	obj := objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.HashOf(content),
		Size: int64(len(content))}
	made, err := w.Create(t.Context(), jane, artifactsdomain.CreateInput{
		Kind: artifacts.KindDoc, Title: "Superseded notes", Owner: "swe",
		OwnerKind: iam.ActorHuman, First: artifactsdomain.PublishInput{
			Uploaded: []objstore.Key{obj.Key},
			Manifest: artifacts.Manifest{Entry: "notes.md", Files: []artifacts.Entry{{
				Path: "notes.md", ContentType: "text/markdown; charset=utf-8", Object: obj,
			}}},
		},
	})
	if err != nil {
		t.Fatalf("create the artifact: %v", err)
	}
	if _, err := w.Trash(t.Context(), jane, made.Artifact); err != nil {
		t.Fatalf("trash the artifact: %v", err)
	}

	fleet := coordmemory.NewFleet()
	if got := reapSource(t, e, fleet, time.Now(), "artifacts"); got.Purged != 0 || got.Failed != 0 {
		t.Fatalf("a pass inside the term reports %+v", got)
	}
	later := time.Now().Add(artifacts.TrashRetention + 2*trashreap.PassInterval)
	if got := reapSource(t, e, fleet, later, "artifacts"); got.Purged != 1 || got.Failed != 0 {
		t.Fatalf("a pass past the term reports %+v, want the artifact purged", got)
	}
	if _, _, err := r.Artifact(t.Context(), made.Artifact,
		statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, artifactsdomain.ErrPurged) {
		t.Errorf("the artifact past its term still reads: %v", err)
	}
}

// artifactsLimits are the plan quotas a case stamps into an artifacts write.
func artifactsLimits() artifacts.Limits {
	return artifacts.Limits{
		CompanyBytes: artifacts.DefaultCompanyBytes, Artifacts: artifacts.DefaultArtifacts,
		DataBytes: artifacts.DefaultDataBytes, DataStoreBytes: artifacts.DefaultDataStoreBytes,
		PendingBytes: artifacts.DefaultPendingBytes,
	}
}

// createEventually writes a page, waiting out the moments after boot before
// the chart's space reaches this node's copy.
func createEventually(t *testing.T, kb *doclet.Store, actor doclet.Actor,
	in doclet.NewPage) string {

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
