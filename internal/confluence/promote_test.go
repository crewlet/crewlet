package confluence_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	// AND WHAT DOES NOT REJECT ONE: deleting the parent moves its drafts up
	// a level, out of the subtree the search leaves out, and every agent
	// then finds them.
	if !strings.Contains(parent.Body, "Do not delete this page to reject what is under it") {
		t.Errorf("the drafts parent does not warn against deleting it:\n%s", parent.Body)
	}
	if got := writer.Backend(); got != (&confluence.Searcher{}).Backend() {
		t.Errorf("the writer calls its knowledge base %q, its searcher %q",
			got, (&confluence.Searcher{}).Backend())
	}
}

// scriptedWiki answers the few calls a promotion writer makes with statuses a
// test chooses, where the stateful fake answers only success.
type scriptedWiki struct {
	*httptest.Server

	mu sync.Mutex
	// page is the status GET /content/{id} answers.
	page int
	// space is the status GET /space/{key} answers.
	space int
	// create is the status POST /content answers.
	create int
	// heldAfterCreate makes a title lookup find a page once a create has
	// been refused: somebody else's create landing between the two.
	heldAfterCreate bool
	refused         bool
	// parentHeld makes the space already hold the drafts parent, so the
	// create a case scripts is the draft's own.
	parentHeld bool
}

func newScriptedWiki(t *testing.T) *scriptedWiki {
	t.Helper()
	w := &scriptedWiki{page: http.StatusOK, space: http.StatusOK, create: http.StatusOK}
	w.Server = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.Close)
	return w
}

func (w *scriptedWiki) serve(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	path := strings.TrimPrefix(req.URL.Path, confluence.APIPath)
	rw.Header().Set("Content-Type", "application/json")
	answer := func(status int, body string) {
		if status != http.StatusOK {
			rw.WriteHeader(status)
			fmt.Fprintf(rw, `{"message":"scripted %d"}`, status)
			return
		}
		fmt.Fprint(rw, body)
	}
	switch {
	case strings.HasPrefix(path, "/space/"):
		answer(w.space, `{}`)
	case path == "/content" && req.Method == http.MethodGet:
		if req.URL.Query().Get("title") == knowledge.AutoDraftedParent && w.parentHeld {
			fmt.Fprint(rw, `{"results":[{"id":"p-parent","title":"parent","type":"page"}]}`)
			return
		}
		if w.heldAfterCreate && w.refused {
			fmt.Fprintf(rw, `{"results":[{"id":"p-theirs","title":%q,"type":"page"}]}`,
				req.URL.Query().Get("title"))
			return
		}
		fmt.Fprint(rw, `{"results":[]}`)
	case path == "/content" && req.Method == http.MethodPost:
		if w.create != http.StatusOK {
			w.refused = true
		}
		answer(w.create, `{"id":"p-new","title":"made","type":"page"}`)
	case strings.HasPrefix(path, "/content/"):
		answer(w.page, `{"id":"p-1","title":"a draft","type":"page"}`)
	default:
		rw.WriteHeader(http.StatusNotFound)
		fmt.Fprint(rw, `{"message":"no such route"}`)
	}
}

func (w *scriptedWiki) writer(t *testing.T) *confluence.PromotionWriter {
	t.Helper()
	c, err := confluence.NewClient(confluence.ClientOptions{URL: w.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return confluence.NewPromotionWriter(c)
}

// A DRAFT CONFLUENCE COULD NOT BE ASKED ABOUT IS NOT A REJECTION. Only a page
// that is not served in a space that is served was deleted; a server error, a
// refused credential or a forbidden read says nothing about what a lead did,
// and a pass that recorded any of them as a rejection would hold, for good, a
// decision nobody made.
func TestADraftConfluenceCouldNotBeAskedAboutIsNotARejection(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			wiki := newScriptedWiki(t)
			wiki.page = status
			how, err := wiki.writer(t).Rejected(t.Context(), "ENG", "p-1")
			if err == nil {
				t.Fatalf("a draft read that answered %d was concluded about: %q", status, how)
			}
			if how != "" {
				t.Errorf("a draft read that answered %d still said %q", status, how)
			}
		})
	}
}

// A TITLE CONFLUENCE REFUSES BY LENGTH IS REFUSED BEFORE ANYTHING IS RECORDED,
// counted so that a title accepted here fits however the server counts it.
func TestADraftTitleIsCheckedAgainstConfluencesLimit(t *testing.T) {
	t.Parallel()
	writer := confluence.NewPromotionWriter(wikiClient(t, newWiki(t, "ENG")))
	for _, tc := range []struct {
		name  string
		title string
		ok    bool
	}{
		{"at the limit", strings.Repeat("a", 255), true},
		{"one past it", strings.Repeat("a", 256), false},
		{"accented letters at the limit", strings.Repeat("é", 255), true},
		// A character outside the basic plane is two UTF-16 units, which is
		// the count that can never undercount what a server sees.
		{"astral characters past it", strings.Repeat("😀", 128), false},
		{"blank", "   ", false},
	} {
		err := writer.CheckDraft(tc.title, "body")
		if (err == nil) != tc.ok {
			t.Errorf("%s: CheckDraft = %v, want accepted %v", tc.name, err, tc.ok)
		}
	}
}

// A CREATE CONFLUENCE REFUSES SAYS WHETHER IT WILL EVER CLEAR. A space that is
// not served takes no page however often it is asked, and a page refused as
// sent in a space that is served is refused again — while a title somebody
// else took between the look and the create is handed back as found, and an
// outage is left to the next pass.
func TestACreateConfluenceRefusesSaysWhetherItWillEverClear(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		create    int
		space     int
		parent    bool // the space already holds the drafts parent
		held      bool
		container bool
		page      bool
		found     bool
	}{
		{name: "a space that is not served", create: http.StatusNotFound,
			space: http.StatusNotFound, container: true},
		{name: "a draft refused in a served space", create: http.StatusBadRequest,
			space: http.StatusOK, parent: true, page: true},
		// The parent is the same page for every draft in the space, so the
		// space refusing it is the space refusing them all.
		{name: "the drafts parent refused in a served space", create: http.StatusBadRequest,
			space: http.StatusOK, container: true},
		{name: "a title taken between the look and the create",
			create: http.StatusBadRequest, space: http.StatusOK, parent: true,
			held: true, found: true},
		{name: "an outage", create: http.StatusInternalServerError, space: http.StatusOK},
		{name: "a space that cannot be asked about", create: http.StatusNotFound,
			space: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wiki := newScriptedWiki(t)
			wiki.create, wiki.space, wiki.heldAfterCreate = tc.create, tc.space, tc.held
			wiki.parentHeld = tc.parent
			page, created, err := wiki.writer(t).CreateDraft(t.Context(), "ENG",
				knowledge.AutoDraftTitlePrefix+"cut-a-release", "# Steps\n")
			if tc.found {
				if err != nil || created || page.ID != "p-theirs" {
					t.Fatalf("CreateDraft = %+v, %v, %v, want the page that took "+
						"the title, found", page, created, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a create answered %d was reported made: %+v", tc.create, page)
			}
			var container interface{ RefusesContainer() bool }
			var refused interface{ RefusesPage() bool }
			if got := errors.As(err, &container) && container.RefusesContainer(); got != tc.container {
				t.Errorf("RefusesContainer = %v, want %v: %v", got, tc.container, err)
			}
			if got := errors.As(err, &refused) && refused.RefusesPage(); got != tc.page {
				t.Errorf("RefusesPage = %v, want %v: %v", got, tc.page, err)
			}
		})
	}
}
