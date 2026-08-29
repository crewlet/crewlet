package plane_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/plane"
)

// WHAT THE PROMOTION WRITER IS FOR.
//
// A cross-agent promotion is a draft page a unit lead reviews. Plane pages
// have no parent chain, so what hides a draft from every agent's knowledge
// search is the title PREFIX — which the pass supplies. What this side owns
// is dedup: the pass re-clusters the same skills on every tick, so without it
// one converging team yields one page a day for the life of the company.

const draftTitle = knowledge.AutoDraftTitlePrefix + "cut-a-release"

// A DRAFT IS CREATED, STAMPED AND RENDERED.
func TestAPromotedDraftIsCreatedWithItsExternalIdentity(t *testing.T) {
	t.Parallel()
	f := newInstance()
	writer := plane.NewPromotionWriter(workspaceClient(t, f))

	page, created, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle,
		"# Steps\n\n1. tag\n")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if !created || page.ID == "" || page.Title != draftTitle {
		t.Fatalf("created = %v, page = %+v", created, page)
	}
	row, found := f.pageNamed(draftTitle)
	if !found {
		t.Fatal("no page was created")
	}
	if !strings.HasPrefix(row.ExternalID, "draft:") {
		t.Fatalf("external_id = %q — without this engine's own identity a "+
			"renamed draft is re-created beside itself on the next tick",
			row.ExternalID)
	}
	if !row.Managed() {
		t.Fatalf("external_source = %q, want this engine's", row.ExternalSource)
	}
	if !strings.Contains(row.HTML, "<li>tag</li>") {
		t.Fatalf("the markdown was not rendered to HTML:\n%s", row.HTML)
	}
}

// AN EXISTING DRAFT IS RETURNED, NOT DUPLICATED.
func TestAnExistingPlaneDraftIsReturnedRatherThanDuplicated(t *testing.T) {
	t.Parallel()
	f := newInstance()
	writer := plane.NewPromotionWriter(workspaceClient(t, f))

	first, created, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle, "# One\n")
	if err != nil || !created {
		t.Fatalf("first draft: %v created=%v", err, created)
	}
	again, created, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle, "# Two\n")
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if created {
		t.Fatal("the second call reported a fresh creation, so the pass would " +
			"announce the same promotion on every tick")
	}
	if again.ID != first.ID {
		t.Fatalf("second id = %q, want the existing %q", again.ID, first.ID)
	}
	if n := f.pagesNamed(draftTitle); n != 1 {
		t.Fatalf("%d pages named %q — the draft was duplicated", n, draftTitle)
	}
}

// DEDUP IS BY EXTERNAL ID, NOT BY NAME. A lead reviewing a draft renames it,
// and a name-keyed check would then create a second copy beside the one they
// are editing.
func TestARenamedDraftIsStillFoundByItsExternalIdentity(t *testing.T) {
	t.Parallel()
	f := newInstance()
	writer := plane.NewPromotionWriter(workspaceClient(t, f))
	if _, _, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle, "# One\n"); err != nil {
		t.Fatalf("first draft: %v", err)
	}
	f.renamePage(draftTitle, "Release procedure (reviewed)")

	_, created, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle, "# One\n")
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if created {
		t.Fatal("a renamed draft was re-created beside the one a lead is editing")
	}
}

// A PAGE SOMEBODY ELSE OWNS IS NOT ADOPTED. Another tool's row carrying the
// same external id is not this engine's draft, and treating it as one would
// make the pass report a promotion it never wrote.
func TestAnUnmanagedPageWithTheSameIdIsNotAdopted(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.addForeignPage("p-eng", draftTitle, "draft:"+draftTitle)
	writer := plane.NewPromotionWriter(workspaceClient(t, f))

	_, created, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle, "# One\n")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if !created {
		t.Fatal("a page owned by another tool was adopted as this engine's draft")
	}
}

// AN UNREADABLE PROJECT REFUSES THE DRAFT rather than creating a second copy.
// A truncated or failed walk makes an existing draft look absent.
func TestAnUnreadableProjectRefusesTheDraft(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.failPageWalk = true
	writer := plane.NewPromotionWriter(workspaceClient(t, f))

	if _, created, err := writer.CreateDraft(t.Context(), "p-eng", draftTitle, "# One\n"); err == nil {
		t.Fatalf("a draft was created over an unreadable page list (created=%v)", created)
	}
	if n := f.pagesNamed(draftTitle); n != 0 {
		t.Fatalf("%d pages created despite the failed walk", n)
	}
}

// NO PROJECT IS REFUSED BEFORE ANY CALL.
func TestAPlaneDraftWithNoProjectIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	writer := plane.NewPromotionWriter(workspaceClient(t, f))
	if _, _, err := writer.CreateDraft(t.Context(), "  ", "x", "y"); err == nil {
		t.Fatal("a draft with no project was accepted")
	}
}
