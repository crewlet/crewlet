package engine

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
)

// draftingProvider answers every auxiliary call with one promotion draft.
type draftingProvider struct{ answer string }

func (draftingProvider) Model() string { return "draft-test" }

func (p draftingProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
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
// pass the engine would arm, over a model that answers rotationDraft.
func promotingEngine(t *testing.T) (*Engine, *learning.Promoter) {
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

	// THE ENGINE'S OWN RESOLVER AND ROSTER, and only the model replaced:
	// the wiring under test is which knowledge base the pass is handed and
	// which container each unit files in.
	promoter, err := learning.NewPromoter(learning.PromoterOptions{
		Writer:      e.promotionWriter,
		Skills:      learning.NewSkills(e.backends.Store),
		Models:      staticModels{provider: draftingProvider{answer: rotationDraft}},
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
	return e, promoter
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
	e, promoter := promotingEngine(t)
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
