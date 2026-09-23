package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

type fakeKB struct {
	renames []renamed
	page    pages.Detail

	created  []pages.NewPage
	saved    []pages.Save
	comments []pages.NewComment
	edits    []commentEdit
	actors   []pages.Actor

	readErr  error
	writeErr error

	// awaited is every position [builtin.PageDeps.Await] was handed, in
	// order, so a tool that waited for the wrong write — or waited before
	// making the write that needs it — is visible rather than merely
	// plausible.
	awaited []uint64

	// renameResult is what Rename answers. Nil takes an ordinary rename
	// that landed a record; a test pins it to model the idempotent shape,
	// where nothing was appended and the answer carries the revision this
	// node had already applied.
	rename func(pageID, title string) pages.Written

	// levels is every read level these tools asked for, so a tool that
	// stopped naming one — or named the wrong one — is visible. A seat
	// reads at `session` because it must see its own writes.
	levels []statelog.ReadLevel
}

func newFakeKB() *fakeKB {
	return &fakeKB{page: pages.Detail{
		Page:     pages.Page{ID: "p1", Container: "ENG", Title: "Deploy Runbook", Version: 4},
		Revision: 9,
	}}
}

func (f *fakeKB) List(_ context.Context, _ pages.Filter,
	fresh statelog.Freshness) (pages.Listing, error) {

	f.levels = append(f.levels, fresh.Level)
	if f.readErr != nil {
		return pages.Listing{}, f.readErr
	}
	return pages.Listing{
		Pages:    []pages.Summary{{ID: f.page.Page.ID, Title: f.page.Page.Title}},
		Level:    fresh.Level,
		Complete: true,
	}, nil
}

func (f *fakeKB) Get(_ context.Context, ref string,
	fresh statelog.Freshness) (pages.Detail, error) {

	f.levels = append(f.levels, fresh.Level)
	if f.readErr != nil {
		return pages.Detail{}, f.readErr
	}
	if ref == f.page.Page.ID || strings.EqualFold(ref, "ENG/Deploy Runbook") {
		return f.page, nil
	}
	return pages.Detail{}, pages.ErrNotFound
}

func (f *fakeKB) Create(_ context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error) {
	if f.writeErr != nil {
		return pages.Written{}, f.writeErr
	}
	f.created = append(f.created, in)
	f.actors = append(f.actors, actor)
	return pages.Written{Page: pages.Page{ID: "new", Container: in.Container,
		Title: in.Title, Version: 1}, Revision: 10}, nil
}

func (f *fakeKB) SavePage(_ context.Context, actor pages.Actor, _ string, save pages.Save) (pages.Written, error) {
	if f.writeErr != nil {
		return pages.Written{}, f.writeErr
	}
	f.saved = append(f.saved, save)
	f.actors = append(f.actors, actor)
	return pages.Written{
		Page: pages.Page{ID: "p1", Version: 5}, Revision: 11,
		Outcome: landedAt(11),
	}, nil
}

// renamed is one Rename call the fake took, with everything the fake had
// already been asked to wait for when it arrived — which is what makes the
// ORDER of the save, the settle and the rename assertable.
type renamed struct {
	pageID, title string
	awaitedBefore []uint64
}

func (f *fakeKB) Rename(_ context.Context, actor pages.Actor, pageID, title string,
	_ bool) (pages.Written, error) {

	if f.writeErr != nil {
		return pages.Written{}, f.writeErr
	}
	f.renames = append(f.renames, renamed{
		pageID: pageID, title: title,
		awaitedBefore: slices.Clone(f.awaited),
	})
	f.actors = append(f.actors, actor)
	return f.renameResult(pageID, title), nil
}

func (f *fakeKB) Comment(_ context.Context, actor pages.Actor, _ string, in pages.NewComment) (pages.Comment, pages.Written, error) {
	if f.writeErr != nil {
		return pages.Comment{}, pages.Written{}, f.writeErr
	}
	f.comments = append(f.comments, in)
	f.actors = append(f.actors, actor)
	return pages.Comment{ID: "m1", Mentions: in.Mentions},
		pages.Written{Page: pages.Page{ID: "p1"}, Revision: 12}, nil
}

// commentEdit is one EditComment call the fake took.
type commentEdit struct{ commentID, body string }

func (f *fakeKB) EditComment(_ context.Context, actor pages.Actor, _, commentID, body string) (pages.Comment, pages.Written, error) {
	if f.writeErr != nil {
		return pages.Comment{}, pages.Written{}, f.writeErr
	}
	f.edits = append(f.edits, commentEdit{commentID: commentID, body: body})
	f.actors = append(f.actors, actor)
	return pages.Comment{ID: commentID, Body: body},
		pages.Written{Page: pages.Page{ID: "p1"}, Revision: 13}, nil
}

// landedAt is a write that appended a record at one position on the pages log.
func landedAt(seq uint64) statelog.Result {
	return statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "CREWLET_PAGES_LOG", Seq: seq},
	}
}

// renameResult is the fake's answer to a rename: an ordinary one a record
// landed for, unless the test pinned another shape.
func (f *fakeKB) renameResult(pageID, title string) pages.Written {
	if f.rename != nil {
		return f.rename(pageID, title)
	}
	return pages.Written{
		Page: pages.Page{ID: pageID, Title: title, Version: 5}, Revision: 13,
		Outcome: landedAt(13),
	}
}

func (f *fakeKB) await(_ context.Context, at statelog.Position) error {
	f.awaited = append(f.awaited, at.Seq)
	return nil
}

func kbRegistry(t *testing.T, deps builtin.PageDeps) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Pages: deps}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// WRITING SOMETHING DOWN IS AN ANSWER. A turn asked to document a decision
// answers by writing the page, and without this the gate would correct it for
// having done exactly what was asked.
func TestThePageWritesCountAsDeliveries(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	deliveries := reg.Deliveries()
	for _, name := range builtin.PageWrites() {
		// AND ON THE KNOWLEDGE BASE, not just "somewhere". A page write
		// answers a turn asked to document something; it does not answer
		// somebody waiting in a chat thread, and the surface is what keeps
		// those two apart.
		if deliveries[name] != pages.Source {
			t.Errorf("%s delivers to %q, want %q", name, deliveries[name], pages.Source)
		}
	}
	for _, name := range []string{builtin.ListPagesTool, builtin.GetPageTool} {
		if _, ok := deliveries[name]; ok {
			t.Errorf("%s counts as a delivery, so a turn that only read would pass", name)
		}
	}
}

// A SAVE WITHOUT A BASE VERSION IS REFUSED BEFORE IT REACHES THE STORE, with
// a message that says what to do — the model's next call has to be right
// rather than another guess.
func TestASaveNeedsTheVersionItRead(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "body": "rewritten",
	})
	if !got.Failed {
		t.Fatal("a save with no base version was accepted")
	}
	if !strings.Contains(got.Output, "get_page") || !strings.Contains(got.Output, "silent overwrite") {
		t.Errorf("the refusal does not say what to do: %s", got.Output)
	}
	if len(kb.saved) != 0 {
		t.Errorf("the save reached the store anyway: %+v", kb.saved)
	}

	got = callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "rewritten", "message": "tightened",
	})
	if got.Failed {
		t.Fatalf("a proper save failed: %s", got.Output)
	}
	if len(kb.saved) != 1 || kb.saved[0].BaseVersion != 4 {
		t.Errorf("the base version did not reach the store: %+v", kb.saved)
	}
}

// A RESERVED CONTAINER IS REFUSED NAMING IT. A page written there is excluded
// from every search, so it would land somewhere no reader ever finds — and
// the seat would report the work as done.
func TestWritingToAReservedContainerIsRefused(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{
		Reader: kb, Writer: kb, Reserved: []string{"TS", "HOME"},
	})
	for _, container := range []string{"TS", "ts", "HOME"} {
		got := callWork(t, reg, builtin.WritePageTool, map[string]any{
			"title": "somewhere hidden", "body": "x", "container": container,
		})
		if !got.Failed {
			t.Errorf("a page was written into the reserved container %q", container)
			continue
		}
		if !strings.Contains(got.Output, "excluded from every search") {
			t.Errorf("the refusal does not say why: %s", got.Output)
		}
	}
	if len(kb.created) != 0 {
		t.Errorf("a reserved write reached the store: %+v", kb.created)
	}
}

// A TITLE COLLISION SENDS THE MODEL TO THE EXISTING PAGE. Two pages on one
// subject split what the company knows in half, and the next reader finds
// whichever they happen to search for.
func TestATitleCollisionPointsAtTheExistingPage(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.writeErr = pages.ErrTitleTaken
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	got := callWork(t, reg, builtin.WritePageTool, map[string]any{
		"title": "Deploy Runbook", "body": "x", "container": "ENG",
	})
	if !got.Failed {
		t.Fatal("a duplicate title was accepted")
	}
	for _, want := range []string{"get_page", "save_page", "second page on the same subject"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal omits %q: %s", want, got.Output)
		}
	}
}

// A STALE SAVE TELLS THE MODEL TO RE-BASE, which is the only recovery that
// does not lose somebody's paragraph.
func TestAStaleSaveTellsTheModelToReRead(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.writeErr = pages.ErrStaleVersion
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "x",
	})
	if !strings.Contains(got.Output, "re-apply your change") {
		t.Errorf("the refusal does not say how to recover: %s", got.Output)
	}
}

// A DRAFT IS NEVER LISTED. It is somebody's unfinished thought, and an agent
// given the option to list drafts would act on one.
func TestListingReturnsOnlyPublishedPages(t *testing.T) {
	t.Parallel()
	kb := &listRecorder{}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})
	callWork(t, reg, builtin.ListPagesTool, map[string]any{"container": "ENG"})
	if len(kb.filters) != 1 {
		t.Fatalf("filters = %v", kb.filters)
	}
	if !slices.Equal(kb.filters[0].Status, []pages.Status{pages.StatusPublished}) {
		t.Errorf("the listing asked for %v, want published only", kb.filters[0].Status)
	}
}

type listRecorder struct{ filters []pages.Filter }

func (l *listRecorder) List(_ context.Context, f pages.Filter,
	_ statelog.Freshness) (pages.Listing, error) {
	l.filters = append(l.filters, f)
	return pages.Listing{}, nil
}

func (l *listRecorder) Get(context.Context, string,
	statelog.Freshness,
) (pages.Detail, error) {
	return pages.Detail{}, pages.ErrNotFound
}

// A COMPANY RUNNING CONFLUENCE GETS NO NATIVE PAGE TOOLS.
func TestNoKnowledgeBaseMeansNoTools(t *testing.T) {
	t.Parallel()
	reg := kbRegistry(t, builtin.PageDeps{})
	for _, name := range builtin.PageTools() {
		if _, ok := reg.Lookup(name); ok {
			t.Errorf("%s was registered with no knowledge base configured", name)
		}
	}
}

// Identity comes from the turn, and a failed read never reads as empty.
func TestPageWritesAreAttributedAndFailuresAreHonest(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb, Mentions: fakeMentions{"pm"}})

	got := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{
		"page": "p1", "body": "@pm is this still right?",
	})
	if got.Failed {
		t.Fatalf("comment failed: %s", got.Output)
	}
	if len(kb.actors) == 0 || kb.actors[0].Handle != "eng" || kb.actors[0].Kind != pages.AuthorAgent {
		t.Errorf("attributed to %+v, want the turn's own seat", kb.actors)
	}
	if kb.comments[0].TurnKey != "turn-1" {
		t.Errorf("the comment carries turn key %q — a re-run turn would post twice",
			kb.comments[0].TurnKey)
	}
	if !slices.Equal(kb.comments[0].Mentions, []string{"pm"}) {
		t.Errorf("mentions = %v", kb.comments[0].Mentions)
	}

	kb.readErr = errors.New("the projection is not hydrated yet")
	for _, name := range []string{builtin.ListPagesTool, builtin.GetPageTool} {
		got := callWork(t, reg, name, map[string]any{"page": "p1"})
		if !got.Failed || !strings.Contains(got.Output, "NOT an empty result") {
			t.Errorf("%s on a failed read gave %q", name, got.Output)
		}
	}
}

// The annotations are what the sub-agent guard reads.
func TestThePageWritesAreClassifiedAsSharedWrites(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	for _, name := range builtin.PageWrites() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !mcp.WritesToSharedSurface(entry.Annotations) {
			t.Errorf("%s is not classified as a shared write", name)
		}
	}
	entry, _ := reg.Lookup(builtin.SavePageTool)
	if entry.Annotations.Destructive != mcp.Yes {
		t.Error("save_page is not marked destructive, though it replaces a body " +
			"somebody wrote")
	}
	for _, name := range []string{builtin.ListPagesTool, builtin.GetPageTool} {
		entry, _ := reg.Lookup(name)
		if !mcp.ReadOnlyProven(entry.Annotations) {
			t.Errorf("%s is not proven read-only", name)
		}
	}
}

// AN EDIT IS THE SAME TOOL, because it is the same gesture: a model
// correcting its own remark is putting words on a page, and a second name in
// the registry is a second thing to learn and a second thing to get wrong.
func TestCommentOnPageEditsWhenGivenACommentID(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	out := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{
		"page": "p1", "body": "a better thought", "edit": "m1",
	})
	if out.Failed {
		t.Fatalf("edit failed: %s", out.Output)
	}
	if len(kb.edits) != 1 {
		t.Fatalf("%d edits recorded, want 1 (comments: %d)", len(kb.edits), len(kb.comments))
	}
	if kb.edits[0].commentID != "m1" || kb.edits[0].body != "a better thought" {
		t.Errorf("edit = %+v", kb.edits[0])
	}
	// AND IT DID NOT ALSO POST ONE. An edit that added a comment beside the
	// one it edited would leave the page saying the thing twice.
	if len(kb.comments) != 0 {
		t.Errorf("the edit also posted %d new comment(s)", len(kb.comments))
	}
	if !strings.Contains(out.Output, `"edited"`) || !strings.Contains(out.Output, "true") {
		t.Errorf("the result does not say it edited: %s", out.Output)
	}
}

// WITHOUT `edit` IT STILL COMMENTS, which is the case that must not regress.
func TestCommentOnPageStillPostsWithoutAnEditID(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	if out := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{
		"page": "p1", "body": "a first thought",
	}); out.Failed {
		t.Fatalf("comment failed: %s", out.Output)
	}
	if len(kb.comments) != 1 || len(kb.edits) != 0 {
		t.Errorf("%d comments and %d edits, want 1 and 0", len(kb.comments), len(kb.edits))
	}
}

// EVERY PAGE READ A SEAT MAKES NAMES ITS LEVEL, and it is the seat surface's.
//
// A seat must see its own writes. A turn that created a page and then read the
// container back at whatever this node happened to hold would find the page
// missing and create it again — which is the duplicate the whole read contract
// exists to prevent, arriving through the tool surface rather than the API.
//
// Until the level reached the reader at all, every one of these calls was
// served from local rows and reported back at the level the caller had asked
// for, so the degradation was invisible in the answer. Naming `session` as a
// literal was the second half of that same bug: nothing populates a seat
// query's [statelog.Query.Session], so the level named a position the read
// never carried and degraded to the same local prefix under a stronger name.
func TestEveryPageToolReadsAtTheSeatDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.ListPagesTool, map[string]any{"container": "ENG"}},
		{builtin.GetPageTool, map[string]any{"page": "p1"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			kb := newFakeKB()
			reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
			if got := callWork(t, reg, tc.tool, tc.args); got.Failed {
				t.Fatalf("%s: %s", tc.tool, got.Output)
			}
			if len(kb.levels) == 0 {
				t.Fatalf("%s made no read, so this case proves nothing", tc.tool)
			}
			want := statelog.DefaultReadLevel(statelog.SurfaceSeat)
			for _, got := range kb.levels {
				if got != want {
					t.Errorf("%s read at %q, want %q — a seat that cannot see "+
						"its own writes files the duplicate",
						tc.tool, got, want)
				}
			}
		})
	}
}

// A LISTING THAT COULD NOT ACCOUNT FOR EVERYTHING SAYS SO TO THE MODEL.
//
// `complete: false` is the coverage answer, and it is a different fact from
// staleness: the rows may be missing pages a deferred record would have
// created. A model that reads a short list as the whole truth writes the
// duplicate, which is the same failure the level prevents through the other
// door.
func TestAnIncompleteListingTellsTheModel(t *testing.T) {
	t.Parallel()
	kb := &partialKB{}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})
	res := callWork(t, reg, builtin.ListPagesTool, map[string]any{"container": "ENG"})
	if res.Failed {
		t.Fatalf("list_pages: %s", res.Output)
	}
	if !strings.Contains(res.Output, `"complete"`) {
		t.Errorf("an incomplete listing rendered as a complete one:\n%s", res.Output)
	}

	// THE CONTROL: a complete listing does NOT carry the flag, or the
	// assertion above would pass on a tool that always renders it.
	full := newFakeKB()
	regFull := kbRegistry(t, builtin.PageDeps{Reader: full, Writer: full})
	resFull := callWork(t, regFull, builtin.ListPagesTool,
		map[string]any{"container": "ENG"})
	if resFull.Failed {
		t.Fatalf("list_pages (complete): %s", resFull.Output)
	}
	if strings.Contains(resFull.Output, `"complete"`) {
		t.Errorf("a complete listing carried the incompleteness flag:\n%s",
			resFull.Output)
	}
}

// partialKB answers a listing it could not account for.
type partialKB struct{ fakeKB }

func (p *partialKB) List(_ context.Context, _ pages.Filter,
	fresh statelog.Freshness,
) (pages.Listing, error) {
	return pages.Listing{
		Pages:    []pages.Summary{{ID: "p1", Title: "Deploy Runbook"}},
		Level:    fresh.Level,
		Complete: false,
	}, nil
}

// A SAVE THAT ALSO RENAMES REPORTS THE LATER OF THE TWO WRITES, AND WAITS FOR
// BOTH.
//
// save_page is two records — the content contends for the page and the address
// for the title, and one record cannot arbitrate both — so the tool makes two
// writes and has to combine their answers rather than let the second overwrite
// the first.
//
// THE RENAME'S ANSWER IS NOT ALWAYS THE LATER ONE. A rename to the title a
// page already displays appends no record at all: its position is zero and its
// revision is whatever this node had applied when it decided, which is BELOW
// the save's. Taking it outright told the model its edit was at a revision
// that predates the edit, and handed the settle the earlier position — so a
// re-read in the same turn could still show the page before either write.
func TestASaveThatAlsoRenamesReportsTheLaterWriteAndWaitsForBoth(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	// The idempotent rename: nothing appended, and the revision is the one
	// this node had already applied — three behind the save's.
	kb.rename = func(pageID, title string) pages.Written {
		return pages.Written{
			Page:     pages.Page{ID: pageID, Title: title, Version: 5},
			Revision: 8,
			Outcome:  pages.Written{}.Outcome,
		}
	}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb, Await: kb.await})

	got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "rewritten", "title": "Deploy Runbook",
	})
	if got.Failed {
		t.Fatalf("save: %s", got.Output)
	}
	if !strings.Contains(got.Output, `"revision": 11`) {
		t.Errorf("the result reports %s — a rename that appended nothing "+
			"carries the revision this node had BEFORE the save, and reporting "+
			"it tells the model its own edit is not there yet", got.Output)
	}
	if len(kb.renames) != 1 {
		t.Fatalf("%d renames reached the store", len(kb.renames))
	}
	// THE SAVE IS WAITED FOR BEFORE THE RENAME IS MADE. A rename decides
	// from this node's own applied rows, so one issued first reads the
	// pre-save head and answers with its version — which this tool hands
	// back as `version` and the model passes to the next save.
	if len(kb.renames[0].awaitedBefore) != 1 || kb.renames[0].awaitedBefore[0] != 11 {
		t.Errorf("the rename was made having waited for %v, want the save's "+
			"own position", kb.renames[0].awaitedBefore)
	}
}

// AND WHEN THE RENAME DOES LAND A RECORD, THAT IS WHAT THE TURN WAITS FOR.
//
// The rename is the second write, so its position is strictly later. Settling
// on the save's instead leaves a turn that renames and re-reads looking at the
// old title — the projection has caught up to the edit and not to the move.
func TestASaveThatAlsoRenamesWaitsForTheRenamesOwnPosition(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb, Await: kb.await})

	got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "rewritten", "title": "Deploy Guide",
	})
	if got.Failed {
		t.Fatalf("save: %s", got.Output)
	}
	if !strings.Contains(got.Output, `"revision": 13`) {
		t.Errorf("the result reports %s, want the rename's own revision", got.Output)
	}
	if len(kb.awaited) == 0 || kb.awaited[len(kb.awaited)-1] != 13 {
		t.Errorf("the turn waited for %v — the rename is the second write, so "+
			"a re-read in this turn has to be past ITS position, not the "+
			"save's", kb.awaited)
	}
}

// A PAGE REMARK MADE AGAIN AFTER A DIFFERENT ONE CARRIES ITS REPEAT COUNT, so
// the store derives a second comment rather than the first one's retry; a
// remark repeated with nothing between carries the same count and stays one.
func TestAPageRemarkCarriesItsRepeatCount(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	turn := workTurn(t)
	turn.Calls = turnctx.NewCallLog()
	surface := tools.NewSurface("execute", reg.Snapshot(),
		[]string{builtin.CommentOnPageTool}).ForTurn(turn)
	for _, body := range []string{"blocked", "blocked", "unblocked", "blocked"} {
		res, err := surface.Execute(t.Context(), llm.ToolCall{
			Name: builtin.CommentOnPageTool, Arguments: map[string]any{"page": "p1", "body": body},
		})
		if err != nil || res.Failed {
			t.Fatalf("comment %q = (%+v, %v)", body, res, err)
		}
	}
	var repeats []int
	for _, c := range kb.comments {
		repeats = append(repeats, c.Repeat)
	}
	if len(repeats) != 4 {
		t.Fatalf("four remarks reached the store as %d", len(repeats))
	}
	if repeats[1] != repeats[0] {
		t.Errorf("a remark repeated with nothing between carried %d after %d — "+
			"a retry would post it twice", repeats[1], repeats[0])
	}
	if repeats[3] == repeats[0] {
		t.Errorf("the remark made again after \"unblocked\" carried the first "+
			"one's count %d, so its id is the first one's and it is that one's "+
			"retry (counts %v)", repeats[0], repeats)
	}
}
