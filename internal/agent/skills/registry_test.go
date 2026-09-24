package skills_test

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
)

// skill is a skill read from a page named after its key, which is what every
// case here needs unless it is about the page itself.
func skill(key string, trigger skills.Trigger, required bool) skills.Skill {
	return onPage(key+"-page", key, trigger, required)
}

// onPage is a skill read from a named page.
func onPage(page, key string, trigger skills.Trigger, required bool) skills.Skill {
	return skills.Skill{
		Key: key, Title: strings.ToUpper(key[:1]) + key[1:],
		Summary: "how to use " + key, Body: "the body of " + key + " from " + page,
		Trigger: trigger, Required: required, SourcePageID: page,
	}
}

// registry is a registry after one complete walk over these skills.
func registry(t *testing.T, in ...skills.Skill) *skills.Registry {
	t.Helper()
	r := skills.NewRegistry()
	r.Replace(in)
	if r.Len() != len(in) {
		t.Fatalf("a walk over %d skills registered %d", len(in), r.Len())
	}
	return r
}

// servedFrom is the page a key is served from, or "" when nothing serves it.
func servedFrom(r *skills.Registry, key string) string {
	s, ok := r.Get(key)
	if !ok {
		return ""
	}
	return s.SourcePageID
}

func keysOf(in []skills.Skill) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Key)
	}
	return out
}

// The catalogue offers only what this phase's surface can call: a skill for
// a tool the phase cannot reach is noise the model has to read past.
func TestOnlyTheSkillsForThisSurfaceAreOffered(t *testing.T) {
	t.Parallel()
	r := registry(t,
		skill("chat", skills.Trigger{MCPServer: "mattermost"}, true),
		skill("code", skills.Trigger{MCPServer: "gitlab"}, true),
		skill("recall", skills.Trigger{Tool: "query_episodes"}, false),
	)
	got := keysOf(r.Matching(prompts.PhaseExecute,
		prompts.Surface{Tools: []string{"query_episodes"}, MCPServers: []string{"gitlab"}}))
	if !slices.Equal(got, []string{"code", "recall"}) {
		t.Fatalf("offered %v", got)
	}
}

// KEY-SORTED. The prompt package sorts again — its byte-stability is its own
// promise — but answering in map order here would move every other caller's
// output between two identical builds for no reason anybody could see.
func TestTheCatalogueAnswersInAStableOrder(t *testing.T) {
	t.Parallel()
	r := registry(t,
		skill("zebra", skills.Trigger{Tool: "t"}, true),
		skill("alpha", skills.Trigger{Tool: "t"}, true),
		skill("middle", skills.Trigger{Tool: "t"}, true),
	)
	want := []string{"alpha", "middle", "zebra"}
	for range 8 {
		got := keysOf(r.Matching(prompts.PhaseExecute, prompts.Surface{Tools: []string{"t"}}))
		if !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// A skill declaring phases is offered only in those; one declaring none is
// offered wherever its surface matches, which is the ordinary case — a skill
// about a tool applies wherever that tool can be called.
func TestPhaseScopingIsOptOut(t *testing.T) {
	t.Parallel()
	scoped := skill("review-only", skills.Trigger{Tool: "t"}, true)
	scoped.Phases = []prompts.Phase{prompts.PhaseReview}
	r := registry(t, scoped, skill("everywhere", skills.Trigger{Tool: "t"}, true))

	on := prompts.Surface{Tools: []string{"t"}}
	if got := keysOf(r.Matching(prompts.PhaseReview, on)); !slices.Equal(got,
		[]string{"everywhere", "review-only"}) {
		t.Fatalf("review offered %v", got)
	}
	if got := keysOf(r.Matching(prompts.PhaseExecute, on)); !slices.Equal(got,
		[]string{"everywhere"}) {
		t.Fatalf("execute offered %v", got)
	}
}

// A PAGE WRITE REPLACES rather than merges: a page IS the skill, so an edit
// that removed a trigger leaf must remove it here, and a merge would keep the
// skill matching a surface its author just stopped claiming.
func TestAnEditReplacesTheSkillWholesale(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("chat", skills.Trigger{AnyOf: []skills.Trigger{
		{MCPServer: "mattermost"}, {MCPServer: "slack"}}}, true))

	narrowed := skill("chat", skills.Trigger{MCPServer: "mattermost"}, true)
	change, err := r.PutPage(narrowed)
	if err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	if change.Changed() || change.Before != "chat" || change.After != "chat" {
		t.Fatalf("an edit that kept its key reported %+v", change)
	}
	if got := r.Matching(prompts.PhaseExecute,
		prompts.Surface{MCPServers: []string{"slack"}}); len(got) != 0 {
		t.Fatalf("the removed leaf still matches: %v", keysOf(got))
	}
	if r.Len() != 1 {
		t.Fatalf("an edit produced %d skills", r.Len())
	}
}

// A PAGE WHOSE KEY WAS EDITED LEAVES NOTHING BEHIND. This is the case a
// key-addressed registry could not see: the change names the page, the page
// no longer says what key it used to hold, and the old key would be served
// for ever.
func TestAPageWhoseKeyWasEditedLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	r := registry(t, onPage("p1", "old-name", skills.Trigger{Tool: "t"}, true))

	change, err := r.PutPage(onPage("p1", "new-name", skills.Trigger{Tool: "t"}, true))
	if err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	if !change.Changed() || change.Before != "old-name" || change.After != "new-name" {
		t.Fatalf("a renamed key reported %+v", change)
	}
	if _, ok := r.Get("old-name"); ok {
		t.Fatal("the key the page no longer declares is still served")
	}
	if servedFrom(r, "new-name") != "p1" || r.Len() != 1 {
		t.Fatalf("the renamed skill is not the one thing registered (%d)", r.Len())
	}
}

// DROPPING A PAGE REPORTS WHAT IT HELD, which is what lets a sync tell "a
// skill page was deleted" from "a page that was never a skill was deleted":
// it drops on every removal it hears of, and only one is worth a line.
func TestDroppingAPageReportsWhatItHeld(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("chat", skills.Trigger{Tool: "t"}, true))
	if change := r.DropPage("chat-page"); change.Before != "chat" || change.After != "" {
		t.Fatalf("dropping a skill page reported %+v", change)
	}
	if r.Len() != 0 || r.HoldsPage("chat-page") {
		t.Fatal("the dropped page is still registered")
	}
	if change := r.DropPage("chat-page"); change.Changed() {
		t.Fatalf("dropping an absent page reported %+v", change)
	}
}

// A DUPLICATED KEY IS SERVED FROM THE SAME PAGE ON EVERY NODE, whatever order
// its walks and updates came in. Two nodes that each served the page they
// happened to hear about last would give one seat two different conventions
// depending on where it ran.
func TestADuplicatedKeyIsServedFromTheOlderPageInAnyOrder(t *testing.T) {
	t.Parallel()
	older := onPage("9", "chat", skills.Trigger{Tool: "t"}, true)
	newer := onPage("10", "chat", skills.Trigger{Tool: "t"}, true)

	walked := skills.NewRegistry()
	walked.Replace([]skills.Skill{newer, older})
	if got := servedFrom(walked, "chat"); got != "9" {
		t.Fatalf("a walk served page %q, want the older page 9", got)
	}

	for _, order := range [][]skills.Skill{{older, newer}, {newer, older}} {
		r := skills.NewRegistry()
		var last skills.PageChange
		for _, s := range order {
			change, err := r.PutPage(s)
			if err != nil {
				t.Fatalf("PutPage: %v", err)
			}
			last = change
		}
		if got := servedFrom(r, "chat"); got != "9" {
			t.Fatalf("writing %s then %s served page %q, want 9",
				order[0].SourcePageID, order[1].SourcePageID, got)
		}
		if shadowed := order[1].SourcePageID == "10"; last.Shadowed != shadowed {
			t.Errorf("the last write reported shadowed=%v, want %v", last.Shadowed, shadowed)
		}
	}
}

// A SHADOWED DUPLICATE TAKES THE KEY THE MOMENT IT IS FREE. Kept by key, the
// loser would have been discarded and only the next walk could bring it back.
func TestAShadowedDuplicateTakesTheKeyWhenItIsFreed(t *testing.T) {
	t.Parallel()
	r := registry(t, onPage("1", "chat", skills.Trigger{Tool: "t"}, true))
	if _, err := r.PutPage(onPage("2", "chat", skills.Trigger{Tool: "t"}, true)); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	if !r.HoldsPage("2") {
		t.Fatal("the shadowed duplicate was not kept")
	}

	if _, err := r.PutPage(onPage("1", "renamed", skills.Trigger{Tool: "t"}, true)); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	if got := servedFrom(r, "chat"); got != "2" {
		t.Fatalf("after the winner moved to another key, chat is served from %q", got)
	}

	r.DropPage("1")
	r.DropPage("2")
	if r.Len() != 0 {
		t.Fatalf("%d skills survived dropping both pages", r.Len())
	}
}

// ANY SEQUENCE OF PAGE UPDATES ENDS WHERE A WALK OF THE SAME PAGES WOULD. This
// is the property a fleet converges on: a node that heard every change one
// page at a time and a node that walked the container afterwards must serve
// the same thing, or the periodic walk would keep moving guidance under seats
// that had already read it.
func TestPageUpdatesEndWhereAWalkOfTheSamePagesWould(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 11))
	pages := []string{"1", "2", "3", "10", "11"}
	keys := []string{"chat", "code", "deploy"}

	for round := range 200 {
		r := skills.NewRegistry()
		final := map[string]skills.Skill{}
		for step := range 12 {
			page := pages[rng.IntN(len(pages))]
			if rng.IntN(3) == 0 {
				r.DropPage(page)
				delete(final, page)
				continue
			}
			s := onPage(page, keys[rng.IntN(len(keys))], skills.Trigger{Tool: "t"}, true)
			s.Body = fmt.Sprintf("revision %d.%d of page %s", round, step, page)
			if _, err := r.PutPage(s); err != nil {
				t.Fatalf("PutPage: %v", err)
			}
			final[page] = s
		}

		walked := skills.NewRegistry()
		walked.Replace(slices.Collect(maps.Values(final)))
		for _, key := range keys {
			got, gotOK := r.Get(key)
			want, wantOK := walked.Get(key)
			if gotOK != wantOK || got.SourcePageID != want.SourcePageID || got.Body != want.Body {
				t.Fatalf("round %d: %q is %+v after page updates and %+v after a walk",
					round, key, got, want)
			}
		}
		for _, page := range pages {
			_, held := final[page]
			if r.HoldsPage(page) != held || walked.HoldsPage(page) != held {
				t.Fatalf("round %d: page %s held=%v after updates, %v after a walk, want %v",
					round, page, r.HoldsPage(page), walked.HoldsPage(page), held)
			}
		}
	}
}

// A SKILL THAT NAMES NO PAGE HAS NO IDENTITY a later change could reach: a
// deletion names a page, and nothing could ever remove it.
func TestASkillThatNamesNoPageIsRefused(t *testing.T) {
	t.Parallel()
	orphan := skill("chat", skills.Trigger{Tool: "t"}, true)
	orphan.SourcePageID = ""

	r := skills.NewRegistry()
	if _, err := r.PutPage(orphan); err == nil {
		t.Fatal("a skill with no source page was stored")
	}
	r.Replace([]skills.Skill{orphan, skill("code", skills.Trigger{Tool: "t"}, true)})
	if _, ok := r.Get("chat"); ok {
		t.Fatal("a walk stored a skill with no source page")
	}
	if _, ok := r.Get("code"); !ok {
		t.Fatal("refusing one skill cost the walk the other")
	}
}

// A REPLACE IS ATOMIC, which is what makes a boot walk safe against a
// registry already serving: applied one at a time it would leave a window
// where half the company's guidance exists.
func TestAWalkSwapsTheWholeSetAndRefusesWhatIsInvalid(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("old", skills.Trigger{Tool: "t"}, true))
	broken := skill("broken", skills.Trigger{}, true)

	r.Replace([]skills.Skill{skill("fresh", skills.Trigger{Tool: "t"}, true), broken})
	if r.HoldsPage("old-page") {
		t.Fatal("a walk kept a page it did not carry")
	}
	if _, ok := r.Get("old"); ok {
		t.Fatal("a walk left the previous set behind")
	}
	if _, ok := r.Get("broken"); ok {
		t.Fatal("a skill with no trigger was registered")
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Fatal("the walk's own skill is missing")
	}
}

// A skill with no summary can never be chosen — the summary is the only
// thing that always reaches the prompt — so registering it would store
// something nothing can reach.
func TestASkillThatCannotBeOfferedIsRefused(t *testing.T) {
	t.Parallel()
	r := skills.NewRegistry()
	for _, tc := range []struct {
		name string
		in   skills.Skill
	}{
		{"no key", skills.Skill{Summary: "s", Trigger: skills.Trigger{Tool: "t"}}},
		{"no summary", skills.Skill{Key: "k", Trigger: skills.Trigger{Tool: "t"}}},
		{"no trigger", skills.Skill{Key: "k", Summary: "s"}},
		{"an over-long summary", skills.Skill{Key: "k", Trigger: skills.Trigger{Tool: "t"},
			Summary: strings.Repeat("x", skills.MaxSummaryBytes+1)}},
		{"an over-long body", skills.Skill{Key: "k", Summary: "s",
			Trigger: skills.Trigger{Tool: "t"},
			Body:    strings.Repeat("x", skills.MaxBodyBytes+1)}},
	} {
		tc.in.SourcePageID = "p1"
		if _, err := r.PutPage(tc.in); err == nil {
			t.Fatalf("%s was accepted", tc.name)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("%d refused skills were registered", r.Len())
	}
}

// The catalogue renders ${var} references so an operator's facts appear
// substituted rather than as the reference.
func TestTheCatalogueRendersOperatorVariables(t *testing.T) {
	t.Parallel()
	r := registry(t, skills.Skill{
		Key: "chat", Title: "Chat on ${tenant}",
		Summary: "post to ${tenant}.example.com",
		Body:    "the ${tenant} workspace",
		Trigger: skills.Trigger{Tool: "t"}, SourcePageID: "chat-page",
	})
	r.SetVariables(map[string]string{"tenant": "nimbus"})

	offered := r.SkillsFor(prompts.PhaseExecute, prompts.Surface{Tools: []string{"t"}})
	if len(offered) != 1 {
		t.Fatalf("offered %+v", offered)
	}
	// The prompt package renders through Render, so the SUMMARY comes back
	// raw here and substituted there — which is what keeps the source-byte
	// cap on the summary meaningful.
	if got := r.Render(offered[0].Summary); got != "post to nimbus.example.com" {
		t.Fatalf("rendered summary = %q", got)
	}
	loaded, ok := r.Load("chat")
	if !ok || !strings.Contains(loaded.Body, "the nimbus workspace") ||
		!strings.Contains(loaded.Body, "# Chat on nimbus") {
		t.Fatalf("body = %q", loaded.Body)
	}
}

// A body carries its TITLE and summary, because a model that asked for a key
// gets back prose with no header otherwise — and a body it cannot attribute
// is a body it cannot decide to trust.
func TestALoadedBodyNamesWhatItIs(t *testing.T) {
	t.Parallel()
	chat := skill("chat", skills.Trigger{Tool: "t"}, true)
	chat.SourceBackend, chat.SourceContainer, chat.SourceTitle = "native", "SKILLS", "Chat page"
	r := registry(t, chat)
	loaded, ok := r.Load("chat")
	if !ok {
		t.Fatal("the skill has no body")
	}
	for _, want := range []string{"# Chat", "how to use chat", "the body of chat"} {
		if !strings.Contains(loaded.Body, want) {
			t.Fatalf("the body is missing %q:\n%s", want, loaded.Body)
		}
	}
	// AND THE PAGE IT CAME FROM, in the same answer, so a load is recorded
	// against the page whose body was handed over.
	if loaded.PageID != chat.SourcePageID || loaded.Backend != "native" ||
		loaded.Container != "SKILLS" || loaded.Title != "Chat page" {
		t.Errorf("loaded page = %+v; want the skill's own source", loaded)
	}
	if _, ok := r.Load("never-existed"); ok {
		t.Fatal("an absent key produced a body")
	}
}

// A NIL REGISTRY is what a company that published no skills has, and every
// read must answer rather than panic — the prompt path consults it on every
// phase.
func TestANilRegistryAnswersEmpty(t *testing.T) {
	t.Parallel()
	var r *skills.Registry
	if got := r.Matching(prompts.PhaseExecute, prompts.Surface{Tools: []string{"t"}}); got != nil {
		t.Fatalf("Matching = %v", got)
	}
	if _, ok := r.Get("chat"); ok {
		t.Fatal("a nil registry has a skill")
	}
	if got := r.Render("${tenant}"); got != "${tenant}" {
		t.Fatalf("Render = %q", got)
	}
	if r.Len() != 0 || r.HoldsPage("chat-page") || r.DropPage("chat-page").Changed() {
		t.Fatal("a nil registry reported content")
	}
	if _, err := r.PutPage(skill("chat", skills.Trigger{Tool: "t"}, true)); err != nil {
		t.Fatalf("a nil registry refused a write: %v", err)
	}
	r.Replace([]skills.Skill{skill("chat", skills.Trigger{Tool: "t"}, true)})
	r.SetVariables(map[string]string{"a": "b"})
	r.Audit(nil, nil)
}

// AN OFFER RECORDS WHAT EACH RENDER OFFERED, AND REPORTS IT ONCE.
//
// Which skills a prompt's catalogue carried is known only to the render, since
// the registry is live and a second match could answer differently. Each render
// is its own offering — two worker prompts offered the same skill twice, and a
// merged set would say one prompt carried both — and a drain hands each over
// exactly once, however many callers drain.
func TestAnOfferRecordsEachRenderOnce(t *testing.T) {
	t.Parallel()
	r := registry(t,
		skill("chat", skills.Trigger{Tool: "post"}, false),
		skill("deploy", skills.Trigger{Tool: "ship"}, false))
	offer := r.Offer()
	cat := offer.Catalogue()
	surface := prompts.Surface{Tools: []string{"post", "ship"}}

	if got := cat.SkillsFor(prompts.PhaseExecute, surface); len(got) != 2 {
		t.Fatalf("the catalogue offered %d skills, want both", len(got))
	}
	cat.SkillsFor(prompts.PhaseSubagent, prompts.Surface{Tools: []string{"post"}})
	// A render that matched nothing is not an offer.
	cat.SkillsFor(prompts.PhaseExecute, prompts.Surface{Tools: []string{"none"}})

	drained := offer.Drain()
	if len(drained) != 2 {
		t.Fatalf("drained %d offerings, want one per render that offered something", len(drained))
	}
	if len(drained[0]) != 2 || drained[0][0].SourcePageID != "chat-page" ||
		drained[0][1].SourcePageID != "deploy-page" {
		t.Errorf("first offering = %+v; want both skills, by page", drained[0])
	}
	if len(drained[1]) != 1 || drained[1][0].Key != "chat" {
		t.Errorf("second offering = %+v; want chat alone", drained[1])
	}
	if again := offer.Drain(); len(again) != 0 {
		t.Errorf("a second drain reported %d offerings again", len(again))
	}

	var none *skills.Registry
	if none.Offer().Catalogue() != nil {
		t.Error("a nil registry's offer is a non-nil catalogue, which renders a header over nothing")
	}
}
