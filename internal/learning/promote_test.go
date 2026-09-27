package learning_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
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
// asked for. And a convergence is drafted ONCE: the pass re-clusters the same
// skills every tick, and what it drafted, what a lead rejected and what the
// model declined are the fleet's ledger, read before any model is asked.

// fakeWriter records the drafts a pass asked for, holding each title once as
// a knowledge base does, and answers for rejections a test marks.
type fakeWriter struct {
	mu       sync.Mutex
	backend  string
	drafts   []draftCall
	pages    map[string]string // title → page id
	rejected map[string]string // page id → how
	asked    []string          // page ids Rejected was asked about
	err      error
	existed  bool
}

type draftCall struct{ container, name, body string }

func (w *fakeWriter) Backend() string {
	if w.backend == "" {
		return "fake"
	}
	return w.backend
}

func (w *fakeWriter) Rejection() string { return "throw it in the fake bin" }

func (w *fakeWriter) CreateDraft(_ context.Context, container, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drafts = append(w.drafts, draftCall{container, name, markdown})
	if w.err != nil {
		return knowledge.DraftPage{}, false, w.err
	}
	if w.pages == nil {
		w.pages = map[string]string{}
	}
	if id, held := w.pages[name]; held {
		return knowledge.DraftPage{ID: id, Title: name}, false, nil
	}
	id := fmt.Sprintf("page-%d", len(w.pages)+1)
	w.pages[name] = id
	return knowledge.DraftPage{ID: id, Title: name}, !w.existed, nil
}

func (w *fakeWriter) Rejected(_ context.Context, _, pageID string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, pageID)
	return w.rejected[pageID], nil
}

// reject marks a page rejected, the way a lead's gesture would.
func (w *fakeWriter) reject(pageID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rejected == nil {
		w.rejected = map[string]string{}
	}
	w.rejected[pageID] = "it was thrown in the bin"
}

func (w *fakeWriter) calls() []draftCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]draftCall(nil), w.drafts...)
}

func (w *fakeWriter) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *fakeWriter) askedAbout() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.asked...)
}

// promoter builds a pass over one unit, with a fresh fleet ledger.
func promoter(t *testing.T, db *store.DB, w *fakeWriter, answer string,
	unit learning.PromotionUnit, opts learning.PromoterOptions,
) (*learning.Promoter, *auxProvider) {
	t.Helper()
	p := &auxProvider{replies: []llm.Completion{{Content: answer}}}
	return promoterOver(t, db, w, p, memory.NewFleet(), unit, opts), p
}

// promoterOver builds a pass over one unit against a given model and ledger.
func promoterOver(t *testing.T, db *store.DB, w learning.PromotionWriter,
	model llm.Provider, ledger learning.PromotionLedger,
	unit learning.PromotionUnit, opts learning.PromoterOptions,
) *learning.Promoter {
	t.Helper()
	opts.Writer = func() (learning.PromotionWriter, string) { return w, "" }
	opts.Ledger = ledger
	opts.Skills, opts.Models = learning.NewSkills(db), &stubModels{p: model}
	opts.Units = func() []learning.PromotionUnit { return []learning.PromotionUnit{unit} }
	built, err := learning.NewPromoter(opts)
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	return built
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
	seedSiblingAt(t, db, time.Now().UTC(), handle, name, tools...)
}

// seedSiblingAt gives one seat a skill written at a given instant.
func seedSiblingAt(t *testing.T, db *store.DB, at time.Time, handle, name string, tools ...string) {
	t.Helper()
	err := learning.NewSkills(db).Insert(t.Context(), learning.Skill{
		ID: handle + "/" + name, AgentHandle: handle, Name: name,
		Description:  "how " + handle + " does it",
		Content:      "1. fetch\n2. build\n3. tag",
		ToolSequence: tools, CreatedAt: at, UpdatedAt: at,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

// records reads a unit's ledger back as the states and fields it holds.
func records(t *testing.T, ledger learning.PromotionLedger, unit string) []map[string]any {
	t.Helper()
	held, err := ledger.Promotions(t.Context(), unit)
	if err != nil {
		t.Fatalf("Promotions(%s): %v", unit, err)
	}
	out := make([]map[string]any, 0, len(held))
	for _, rec := range held {
		var fields map[string]any
		if err := json.Unmarshal(rec.Value, &fields); err != nil {
			t.Fatalf("record %s does not decode: %v", rec.Fingerprint, err)
		}
		out = append(out, fields)
	}
	return out
}

const promotionDraft = `{"name":"cut-a-release","description":"Ship a tagged release",` +
	`"content":"1. run the pipeline\n2. tag\n3. announce"}`

// renamedDraft is the same procedure under another name — what a model at a
// non-zero temperature answers on another day.
const renamedDraft = `{"name":"ship-a-tagged-release","description":"Ship a tagged release",` +
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
	if ev.UnitID != "Platform" || ev.ContainerKey != "ENG" || ev.PageID != "page-1" ||
		ev.SkillName != "cut-a-release" {
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

// THE PAGE TELLS ITS REVIEWER HOW TO ADOPT IT AND HOW TO REJECT IT, in the
// knowledge base's own gesture: the page is the only place a lead is told,
// and the gesture is the writer's, because only the writer's check recognises
// it.
func TestTheDraftTellsItsReviewerHowToAdoptAndRejectIt(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	p, _ := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})
	p.Pass(t.Context())
	calls := w.calls()
	if len(calls) != 1 {
		t.Fatalf("draft calls = %d, want 1", len(calls))
	}
	for _, want := range []string{
		"move it out from under \"" + knowledge.AutoDraftedParent + "\"",
		"**To reject it**, " + w.Rejection() + ".",
		"does not draft this procedure for Platform",
	} {
		if !strings.Contains(calls[0].body, want) {
			t.Errorf("the draft does not say %q:\n%s", want, calls[0].body)
		}
	}
}

// A SECOND PASS OVER AN UNCHANGED CLUSTER ASKS NO MODEL AND DRAFTS NOTHING —
// even when the model would have named the procedure differently, which at a
// non-zero temperature it does.
//
// The name is the model's, so a dedup keyed on it would pay for a call every
// tick for every converged unit and turn a renamed answer into a second page.
func TestASecondPassOverAnUnchangedClusterAsksNoModelAndDraftsNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{
		{Content: promotionDraft}, {Content: renamedDraft},
	}}
	p := promoterOver(t, db, w, model, memory.NewFleet(), unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 1 {
		t.Fatalf("the first pass announced %d, want 1", len(out))
	}
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("the second pass announced %v", out)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want the first pass's 1 — the second paid for "+
			"a procedure already drafted", model.calls)
	}
	if calls := w.calls(); len(calls) != 1 {
		t.Errorf("draft calls = %d, want 1: %+v", len(calls), calls)
	}
}

// A REJECTED DRAFT STAYS REJECTED. The seats' skills still converge — the
// pass re-clusters them every day — and a lead's rejection is read once,
// recorded for the fleet, and never re-asked.
func TestARejectedDraftIsNotDraftedAgain(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	ledger := memory.NewFleet()
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, ledger, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("the first pass announced %d, want 1", len(out))
	}
	w.reject(out[0].(types.SkillPromoted).PageID)

	for pass := 2; pass <= 3; pass++ {
		if out := p.Pass(t.Context()); len(out) != 0 {
			t.Fatalf("pass %d announced %v after the lead rejected the draft", pass, out)
		}
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1: a rejected convergence was paid for again",
			model.calls)
	}
	if calls := w.calls(); len(calls) != 1 {
		t.Errorf("draft calls = %d, want 1 — the rejected draft came back", len(calls))
	}
	held := records(t, ledger, "Platform")
	if len(held) != 1 || held[0]["state"] != "rejected" {
		t.Fatalf("the ledger holds %v, want the one convergence, rejected", held)
	}
	// RECORDED ONCE: a pass that finds a rejection in the ledger does not
	// ask the knowledge base about the page again.
	if asked := w.askedAbout(); len(asked) != 1 {
		t.Errorf("the knowledge base was asked about the draft %d time(s), want "+
			"once — the pass after the rejection was recorded asked again", len(asked))
	}
}

// A REJECTION HOLDS WHEN A NEWER SKILL WOULD HAVE TAKEN THE CLUSTER OVER.
//
// A record is matched to a cluster by its leading tool run. Pooled newest
// first, a seat's newest skill leads, and one far enough from the rejected
// run takes over the colleagues nearest it — so the same seats' convergence
// comes back under a leader the rejection does not match. Pooled oldest first,
// the leader is the first skill written of the procedure and a newcomer joins
// it or stands alone.
func TestARejectionHoldsWhenANewerSkillWouldHaveTakenTheClusterOver(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	then := time.Now().UTC().Add(-time.Hour)
	seedSiblingAt(t, db, then, "dev", "release-dev", "a", "b", "c", "d", "e")
	seedSiblingAt(t, db, then.Add(time.Minute), "sre", "release-sre", "a", "b", "c", "d", "e")
	seedSiblingAt(t, db, then.Add(2*time.Minute), "qa", "release-qa", "a", "b", "c", "d", "x")
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}, {Content: renamedDraft}}}
	p := promoterOver(t, db, w, model, memory.NewFleet(), unitOf("dev", "sre", "qa", "ops"),
		learning.PromoterOptions{MinSiblings: 3})

	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("the first pass announced %d, want 1", len(out))
	}
	w.reject(out[0].(types.SkillPromoted).PageID)
	p.Pass(t.Context())

	// A NEWCOMER close to the first two seats and far from the third, whose
	// skill is the newest of the three: led by whichever skill is newest,
	// the pool follows the newcomer away from the run the lead rejected.
	seedSiblingAt(t, db, time.Now().UTC(), "ops", "release-ops", "a", "b", "c", "e", "y")
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("the rejected convergence came back as %v once a newer skill "+
			"joined the team", out)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1", model.calls)
	}
}

// THE LEDGER IS READ BEFORE ANY MODEL CALL, and one that cannot be read drafts
// nothing: "no record" is the answer that pays to draft what a lead rejected.
func TestALedgerThatCannotBeReadDraftsNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, unreadableLedger{memory.NewFleet()},
		unitOf("dev", "sre", "qa"), learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v with no ledger to read", out)
	}
	if model.calls != 0 || len(w.calls()) != 0 {
		t.Errorf("model calls = %d, drafts = %d, want none", model.calls, len(w.calls()))
	}
}

// unreadableLedger answers every listing with an outage.
type unreadableLedger struct{ learning.PromotionLedger }

func (unreadableLedger) Promotions(context.Context, string) ([]coord.PromotionRecord, error) {
	return nil, coord.ErrUnavailable
}

// A RECORD THAT DOES NOT DECODE STOPS ITS UNIT: skipped, it would be a
// convergence with no record — and it may be a lead's rejection.
func TestARecordThatDoesNotDecodeDraftsNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	ledger := memory.NewFleet()
	if _, _, err := ledger.CreatePromotion(t.Context(), coord.PromotionRecord{
		Unit: "Platform", Fingerprint: "garbled", Value: []byte("not json"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, ledger, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 || model.calls != 0 {
		t.Fatalf("payloads = %v, model calls = %d past a record it could not read",
			out, model.calls)
	}
}

// A DRAFT WHOSE PAGE COULD NOT BE MADE IS MADE FROM ITS RECORD, with no model
// call: the record is filed before the page, so a failure between the two
// costs the page one tick and nothing else.
func TestADraftWhoseCreateFailedIsMadeFromItsRecordWithoutAModel(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	w.fail(errors.New("the wiki is down"))
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}, {Content: renamedDraft}}}
	p := promoterOver(t, db, w, model, memory.NewFleet(), unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("a pass whose page was refused announced %v", out)
	}
	w.fail(nil)
	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("the retry announced %d, want the draft it made", len(out))
	}
	if got := out[0].(types.SkillPromoted).SkillName; got != "cut-a-release" {
		t.Errorf("the retry drafted %q, want the name the first answer gave", got)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — the retry paid for an answer it held", model.calls)
	}
}

// A PAGE MADE AND NOT RECORDED IS RECORDED ON THE NEXT PASS, not made again:
// the writer finds it by the title the record holds.
func TestAPageMadeButNotRecordedIsRecordedNotMadeAgain(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	ledger := &flakyUpdates{PromotionLedger: memory.NewFleet(), fail: 1}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}, {Content: renamedDraft}}}
	p := promoterOver(t, db, w, model, ledger, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 1 {
		t.Fatalf("the first pass announced %d, want the page it made", len(out))
	}
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("the second pass announced %v for a page already made", out)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1", model.calls)
	}
	if n := len(w.pages); n != 1 {
		t.Errorf("%d pages exist, want the one", n)
	}
	held := records(t, ledger, "Platform")
	if len(held) != 1 || held[0]["state"] != "drafted" || held[0]["page_id"] != "page-1" {
		t.Errorf("the ledger holds %v, want the page recorded as drafted", held)
	}
	if _, has := held[0]["body"]; has {
		t.Error("the recorded draft still carries its body, which the page now holds")
	}
}

// flakyUpdates refuses its first `fail` updates as an outage.
type flakyUpdates struct {
	learning.PromotionLedger
	mu   sync.Mutex
	fail int
}

func (f *flakyUpdates) UpdatePromotion(ctx context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error) {
	f.mu.Lock()
	if f.fail > 0 {
		f.fail--
		f.mu.Unlock()
		return coord.PromotionRecord{}, false, coord.ErrUnavailable
	}
	f.mu.Unlock()
	return f.PromotionLedger.UpdatePromotion(ctx, rec)
}

// A HOLDER OF THE DUTY THAT LOST THE RACE TO FILE A CONVERGENCE MAKES NO PAGE:
// the other holder filed it and makes it.
func TestAFilingLostToAnotherHolderMakesNoPage(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, lostCreates{memory.NewFleet()},
		unitOf("dev", "sre", "qa"), learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v after losing the filing", out)
	}
	if calls := w.calls(); len(calls) != 0 {
		t.Errorf("a pass that lost the filing made %d page(s)", len(calls))
	}
}

// lostCreates answers every create as somebody else's.
type lostCreates struct{ learning.PromotionLedger }

func (lostCreates) CreatePromotion(context.Context, coord.PromotionRecord) (coord.PromotionRecord, bool, error) {
	return coord.PromotionRecord{}, false, nil
}

// A DECLINE IS PAID FOR ONCE, UNTIL MORE SEATS CONVERGE: the model answered
// the evidence it was shown, and the same evidence buys the same answer.
func TestADeclineIsNotPaidForAgainUntilMoreSeatsConverge(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: "{}"}, {Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, memory.NewFleet(), unitOf("dev", "sre", "qa", "ops"),
		learning.PromoterOptions{MinSiblings: 3})

	p.Pass(t.Context())
	p.Pass(t.Context())
	if model.calls != 1 {
		t.Fatalf("model calls = %d after two passes over a declined convergence, want 1",
			model.calls)
	}
	seedSibling(t, db, "ops", "release-ops", "fetch", "build", "tag", "announce")
	if out := p.Pass(t.Context()); len(out) != 1 {
		t.Fatalf("a fourth seat converged and the pass announced %d, want the draft "+
			"the model was asked for again", len(out))
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2", model.calls)
	}
}

// TWO CONVERGENCES ARE DRAFTED ON SUCCESSIVE TICKS, the wider first. One per
// unit per tick, and a drafted one is walked past rather than chosen again.
func TestTwoConvergencesAreDraftedOnSuccessiveTicks(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa", "ops"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "triage-"+h, "page", "diagnose", "mitigate")
	}
	w := &fakeWriter{}
	triage := `{"name":"triage-an-alert","description":"Work an alert",` +
		`"content":"1. page\n2. diagnose\n3. mitigate"}`
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}, {Content: triage}}}
	p := promoterOver(t, db, w, model, memory.NewFleet(), unitOf("dev", "sre", "qa", "ops"),
		learning.PromoterOptions{MinSiblings: 3})

	var agents []int
	for pass := 1; pass <= 3; pass++ {
		for _, payload := range p.Pass(t.Context()) {
			agents = append(agents, payload.(types.SkillPromoted).DistinctAgents)
		}
	}
	if len(agents) != 2 || agents[0] != 4 || agents[1] != 3 {
		t.Fatalf("three passes promoted convergences of %v seat(s), want [4 3]", agents)
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2", model.calls)
	}
}

// A DRAFT IN A KNOWLEDGE BASE THE COMPANY HAS LEFT IS DRAFTED AGAIN WHERE IT
// DRAFTS NOW — and a REJECTION is not: it is a decision about the procedure,
// wherever the page was.
func TestADraftFollowsTheKnowledgeBaseAndARejectionDoesNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		reject     bool
		wantDrafts int
	}{
		{"a standing draft is drafted again", false, 1},
		{"a rejection holds", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newStore(t)
			for _, h := range []string{"dev", "sre", "qa"} {
				seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
			}
			ledger := memory.NewFleet()
			model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
			wiki := &fakeWriter{backend: "confluence"}
			first := promoterOver(t, db, wiki, model, ledger, unitOf("dev", "sre", "qa"),
				learning.PromoterOptions{MinSiblings: 3})
			out := first.Pass(t.Context())
			if len(out) != 1 {
				t.Fatalf("the first pass announced %d", len(out))
			}
			if tc.reject {
				wiki.reject(out[0].(types.SkillPromoted).PageID)
				first.Pass(t.Context())
			}

			native := &fakeWriter{backend: "native"}
			moved := promoterOver(t, db, native, model, ledger, unitOf("dev", "sre", "qa"),
				learning.PromoterOptions{MinSiblings: 3})
			moved.Pass(t.Context())
			moved.Pass(t.Context())
			if got := len(native.calls()); got != tc.wantDrafts {
				t.Errorf("the knowledge base the company moved to got %d draft(s), want %d",
					got, tc.wantDrafts)
			}
		})
	}
}

// A FIELD A NEWER BUILD WROTE IS CARRIED BACK when this build moves the record
// on: the ledger is shared by every build in a rolling upgrade.
func TestARecordKeepsWhatANewerBuildWroteWhenItIsRejected(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	ledger := memory.NewFleet()
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, ledger, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})
	out := p.Pass(t.Context())
	if len(out) != 1 {
		t.Fatalf("the first pass announced %d", len(out))
	}

	// A newer build adds a field to the record.
	held, err := ledger.Promotions(t.Context(), "Platform")
	if err != nil || len(held) != 1 {
		t.Fatalf("Promotions = %v, %v", held, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(held[0].Value, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fields["reviewed_by"] = json.RawMessage(`"lead"`)
	held[0].Value, _ = json.Marshal(fields)
	if _, ok, err := ledger.UpdatePromotion(t.Context(), held[0]); err != nil || !ok {
		t.Fatalf("the newer build's write = %v, %v", ok, err)
	}

	w.reject(out[0].(types.SkillPromoted).PageID)
	p.Pass(t.Context())
	got := records(t, ledger, "Platform")
	if len(got) != 1 || got[0]["state"] != "rejected" {
		t.Fatalf("the ledger holds %v, want the rejection", got)
	}
	if got[0]["reviewed_by"] != "lead" {
		t.Errorf("recording the rejection erased a newer build's field: %v", got[0])
	}
}

// A RECORD FROM A NEWER BUILD STANDS FOR ITS CONVERGENCE AND IS NEVER WRITTEN:
// this build cannot know what its state means, and drafting over it would
// erase what the newer build decided.
func TestARecordFromANewerBuildIsNeitherDraftedOverNorWritten(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	ledger := memory.NewFleet()
	newer := `{"v":2,"state":"escalated","tools":["announce","build","fetch","tag"],"agents":3}`
	stored, _, err := ledger.CreatePromotion(t.Context(), coord.PromotionRecord{
		Unit: "Platform", Fingerprint: "from-a-newer-build", Value: []byte(newer),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, ledger, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 || model.calls != 0 {
		t.Fatalf("payloads = %v, model calls = %d over a newer build's record",
			out, model.calls)
	}
	held, _ := ledger.Promotions(t.Context(), "Platform")
	if len(held) != 1 || held[0].Version != stored.Version || string(held[0].Value) != newer {
		t.Errorf("the newer build's record was rewritten: %+v", held)
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

// THE MODEL READS EVERY SEAT BEFORE ANY SEAT'S SECOND SKILL: the question is
// what the seats agree on, and a prompt filled by one seat's versions leaves
// out the colleagues whose agreement is the evidence.
func TestThePromptShowsEverySeatBeforeAnySeatsSecondSkill(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	then := time.Now().UTC().Add(-time.Hour)
	for i, name := range []string{"one", "two", "three"} {
		seedSiblingAt(t, db, then.Add(time.Duration(i)*time.Minute),
			"dev", "release-"+name, "fetch", "build", "tag", "announce")
	}
	seedSibling(t, db, "sre", "release-sre", "fetch", "build", "tag", "announce")
	seedSibling(t, db, "qa", "release-qa", "fetch", "build", "tag", "announce")
	w := &fakeWriter{}
	p, aux := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})
	p.Pass(t.Context())
	if aux.calls != 1 {
		t.Fatalf("model calls = %d, want 1", aux.calls)
	}
	prompt := aux.seen[0].Messages[len(aux.seen[0].Messages)-1].Content
	for _, want := range []string{"(by dev)", "(by sre)", "(by qa)", "(and 1 more, all similar)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
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
	unconfigured.Container, unconfigured.Hint = "", "set `space` on it"

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

// A PAGE ALREADY HOLDING THE TITLE IS NOT ANNOUNCED. The pass announces only a
// page it made; one the knowledge base already held under the title is an
// earlier attempt's or somebody else's.
func TestAPageAlreadyHoldingTheTitleIsNotAnnounced(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}
	w := &fakeWriter{existed: true}
	p, _ := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
		learning.PromoterOptions{MinSiblings: 3})

	if out := p.Pass(t.Context()); len(out) != 0 {
		t.Fatalf("payloads = %v — a page that already existed was announced", out)
	}
	if len(w.calls()) != 1 {
		t.Fatalf("draft calls = %d, want 1", len(w.calls()))
	}
}

// A FAILED WRITE ANNOUNCES NOTHING AND DOES NOT STOP THE OTHER UNITS. The
// next tick makes the page from the record — that is the retry.
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
		Writer: func() (learning.PromotionWriter, string) {
			return splitWriter{broken: failing, ok: fine}, ""
		},
		Ledger: memory.NewFleet(),
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

func (s splitWriter) Backend() string   { return s.ok.Backend() }
func (s splitWriter) Rejection() string { return s.ok.Rejection() }

func (s splitWriter) CreateDraft(ctx context.Context, container, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	if container == "ENG" {
		return s.broken.CreateDraft(ctx, container, name, markdown)
	}
	return s.ok.CreateDraft(ctx, container, name, markdown)
}

func (s splitWriter) Rejected(ctx context.Context, container, pageID string) (string, error) {
	if container == "ENG" {
		return s.broken.Rejected(ctx, container, pageID)
	}
	return s.ok.Rejected(ctx, container, pageID)
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
// promoting nothing for the life of the process — or, with no ledger,
// drafting again every day what it drafted and what a lead rejected.
func TestAPromoterNeedsEveryHalf(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	full := learning.PromoterOptions{
		Writer: func() (learning.PromotionWriter, string) { return &fakeWriter{}, "" },
		Ledger: memory.NewFleet(),
		Skills: learning.NewSkills(db),
		Models: &stubModels{p: &auxProvider{}},
		Units:  func() []learning.PromotionUnit { return nil },
	}
	for _, missing := range []struct {
		name string
		drop func(o *learning.PromoterOptions)
	}{
		{"writer", func(o *learning.PromoterOptions) { o.Writer = nil }},
		{"ledger", func(o *learning.PromoterOptions) { o.Ledger = nil }},
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

// THE WRITER IS RESOLVED PER PASS, not once when the promoter is built.
//
// The engine arms the background passes BEFORE the inbound service builds
// its integration clients, so a promoter that resolved its writer at
// construction would hold a nil for every company that ever ran. A resolver
// that answers late must be picked up by the pass that runs after it does,
// and the failure this guards is silent: a captured nil promotes nothing for
// the life of the process while the pass reports itself idle.
func TestThePassResolvesItsWriterEachTime(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
	}

	var writer learning.PromotionWriter // nil until the backend "wires"
	w := &fakeWriter{}
	p, err := learning.NewPromoter(learning.PromoterOptions{
		Writer: func() (learning.PromotionWriter, string) {
			if writer == nil {
				return nil, "the knowledge base has not wired yet"
			}
			return writer, ""
		},
		Ledger: memory.NewFleet(),
		Skills: learning.NewSkills(db),
		Models: &stubModels{p: &auxProvider{
			replies: []llm.Completion{{Content: promotionDraft}},
		}},
		Units:       func() []learning.PromotionUnit { return []learning.PromotionUnit{unitOf("dev", "sre", "qa")} },
		MinSiblings: 3,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}

	if got := p.Pass(t.Context()); len(got) != 0 {
		t.Fatalf("a pass with no knowledge base promoted %d thing(s)", len(got))
	}
	if calls := w.calls(); len(calls) != 0 {
		t.Fatalf("a draft was written with no knowledge base: %v", calls)
	}

	writer = w // the inbound service catches up
	if got := p.Pass(t.Context()); len(got) != 1 {
		t.Fatalf("the pass promoted %d thing(s) after the knowledge base "+
			"wired, want 1 — the writer was captured when the pass was built",
			len(got))
	}
	if calls := w.calls(); len(calls) != 1 {
		t.Fatalf("draft calls = %d, want 1", len(calls))
	}
}

// A RECORD IS SETTLED ONCE A PASS, however many convergences it stands for.
// Two clusters can each be close enough to one drafted procedure to be its,
// and asking the knowledge base about the same page twice would write the
// rejection at a version the first write already moved.
func TestARecordStandingForTwoConvergencesIsAskedAboutOnce(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, h := range []string{"dev", "sre", "qa"} {
		seedSibling(t, db, h, "left-"+h, "a", "b", "c", "d", "e", "x")
	}
	for _, h := range []string{"ops", "net", "sec"} {
		seedSibling(t, db, h, "right-"+h, "a", "b", "c", "d", "f", "y")
	}
	ledger := memory.NewFleet()
	drafted := `{"v":1,"state":"drafted","tools":["a","b","c","d","e","f"],"agents":3,` +
		`"backend":"fake","container":"ENG","title":"[Auto-draft] both","page_id":"page-1"}`
	if _, _, err := ledger.CreatePromotion(t.Context(), coord.PromotionRecord{
		Unit: "Platform", Fingerprint: "both", Value: []byte(drafted),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := &fakeWriter{}
	model := &auxProvider{replies: []llm.Completion{{Content: promotionDraft}}}
	p := promoterOver(t, db, w, model, ledger,
		unitOf("dev", "sre", "qa", "ops", "net", "sec"), learning.PromoterOptions{MinSiblings: 3})
	if out := p.Pass(t.Context()); len(out) != 0 || model.calls != 0 {
		t.Fatalf("payloads = %v, model calls = %d: a drafted procedure was drafted again",
			out, model.calls)
	}
	if asked := w.askedAbout(); len(asked) != 1 {
		t.Errorf("the knowledge base was asked about the one draft %d time(s) in one pass",
			len(asked))
	}
}
