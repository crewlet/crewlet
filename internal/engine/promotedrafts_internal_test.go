package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
)

// draftingProvider answers every auxiliary call with one promotion draft, and
// counts the calls — a call is what a convergence already drafted must not
// cost again.
type draftingProvider struct {
	answer string
	calls  atomic.Int32
}

func (*draftingProvider) Model() string { return "draft-test" }

func (p *draftingProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	p.calls.Add(1)
	return &llm.Completion{Model: "draft-test", Content: p.answer}, nil
}

// promotionTeam is a company on the engine's own knowledge base — the default
// — with one unit of three agent seats that files its pages in ENG.
const promotionTeam = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
units:
  - name: Platform
    space: ENG
    roles:
      - name: Dev
        handle: dev
        llm: zulu
      - name: Reliability
        handle: sre
        llm: zulu
      - name: Quality
        handle: qa
        llm: zulu
`

// rotationDraft is what the model distils the three seats' skills into, and
// rotationWords are words only its procedure carries.
const (
	rotationDraft = `{"name":"rotate-the-signing-key",` +
		`"description":"Replace the release signing key",` +
		`"content":"1. mint a replacement keypair\n2. publish the new fingerprint"}`
	rotationWords = "replacement keypair fingerprint"
)

// promotingEngine boots a node on promotionTeam and hands back the promotion
// pass the engine would arm, and the model it asks, which answers
// rotationDraft.
func promotingEngine(t *testing.T) (*Engine, *learning.Promoter, *draftingProvider) {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(promotionTeam))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the native backends to hydrate", e.NativeHydrated)

	// THE ENGINE'S OWN RESOLVER, ROSTER AND LEDGER, and only the model
	// replaced: the wiring under test is which knowledge base the pass is
	// handed, which container each unit files in, and what the fleet
	// remembers of it.
	model := &draftingProvider{answer: rotationDraft}
	promoter, err := learning.NewPromoter(learning.PromoterOptions{
		Writer:      e.promotionWriter,
		Ledger:      e.backends.Fleet,
		Skills:      learning.NewSkills(e.backends.Store),
		Models:      staticModels{provider: model},
		Units:       e.promotionUnits,
		MinSiblings: 3,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	now := time.Now().UTC()
	for _, handle := range []string{"dev", "sre", "qa"} {
		if err := learning.NewSkills(e.backends.Store).Insert(t.Context(), learning.Skill{
			ID: handle + "/rotate", AgentHandle: handle, Name: "rotate-" + handle,
			Description:  "how " + handle + " rotates the key",
			Content:      "1. mint\n2. publish",
			ToolSequence: []string{"mint_key", "publish_fingerprint", "announce"},
			CreatedAt:    now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s's skill: %v", handle, err)
		}
	}
	return e, promoter, model
}

// A NATIVE COMPANY'S PROMOTION DRAFTS A PAGE ITS SEARCH HIDES UNTIL A LEAD
// MOVES IT OUT.
//
// The engine's own knowledge base is the default, so a pass with no writer
// for it drafts nothing for most companies and says only that there is
// nowhere to put a draft. What only a running node can show is the whole
// chain: the pass files a reviewable page under the container's
// auto-drafted parent, every seat's search leaves it out, a second pass finds
// it rather than writing it again, and moving it out of the parent is the one
// gesture that publishes it.
//
// Mutations, each red here: a writer resolved by whether a Confluence client
// is wired drafts nothing; a draft filed at the top of its container has no
// parent chain to hide it; a draft the second pass finds, reported as
// created, is announced again.
func TestANativeCompanysPromotionDraftIsHiddenUntilItIsMovedOut(t *testing.T) {
	t.Parallel()
	e, promoter, model := promotingEngine(t)
	ctx := t.Context()

	out := promoter.Pass(ctx)
	if len(out) != 1 {
		t.Fatalf("the pass announced %d promotion(s), want 1 — a native company "+
			"whose seats converged got no draft", len(out))
	}
	promoted, ok := out[0].(types.SkillPromoted)
	if !ok {
		t.Fatalf("the pass announced a %T", out[0])
	}
	if promoted.ContainerKey != "ENG" || promoted.PageID == "" {
		t.Fatalf("the promotion is %+v, want a page in the unit's own ENG", promoted)
	}

	// A REVIEWABLE PAGE, under the auto-drafted parent, at the address the
	// pass announced.
	detail, err := e.Pages().Get(ctx, promoted.PageID,
		statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("read the announced draft: %v", err)
	}
	draft := detail.Page
	if draft.Title != promoted.PageTitle ||
		!strings.HasPrefix(draft.Title, knowledge.AutoDraftTitlePrefix) {
		t.Errorf("the draft is titled %q, announced as %q, want the auto-draft "+
			"prefix on both", draft.Title, promoted.PageTitle)
	}
	var chain []string
	for _, ancestor := range detail.Ancestors {
		chain = append(chain, ancestor.Title)
	}
	if !slices.Equal(chain, []string{knowledge.AutoDraftedParent}) {
		t.Fatalf("the draft sits under %v, want [%s] — the parent is what "+
			"keeps it out of every seat's search", chain, knowledge.AutoDraftedParent)
	}
	if !strings.Contains(draft.Body, "publish the new fingerprint") {
		t.Errorf("the draft does not carry the procedure it promotes:\n%s", draft.Body)
	}
	// THE ENGINE'S PAGE, WATCHED BY NOBODY: its creation reaches the
	// container's lead, and no seat is subscribed to a page it never wrote.
	if draft.Author != pages.SystemName || len(draft.Watchers) != 0 {
		t.Errorf("the draft is authored by %q and watched by %v, want the engine "+
			"and nobody", draft.Author, draft.Watchers)
	}

	// HIDDEN, and hidden because of where it is: indexed and found when
	// the exclusion is turned off, and absent from the default search. A
	// draft missing from the default answer only because it was not
	// indexed yet would pass the second half without testing anything.
	searcher := e.Knowledge()
	chart := e.Company().Org
	found := func(q knowledge.Query) bool {
		for _, hit := range searcher.Search(ctx, q).Hits {
			if hit.PageID == draft.ID {
				return true
			}
		}
		return false
	}
	waitUntil(t, 20*time.Second, "the draft to be indexed", func() bool {
		return found(knowledge.Query{
			Text: rotationWords, Org: chart, ExcludeAncestors: []string{},
		})
	})
	if found(knowledge.Query{Text: rotationWords, Org: chart}) {
		t.Fatal("an unreviewed draft is in the search every seat reads")
	}

	// A SECOND PASS FINDS IT. The seats' skills still converge — the pass
	// re-clusters the same rows every day — and the draft is already there.
	if again := promoter.Pass(ctx); len(again) != 0 {
		t.Fatalf("a second pass announced %d promotion(s) of a draft that "+
			"already exists", len(again))
	}
	// AND ASKED NO MODEL: the fleet's record of the convergence is read
	// first, so an unchanged cluster costs no auxiliary call.
	if n := model.calls.Load(); n != 1 {
		t.Errorf("model calls = %d after two passes over one convergence, want 1", n)
	}

	// MOVED OUT, IT IS AN ORDINARY PAGE: the review gesture, made the way
	// a lead's own assistant makes it, with nothing renamed.
	top := ""
	moved, err := e.PagesStore().SavePage(ctx,
		pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"},
		draft.ID, pages.Save{BaseVersion: draft.Version, ParentID: &top})
	if err != nil {
		t.Fatalf("move the draft out of its parent: %v", err)
	}
	if err := e.WaitCommitted(ctx, moved.Outcome.Position); err != nil {
		t.Fatalf("wait for the move to apply: %v", err)
	}
	waitUntil(t, 20*time.Second, "the published draft in the default search",
		func() bool { return found(knowledge.Query{Text: rotationWords, Org: chart}) })
}

// A NATIVE DRAFT A LEAD LABELS `rejected` IS NOT DRAFTED AGAIN, and the fleet
// records the rejection.
//
// The draft's own page tells the lead the gesture, the pass reads it on its
// next run, and after that neither the model nor the page is asked again: the
// seats' skills still converge, and the rejection is the fleet's.
func TestANativeDraftALeadLabelsRejectedIsNotDraftedAgain(t *testing.T) {
	t.Parallel()
	e, promoter, model := promotingEngine(t)
	ctx := t.Context()

	out := promoter.Pass(ctx)
	if len(out) != 1 {
		t.Fatalf("the pass announced %d promotion(s), want 1", len(out))
	}
	id := out[0].(types.SkillPromoted).PageID
	detail, err := e.Pages().Get(ctx, id, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("read the draft: %v", err)
	}
	gesture := "add the label `" + rejectedDraftLabel + "` to it"
	if !strings.Contains(detail.Page.Body, gesture) {
		t.Fatalf("the draft does not tell its reviewer %q:\n%s", gesture, detail.Page.Body)
	}

	labels := []string{rejectedDraftLabel}
	labelled, err := e.PagesStore().SavePage(ctx,
		pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"},
		id, pages.Save{BaseVersion: detail.Page.Version, Labels: &labels})
	if err != nil {
		t.Fatalf("label the draft rejected: %v", err)
	}
	if err := e.WaitCommitted(ctx, labelled.Outcome.Position); err != nil {
		t.Fatalf("wait for the label to apply: %v", err)
	}

	for pass := 2; pass <= 3; pass++ {
		if again := promoter.Pass(ctx); len(again) != 0 {
			t.Fatalf("pass %d announced %v after the lead rejected the draft", pass, again)
		}
	}
	if n := model.calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1: a rejected convergence was paid for again", n)
	}
	held, err := e.backends.Fleet.Promotions(ctx, "Platform")
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(held) != 1 || !strings.Contains(string(held[0].Value), `"state":"rejected"`) {
		t.Fatalf("the fleet holds %d record(s), want the one convergence rejected: %+v",
			len(held), held)
	}
}

// A NATIVE DRAFT IN THE TRASH OR PURGED IS REJECTED, and one that stands —
// under review or published — is not. The store can trash and purge a page
// although no page tool offers it, and a draft somebody put out of reach is
// not one they want drafted again.
func TestANativeDraftOutOfReachReadsAsRejected(t *testing.T) {
	t.Parallel()
	e, promoter, _ := promotingEngine(t)
	ctx := t.Context()
	out := promoter.Pass(ctx)
	if len(out) != 1 {
		t.Fatalf("the pass announced %d promotion(s), want 1", len(out))
	}
	id := out[0].(types.SkillPromoted).PageID
	writer := e.native.drafts
	operator := pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"}
	rejected := func(want string) {
		t.Helper()
		how, err := writer.Rejected(ctx, "ENG", id)
		if err != nil {
			t.Fatalf("Rejected: %v", err)
		}
		if how != want {
			t.Fatalf("the draft reads as rejected %q, want %q", how, want)
		}
	}
	rejected("")

	trashed, err := e.PagesStore().Trash(ctx, operator, id)
	if err != nil {
		t.Fatalf("trash the draft: %v", err)
	}
	if err := e.WaitCommitted(ctx, trashed.Outcome.Position); err != nil {
		t.Fatalf("wait for the trash to apply: %v", err)
	}
	rejected("it is in the trash")

	purged, err := e.PagesStore().Purge(ctx, operator, id, "rejected")
	if err != nil {
		t.Fatalf("purge the draft: %v", err)
	}
	if err := e.WaitCommitted(ctx, purged.Outcome.Position); err != nil {
		t.Fatalf("wait for the purge to apply: %v", err)
	}
	rejected("it was purged")
}

// THE NATIVE WRITER NAMES ITS KNOWLEDGE BASE AS ITS SEARCHER DOES. A record
// carries the name, and a draft recorded under another name would read as one
// in a knowledge base the company left, and be drafted again.
func TestTheNativeWriterNamesItsKnowledgeBaseAsItsSearcherDoes(t *testing.T) {
	t.Parallel()
	var searcher *pages.Searcher
	if got, want := (&nativeDrafts{}).Backend(), searcher.Backend(); got != want {
		t.Fatalf("the native writer calls its knowledge base %q, its searcher %q", got, want)
	}
}

// scriptedReads answers the writer's read by the freshness it is asked at, so
// a case can hold a page the log has and this node has not applied.
type scriptedReads struct {
	stale, linearizable func() (pages.Detail, error)
}

func (s scriptedReads) Get(_ context.Context, _ string, fresh statelog.Freshness) (pages.Detail, error) {
	if fresh.Level == statelog.ReadStale {
		return s.stale()
	}
	return s.linearizable()
}

// A DRAFT THIS NODE HAS NOT APPLIED IS NOT A PURGED ONE, and a read that
// failed is not a verdict. The draft may have been made on another node, so a
// miss in this node's own rows is asked of the log before it is called gone —
// and an error from either read is never a rejection, which the pass would
// record for good.
func TestANativeDraftThisNodeHasNotAppliedIsNotReadAsPurged(t *testing.T) {
	t.Parallel()
	missing := func() (pages.Detail, error) { return pages.Detail{}, pages.ErrNotFound }
	standing := func() (pages.Detail, error) {
		return pages.Detail{Page: pages.Page{ID: "p-1", Status: pages.StatusPublished}}, nil
	}
	outage := func() (pages.Detail, error) {
		return pages.Detail{}, errors.New("the log's barrier timed out")
	}
	for _, tc := range []struct {
		name    string
		reads   scriptedReads
		want    string
		wantErr bool
	}{
		{"applied elsewhere, standing in the log",
			scriptedReads{stale: missing, linearizable: standing}, "", false},
		{"missing here, the log unreachable",
			scriptedReads{stale: missing, linearizable: outage}, "", true},
		{"this node's rows unreadable", scriptedReads{stale: outage, linearizable: standing}, "", true},
		// THE CONTROL: missing from both is the one read that is a purge,
		// so the cases above are measuring the barrier and nothing else.
		{"missing from the log too",
			scriptedReads{stale: missing, linearizable: missing}, "it was purged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writer := &nativeDrafts{reader: tc.reads}
			how, err := writer.Rejected(t.Context(), "ENG", "p-1")
			if (err != nil) != tc.wantErr || how != tc.want {
				t.Fatalf("Rejected = %q, %v, want %q with an error %v", how, err,
					tc.want, tc.wantErr)
			}
		})
	}
}

// THE NATIVE WRITER REFUSES WHAT THE PAGES STORE WOULD, BEFORE ANYTHING IS
// RECORDED. The pass files a record before it makes the page, so a title or a
// body the store refuses would be a record offered to it on every tick — and
// a check that drifted from the store's own rule would pass a draft the store
// refuses. So the edge is measured against the store itself.
func TestTheNativeWriterRefusesWhatThePagesStoreWould(t *testing.T) {
	t.Parallel()
	e, _, _ := promotingEngine(t)
	writer := e.native.drafts
	for _, tc := range []struct {
		name, title, body string
		ok                bool
	}{
		{"a title at the cap", strings.Repeat("t", pages.MaxTitle), "b", true},
		{"a title past the cap", strings.Repeat("t", pages.MaxTitle+1), "b", false},
		{"a blank title", "   ", "b", false},
		{"a body at the cap", "at the body cap", strings.Repeat("b", pages.MaxBody), true},
		{"a body past the cap", "past the body cap", strings.Repeat("b", pages.MaxBody+1), false},
	} {
		checked := writer.CheckDraft(tc.title, tc.body)
		if (checked == nil) != tc.ok {
			t.Errorf("%s: CheckDraft = %v, want accepted %v", tc.name, checked, tc.ok)
		}
		_, created := e.PagesStore().Create(t.Context(), draftAuthor, pages.NewPage{
			Container: "CHECK", Title: tc.title, Body: tc.body, Quiet: true,
		})
		if (created == nil) != (checked == nil) {
			t.Errorf("%s: the store answered %v where CheckDraft answered %v",
				tc.name, created, checked)
		}
	}
}

// AN OPERATOR'S PAGE IS WATCHED BY NO SEAT, AND NAMES NO SEAT AS ITS AUTHOR.
//
// A seat's handle on a create becomes the page's watcher and its recorded
// author, and the change feed wakes nobody whose handle equals the author. A
// token named like a seat — `dev` here, which the company's seat holds — must
// therefore reach the store with no handle at all, from the operator's own
// assistant and from the engine's own operator gestures alike.
func TestAnOperatorsPageIsWatchedByNoSeat(t *testing.T) {
	t.Parallel()
	e, _, _ := promotingEngine(t)
	ctx := t.Context()
	surface, err := opsmcp.PageActor(auth.WithOperator(ctx, "dev"), nil)
	if err != nil {
		t.Fatalf("PageActor: %v", err)
	}
	for name, actor := range map[string]pages.Actor{
		"the operator's assistant": surface,
		"an operator gesture":      operatorPageActor("dev"),
	} {
		written, err := e.PagesStore().Create(ctx, actor, pages.NewPage{
			Container: "ENG", Title: "Written by " + name, Body: "b",
		})
		if err != nil {
			t.Fatalf("%s: Create: %v", name, err)
		}
		if err := e.WaitCommitted(ctx, written.Outcome.Position); err != nil {
			t.Fatalf("%s: wait for the create to apply: %v", name, err)
		}
		detail, err := e.Pages().Get(ctx, written.Page.ID,
			statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			t.Fatalf("%s: read the page: %v", name, err)
		}
		if slices.Contains(detail.Page.Watchers, "dev") {
			t.Errorf("%s: the seat dev watches a page it never touched: %v",
				name, detail.Page.Watchers)
		}
		if detail.Page.Author != "operator:dev" {
			t.Errorf("%s: the page is authored by %q, want operator:dev", name,
				detail.Page.Author)
		}
	}
}
