package confluence_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// WHAT THE PROMOTION WRITER IS FOR.
//
// A cross-agent promotion is a draft page a unit lead reviews. Two properties
// carry the whole design: the draft must land UNDER the auto-drafted parent,
// because that subtree is what the Plan-phase knowledge search excludes — a
// page outside it is reachable by every seat in the company, unreviewed — and
// a promotion already drafted must not be drafted again, because the pass
// re-clusters the same skills on every tick.

// A DRAFT LANDS UNDER THE AUTO-DRAFTED PARENT, and the parent is created when
// the space does not have one yet.
func TestADraftLandsUnderTheAutoDraftedParent(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	writer := confluence.NewPromotionWriter(wikiClient(t, w))

	page, created, err := writer.CreateDraft(t.Context(), "ENG",
		knowledge.AutoDraftTitlePrefix+"cut-a-release", "# Steps\n\n1. tag\n")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if !created || page.ID == "" {
		t.Fatalf("created = %v, page = %+v", created, page)
	}

	parent, found := w.pageTitled(knowledge.AutoDraftedParent)
	if !found {
		t.Fatal("the Auto-Drafted Skills parent was not created — a draft " +
			"outside that subtree is one every agent's knowledge search finds")
	}
	drafted, found := w.pageTitled(knowledge.AutoDraftTitlePrefix + "cut-a-release")
	if !found {
		t.Fatal("the draft was not created")
	}
	if drafted.Parent != parent.ID {
		t.Fatalf("the draft's parent is %q, want the auto-drafted page %q — "+
			"a draft at the space root is visible to every agent",
			drafted.Parent, parent.ID)
	}
	if !strings.Contains(drafted.Body, "<li>tag</li>") {
		t.Fatalf("the markdown was not rendered to storage format:\n%s", drafted.Body)
	}
}

// AN EXISTING DRAFT IS RETURNED, NOT RE-CREATED. The pass asks on every tick,
// so without this one converging team yields one page a day forever.
func TestAnExistingDraftIsReturnedRatherThanDuplicated(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	writer := confluence.NewPromotionWriter(wikiClient(t, w))
	title := knowledge.AutoDraftTitlePrefix + "cut-a-release"

	first, created, err := writer.CreateDraft(t.Context(), "ENG", title, "# One\n")
	if err != nil || !created {
		t.Fatalf("first draft: %v created=%v", err, created)
	}
	again, created, err := writer.CreateDraft(t.Context(), "ENG", title, "# Two\n")
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if created {
		t.Fatal("the second call reported a fresh creation, so the pass would " +
			"announce the same promotion on every tick")
	}
	if again.ID != first.ID {
		t.Fatalf("second draft id = %q, want the existing %q", again.ID, first.ID)
	}
	if n := w.countTitled(title); n != 1 {
		t.Fatalf("%d pages titled %q — the draft was duplicated", n, title)
	}
}

// THE PARENT IS REUSED, not re-created, when a later draft lands in the same
// space. Two parents would leave half the drafts in a subtree the search does
// not exclude.
func TestASecondDraftReusesTheSameParent(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	writer := confluence.NewPromotionWriter(wikiClient(t, w))
	for _, name := range []string{"cut-a-release", "triage-a-bug"} {
		if _, _, err := writer.CreateDraft(t.Context(), "ENG",
			knowledge.AutoDraftTitlePrefix+name, "# Steps\n"); err != nil {
			t.Fatalf("CreateDraft(%s): %v", name, err)
		}
	}
	if n := w.countTitled(knowledge.AutoDraftedParent); n != 1 {
		t.Fatalf("%d auto-drafted parents in one space, want 1", n)
	}
}

// A SPACE THAT REFUSES THE PARENT REFUSES THE DRAFT. Filing at the space root
// instead would publish an unreviewed procedure to every agent — the one
// outcome the review step exists to prevent.
func TestADraftIsRefusedRatherThanFiledWhereAgentsCanSeeIt(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	w.refuseCreate(knowledge.AutoDraftedParent)
	writer := confluence.NewPromotionWriter(wikiClient(t, w))

	_, created, err := writer.CreateDraft(t.Context(), "ENG",
		knowledge.AutoDraftTitlePrefix+"cut-a-release", "# Steps\n")
	if err == nil {
		t.Fatal("a draft was accepted with no parent to hide it under")
	}
	if created {
		t.Fatal("a refused draft reported itself created")
	}
	if _, found := w.pageTitled(knowledge.AutoDraftTitlePrefix + "cut-a-release"); found {
		t.Fatal("the draft was filed at the space root, where every agent's " +
			"knowledge search reaches it")
	}
}

// NO SPACE IS REFUSED BEFORE ANY CALL. A unit with no container is the pass's
// soft-skip, and reaching here with an empty one is a wiring bug rather than
// a configuration a page should be guessed for.
func TestADraftWithNoSpaceIsRefused(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	writer := confluence.NewPromotionWriter(wikiClient(t, w))
	if _, _, err := writer.CreateDraft(t.Context(), "  ", "x", "y"); err == nil {
		t.Fatal("a draft with no space was accepted")
	}
}
