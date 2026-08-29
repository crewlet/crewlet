package learning_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT PROMOTION IS FOR.
//
// Every other skill here is agent-scope: one seat's row, in one seat's
// catalogue, in one seat's prompt. A procedure four seats independently
// arrived at is something the TEAM has, which makes it documentation — so the
// output is a draft page a unit lead reviews rather than a skill row nobody
// asked for. `skill_promotion.enabled`, `min_sibling_count`,
// `jaccard_threshold` and `budget_tokens` all validated and were read by
// nothing, and `Skills.ListFor` existed for this pass with no caller.

// fakeWriter records the drafts a pass asked for.
type fakeWriter struct {
	mu      sync.Mutex
	drafts  []draftCall
	err     error
	existed bool
}

type draftCall struct{ container, name, body string }

func (w *fakeWriter) CreateDraft(_ context.Context, container, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drafts = append(w.drafts, draftCall{container, name, markdown})
	if w.err != nil {
		return knowledge.DraftPage{}, false, w.err
	}
	return knowledge.DraftPage{ID: "page-1", Title: name}, !w.existed, nil
}

func (w *fakeWriter) calls() []draftCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]draftCall(nil), w.drafts...)
}

// promoter builds a pass over one unit.
func promoter(t *testing.T, db *store.DB, w *fakeWriter, answer string,
	unit learning.PromotionUnit, opts learning.PromoterOptions,
) (*learning.Promoter, *auxProvider) {
	t.Helper()
	p := &auxProvider{replies: []llm.Completion{{Content: answer}}}
	opts.Writer, opts.Skills, opts.Models = w, learning.NewSkills(db), &stubModels{p: p}
	opts.Units = func() []learning.PromotionUnit { return []learning.PromotionUnit{unit} }
	built, err := learning.NewPromoter(opts)
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	return built, p
}

// unitOf is a unit with a container and the given seats.
func unitOf(handles ...string) learning.PromotionUnit {
	return learning.PromotionUnit{
		ID: "Platform", Lead: &org.Role{Name: "Lead"},
		Handles: handles, Container: "ENG",
	}
}

// seedSibling gives one seat a skill over the given tool run.
func seedSibling(t *testing.T, db *store.DB, handle, name string, tools ...string) {
	t.Helper()
	now := time.Now().UTC()
	err := learning.NewSkills(db).Insert(t.Context(), learning.Skill{
		ID: handle + "/" + name, AgentHandle: handle, Name: name,
		Description:  "how " + handle + " does it",
		Content:      "1. fetch\n2. build\n3. tag",
		ToolSequence: tools, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

const promotionDraft = `{"name":"cut-a-release","description":"Ship a tagged release",` +
	`"content":"1. run the pipeline\n2. tag\n3. announce"}`

// THREE SEATS CONVERGING BECOMES A DRAFT — the pass whose absence made every
// skill_promotion knob inert.
func TestThreeSeatsConvergingProduceAReviewableDraft(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	p, _ := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want the SkillPromoted event", len(out))
	}
	ev, ok := out[0].(types.SkillPromoted)
	if !ok {
		t.Fatalf("payload = %T", out[0])
	}
	if ev.UnitID != "Platform" || ev.ContainerKey != "ENG" || ev.PageID != "page-1" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.DistinctAgents != 3 || ev.SiblingCount != 3 {
		t.Fatalf("DistinctAgents = %d, SiblingCount = %d, want 3 and 3",
			ev.DistinctAgents, ev.SiblingCount)
	}

	calls := w.calls()
	if len(calls) != 1 {
		t.Fatalf("draft calls = %d, want 1", len(calls))
	}
	if !strings.HasPrefix(calls[0].name, knowledge.AutoDraftTitlePrefix) {
		t.Fatalf("title = %q — without the auto-draft prefix the knowledge "+
			"search cannot hide it, and every agent reads an unreviewed page",
			calls[0].name)
	}
	// The provenance is in the PAGE, not only in the event: a reviewer is
	// not reading the event feed.
	for _, want := range []string{"dev", "sre", "qa", "Auto-drafted"} {
		if !strings.Contains(calls[0].body, want) {
			t.Fatalf("the draft omits %q:\n%s", want, calls[0].body)
		}
	}
}

// DISTINCT SEATS ARE WHAT COUNTS, not skills. One seat with four
// near-identical skills is a catalogue that needs curating, and promoting it
// would present one agent's habit as the team's practice.
func TestOneSeatRepeatingItselfIsNotAConvergence(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		seedSibling(t, db, "dev", "release-"+name, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	p, aux := promoter(t, db, w, promotionDraft,
		unitOf("dev", "sre", "qa"), learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v — four skills owned by ONE seat were promoted "+
			"as a team convergence", out)
	}
	if len(w.calls()) != 0 || aux.calls != 0 {
		t.Fatalf("drafts = %d, model calls = %d, want none",
			len(w.calls()), aux.calls)
	}
}

// A UNIT WITH NOWHERE TO FILE IS SOFT-SKIPPED. A company that configured
// knowledge for one team and not another is supported, and failing here would
// stop the configured team's promotions too.
func TestAUnitWithNoContainerIsSkippedNotFailed(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	unconfigured := unitOf("dev", "sre", "qa")
	unconfigured.Container, unconfigured.Hint = "", "set confluence_space on it"

	w := &fakeWriter{}
	p, aux := promoter(t, db, w, promotionDraft, unconfigured,
		learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v, want none for a unit with no container", out)
	}
	if aux.calls != 0 {
		t.Fatalf("model calls = %d — a unit with nowhere to file paid for a draft",
			aux.calls)
	}
}

// A UNIT SMALLER THAN THE THRESHOLD PROMOTES NOTHING. Its seats cannot reach
// the count however perfectly they converge.
//
// The pass ALSO short-circuits before reading the catalogue, which this case
// deliberately does not assert: that early exit is a saved query per unit per
// tick, not a behaviour, and a test that pinned it would pin an optimization
// rather than the rule.
func TestAUnitTooSmallToConvergeCostsNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	seedSibling(t, db, "dev", "release", "fetch", "build", "tag")
	seedSibling(t, db, "sre", "release", "fetch", "build", "tag")
	w := &fakeWriter{}
	p, aux := promoter(t, db, w, promotionDraft, unitOf("dev", "sre"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v — a 2-seat unit promoted under a min of 3", out)
	}
	if len(w.calls()) != 0 || aux.calls != 0 {
		t.Fatalf("drafts = %d, model calls = %d, want none", len(w.calls()), aux.calls)
	}
}

// AN ALREADY-DRAFTED PROMOTION IS NOT RE-ANNOUNCED. The pass re-clusters the
// same rows every tick, so without this the feed carries the same promotion
// every day for the life of the company.
func TestAnAlreadyDraftedPromotionIsQuiet(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{existed: true}
	p, _ := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v — a draft that already existed was announced again", out)
	}
	if len(w.calls()) != 1 {
		t.Fatalf("draft calls = %d — the writer is what dedups, so it must "+
			"still be asked", len(w.calls()))
	}
}

// A FAILED WRITE ANNOUNCES NOTHING AND DOES NOT STOP THE OTHER UNITS. The
// next tick re-clusters the same rows and tries again — that is the retry.
func TestAFailedDraftCostsOneUnitNotThePass(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	for _, h := range []string{"ops", "net", "sec"} {
		seedSibling(t, db, h, "triage-"+h, "page", "diagnose", "mitigate")
	}
	broken := unitOf("dev", "sre", "qa")
	working := learning.PromotionUnit{
		ID: "Infra", Lead: &org.Role{Name: "Lead"},
		Handles: []string{"ops", "net", "sec"}, Container: "OPS",
	}

	failing := &fakeWriter{err: errors.New("the wiki is down")}
	fine := &fakeWriter{}
	p, err := learning.NewPromoter(learning.PromoterOptions{
		Writer: splitWriter{broken: failing, ok: fine},
		Skills: learning.NewSkills(db),
		Models: &stubModels{p: &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}},
		Units: func() []learning.PromotionUnit {
			return []learning.PromotionUnit{broken, working}
		},
		MinSiblings: 3,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("payloads = %d — a unit whose write failed took the next one "+
			"down with it", len(out))
	}
	if got := out[0].(types.SkillPromoted).UnitID; got != "Infra" {
		t.Fatalf("promoted %q, want the unit whose write succeeded", got)
	}
}

// splitWriter fails for one container and succeeds for the other.
type splitWriter struct{ broken, ok *fakeWriter }

func (s splitWriter) CreateDraft(ctx context.Context, container, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	if container == "ENG" {
		return s.broken.CreateDraft(ctx, container, name, markdown)
	}
	return s.ok.CreateDraft(ctx, container, name, markdown)
}

// UNRELATED PROCEDURES DO NOT POOL. Without the threshold every skill in the
// unit would be one cluster, and the "shared practice" drafted from it would
// be the team's job description.
func TestUnrelatedSkillsDoNotPoolIntoOneConvergence(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	seedSibling(t, db, "dev", "release", "fetch", "build", "tag")
	seedSibling(t, db, "sre", "triage", "page", "diagnose", "mitigate")
	seedSibling(t, db, "qa", "report", "query", "chart", "share")
	w := &fakeWriter{}
	p, aux := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v — three unrelated skills were promoted as a "+
			"shared practice", out)
	}
	if aux.calls != 0 {
		t.Fatalf("model calls = %d, want none", aux.calls)
	}
}

// THE STRONGEST CONVERGENCE GOES FIRST, one per unit per tick.
func TestTheWidestConvergenceIsPromotedFirst(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa", "ops"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "triage-"+h, "page", "diagnose", "mitigate")
	}
	w := &fakeWriter{}
	p, aux := promoter(t, db, w, promotionDraft,
		unitOf("dev", "sre", "qa", "ops"), learning.PromoterOptions{MinSiblings: 3})

	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want exactly one promotion per unit per tick", len(out))
	}
	if got := out[0].(types.SkillPromoted).DistinctAgents; got != 4 {
		t.Fatalf("DistinctAgents = %d, want the 4-seat convergence", got)
	}
	if aux.calls != 1 {
		t.Fatalf("model calls = %d — a tick drafted more than one cluster", aux.calls)
	}
	// And the prompt was about the run four seats shared.
	prompt := aux.seen[0].Messages[len(aux.seen[0].Messages)-1].Content
	if !strings.Contains(prompt, "fetch -> build -> tag -> announce") {
		t.Fatalf("the prompt is about the wrong convergence:\n%s", prompt)
	}
}

// A PROMOTER MISSING ANY HALF REFUSES TO BE BUILT rather than silently
// promoting nothing for the life of the process.
func TestAPromoterNeedsEveryHalf(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	full := learning.PromoterOptions{
		Writer: &fakeWriter{}, Skills: learning.NewSkills(db),
		Models: &stubModels{p: &auxProvider{}},
		Units:  func() []learning.PromotionUnit { return nil },
	}
	for _, missing := range []struct {
		name string
		drop func(o *learning.PromoterOptions)
	}{
		{"writer", func(o *learning.PromoterOptions) { o.Writer = nil }},
		{"skills", func(o *learning.PromoterOptions) { o.Skills = nil }},
		{"models", func(o *learning.PromoterOptions) { o.Models = nil }},
		{"units", func(o *learning.PromoterOptions) { o.Units = nil }},
	} {
		opts := full
		missing.drop(&opts)
		if _, err := learning.NewPromoter(opts); err == nil {
			t.Errorf("NewPromoter accepted a promoter with no %s", missing.name)
		}
	}
}

// A DECLINE IS NOT AN ERROR: similar tools do not always mean the same work.
func TestAPromotionTheModelDeclinesDraftsNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	p, _ := promoter(t, db, w, "{}", unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v, want none", out)
	}
	if len(w.calls()) != 0 {
		t.Fatalf("drafts = %d — a declined promotion still wrote a page", len(w.calls()))
	}
}
