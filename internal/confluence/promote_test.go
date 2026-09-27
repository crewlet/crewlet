package confluence_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// WHAT THE PROMOTION WRITER IS FOR.
//
// A cross-agent promotion is a draft page a unit lead reviews. Three
// properties carry the whole design: the draft must land UNDER the
// auto-drafted parent, because that subtree is what a seat's knowledge search
// excludes (a page outside it is reachable by every seat in the company,
// unreviewed); a create must find a page already at its title rather than
// make a second, because a pass that made a page and could not record it
// makes it again from its record; and a lead's deletion must read as a
// rejection, and nothing else must.

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

// AN EXISTING DRAFT IS RETURNED, NOT RE-CREATED. A pass that made the page and
// could not record it makes it again from its record, under the same title,
// and must find the page rather than make a second.
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

// A DELETED DRAFT IS A REJECTION, and one that stands — under the drafts
// parent or moved out of it, which is publishing — is not. Deleting is what
// Confluence lets a lead do to a page, and it is the gesture the draft's page
// names.
func TestADeletedDraftReadsAsRejectedAndAStandingOneDoesNot(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	client := wikiClient(t, w)
	writer := confluence.NewPromotionWriter(client)
	page, created, err := writer.CreateDraft(t.Context(), "ENG",
		knowledge.AutoDraftTitlePrefix+"cut-a-release", "# Steps\n")
	if err != nil || !created {
		t.Fatalf("CreateDraft: %v created=%v", err, created)
	}
	rejected := func(when string) string {
		t.Helper()
		how, err := writer.Rejected(t.Context(), "ENG", page.ID)
		if err != nil {
			t.Fatalf("Rejected %s: %v", when, err)
		}
		return how
	}
	if how := rejected("under review"); how != "" {
		t.Errorf("a draft under review reads as rejected: %q", how)
	}
	w.reparent(page.ID, "")
	if how := rejected("published"); how != "" {
		t.Errorf("a published draft reads as rejected: %q", how)
	}
	if err := client.DeletePage(t.Context(), page.ID); err != nil {
		t.Fatalf("DeletePage: %v", err)
	}
	if how := rejected("deleted"); how == "" {
		t.Error("a deleted draft does not read as rejected, so the pass would " +
			"never record the lead's decision")
	}
}

// A DRAFT MISSING FROM A SPACE THAT DOES NOT ANSWER IS NOT A REJECTION.
// Confluence answers a page it does not serve the same way whether a lead
// deleted it or the whole space is gone or unreadable to this credential, and
// only the first is a decision.
func TestADraftMissingFromASpaceThatDoesNotAnswerIsNotARejection(t *testing.T) {
	t.Parallel()
	w := newWiki(t) // no spaces at all
	writer := confluence.NewPromotionWriter(wikiClient(t, w))
	how, err := writer.Rejected(t.Context(), "ENG", "p-gone")
	if err == nil {
		t.Fatalf("a draft missing from an unreadable space read as %q with no "+
			"error — an outage recorded as a lead's rejection", how)
	}
	if how != "" {
		t.Errorf("an unanswerable read still said %q", how)
	}
}

// THE PARENT TELLS A LEAD THE SAME GESTURE THE WRITER READS, and the writer
// names its knowledge base as its searcher does: a record carries that name,
// and a draft recorded under another would read as one in a knowledge base
// the company left.
func TestTheParentTellsALeadToDeleteADraftToRejectIt(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	writer := confluence.NewPromotionWriter(wikiClient(t, w))
	if _, _, err := writer.CreateDraft(t.Context(), "ENG",
		knowledge.AutoDraftTitlePrefix+"cut-a-release", "# Steps\n"); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if writer.Rejection() != "delete it" {
		t.Errorf("the draft tells its lead to %q, and the writer reads a deletion",
			writer.Rejection())
	}
	parent, found := w.pageTitled(knowledge.AutoDraftedParent)
	if !found {
		t.Fatal("no drafts parent")
	}
	if !strings.Contains(parent.Body, "To reject one, delete it") {
		t.Errorf("the drafts parent does not say how to reject a draft:\n%s", parent.Body)
	}
	if got := writer.Backend(); got != (&confluence.Searcher{}).Backend() {
		t.Errorf("the writer calls its knowledge base %q, its searcher %q",
			got, (&confluence.Searcher{}).Backend())
	}
}
