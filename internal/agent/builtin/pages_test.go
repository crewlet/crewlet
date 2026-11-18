package builtin_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

type fakeKB struct {
	renames []renamed
	page    doclet.Detail

	created  []doclet.NewPage
	saved    []doclet.Save
	comments []doclet.NewComment
	edits    []commentEdit
	actors   []doclet.Actor

	// keys is every write's operation key ([doclet.Actor.OpKey]), in order,
	// so a key derived from anything but the caller's own, or none, is visible.
	keys []string

	readErr  error
	writeErr error

	// renameErr fails a rename alone, so a save that also renames lands its
	// edit and meets this on the half that follows.
	renameErr error

	// awaited is every position [builtin.PageDeps.Await] was handed, in
	// order, so waiting for the wrong write, or before making it, is visible.
	awaited []uint64

	// rename, when set, is what Rename answers; nil is an ordinary rename
	// that landed a record. A test sets it for the idempotent shape: nothing
	// appended, the revision the one this node had already applied.
	rename func(pageID, title string) doclet.Written

	// levels is every read level these tools asked for, so a missing or
	// wrong one is visible: a seat must see its own writes.
	levels []statelog.ReadLevel

	// outcome, when set, is every write's outcome: how a case makes a write
	// answer `unknown` without a broker.
	outcome *statelog.Result
}

// written is a write's answer, carrying the pinned outcome where there is one.
func (f *fakeKB) written(w doclet.Written) doclet.Written {
	if f.outcome != nil {
		w.Outcome = *f.outcome
	}
	return w
}

func newFakeKB() *fakeKB {
	return &fakeKB{page: doclet.Detail{
		Page:     doclet.Page{ID: "p1", Container: "ENG", Title: "Deploy Runbook", Version: 4},
		Revision: 9,
	}}
}

func (f *fakeKB) List(_ context.Context, _ doclet.Filter,
	fresh statelog.Freshness) (doclet.Listing, error) {

	f.levels = append(f.levels, fresh.Level)
	if f.readErr != nil {
		return doclet.Listing{}, f.readErr
	}
	return doclet.Listing{
		Pages: []doclet.Summary{{ID: f.page.Page.ID, Title: f.page.Page.Title,
			Author: f.page.Page.Author, Labels: f.page.Page.Labels}},
		Level:    fresh.Level,
		Complete: true,
	}, nil
}

func (f *fakeKB) Get(_ context.Context, ref string,
	fresh statelog.Freshness) (doclet.Detail, error) {

	f.levels = append(f.levels, fresh.Level)
	if f.readErr != nil {
		return doclet.Detail{}, f.readErr
	}
	if ref == f.page.Page.ID || strings.EqualFold(ref, "ENG/Deploy Runbook") {
		return f.page, nil
	}
	return doclet.Detail{}, doclet.ErrNotFound
}

func (f *fakeKB) Create(_ context.Context, actor doclet.Actor, in doclet.NewPage) (doclet.Written, error) {
	if f.writeErr != nil {
		return doclet.Written{}, f.writeErr
	}
	f.created = append(f.created, in)
	f.actors = append(f.actors, actor)
	f.keys = append(f.keys, actor.OpKey)
	return f.written(doclet.Written{Page: doclet.Page{ID: "new", Container: in.Container,
		Title: in.Title, Version: 1}, Revision: 10}), nil
}

func (f *fakeKB) SavePage(_ context.Context, actor doclet.Actor, _ string, save doclet.Save) (doclet.Written, error) {
	if f.writeErr != nil {
		return doclet.Written{}, f.writeErr
	}
	f.saved = append(f.saved, save)
	f.actors = append(f.actors, actor)
	f.keys = append(f.keys, actor.OpKey)
	return f.written(doclet.Written{
		Page: doclet.Page{ID: "p1", Version: 5}, Revision: 11,
		Outcome: landedAt(11),
	}), nil
}

// renamed is one Rename call, with every position the fake had been asked to
// wait for before it: what makes the ORDER of save, settle and rename assertable.
type renamed struct {
	pageID, title string
	awaitedBefore []uint64
}

func (f *fakeKB) Rename(_ context.Context, actor doclet.Actor, pageID, title string,
	_ bool) (doclet.Written, error) {

	if f.writeErr != nil {
		return doclet.Written{}, f.writeErr
	}
	if f.renameErr != nil {
		return doclet.Written{}, f.renameErr
	}
	f.renames = append(f.renames, renamed{
		pageID: pageID, title: title,
		awaitedBefore: slices.Clone(f.awaited),
	})
	f.actors = append(f.actors, actor)
	f.keys = append(f.keys, actor.OpKey)
	return f.renameResult(pageID, title), nil
}

func (f *fakeKB) Comment(_ context.Context, actor doclet.Actor, _ string, in doclet.NewComment) (doclet.Comment, doclet.Written, error) {
	if f.writeErr != nil {
		return doclet.Comment{}, doclet.Written{}, f.writeErr
	}
	f.comments = append(f.comments, in)
	f.actors = append(f.actors, actor)
	f.keys = append(f.keys, actor.OpKey)
	return doclet.Comment{ID: "m1", Mentions: in.Mentions},
		f.written(doclet.Written{Page: doclet.Page{ID: "p1"}, Revision: 12}), nil
}

// commentEdit is one EditComment call the fake took.
type commentEdit struct{ commentID, body string }

func (f *fakeKB) EditComment(_ context.Context, actor doclet.Actor, _, commentID,
	body string) (doclet.Comment, doclet.Written, error) {
	if f.writeErr != nil {
		return doclet.Comment{}, doclet.Written{}, f.writeErr
	}
	f.edits = append(f.edits, commentEdit{commentID: commentID, body: body})
	f.actors = append(f.actors, actor)
	f.keys = append(f.keys, actor.OpKey)
	return doclet.Comment{ID: commentID, Body: body},
		f.written(doclet.Written{Page: doclet.Page{ID: "p1"}, Revision: 13}), nil
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
func (f *fakeKB) renameResult(pageID, title string) doclet.Written {
	if f.rename != nil {
		return f.rename(pageID, title)
	}
	return doclet.Written{
		Page: doclet.Page{ID: pageID, Title: title, Version: 5}, Revision: 13,
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
	if _, err := builtin.Register(reg, gated(builtin.Deps{Pages: deps})); err != nil {
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
		// AND ON THE KNOWLEDGE BASE: a page write answers a turn asked to
		// document something, not somebody waiting in a chat thread, and the
		// surface keeps the two apart.
		if deliveries[name] != doclet.Source {
			t.Errorf("%s delivers to %q, want %q", name, deliveries[name], doclet.Source)
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

// A TITLE COLLISION SENDS THE MODEL TO THE EXISTING PAGE. Two pages on one
// subject split what the company knows in half, and the next reader finds
// whichever they happen to search for.
func TestATitleCollisionPointsAtTheExistingPage(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.writeErr = doclet.ErrTitleTaken
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
	kb.writeErr = doclet.ErrStaleVersion
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
	if !slices.Equal(kb.filters[0].Status, []doclet.Status{doclet.StatusPublished}) {
		t.Errorf("the listing asked for %v, want published only", kb.filters[0].Status)
	}
}

type listRecorder struct{ filters []doclet.Filter }

func (l *listRecorder) List(_ context.Context, f doclet.Filter,
	_ statelog.Freshness) (doclet.Listing, error) {
	l.filters = append(l.filters, f)
	return doclet.Listing{}, nil
}

func (l *listRecorder) Get(context.Context, string,
	statelog.Freshness,
) (doclet.Detail, error) {
	return doclet.Detail{}, doclet.ErrNotFound
}

// A COMPANY RUNNING CONFLUENCE GETS NO DOCLET PAGE TOOLS.
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

	got := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{"asks": false,
		"page": "p1", "body": "@pm is this still right?",
	})
	if got.Failed {
		t.Fatalf("comment failed: %s", got.Output)
	}
	if len(kb.actors) == 0 || kb.actors[0].Handle != "eng" || kb.actors[0].Kind != doclet.AuthorAgent {
		t.Errorf("attributed to %+v, want the turn's own seat", kb.actors)
	}
	// AND ITS OPERATION KEY IS THE TURN'S: an id the engine minted, the same
	// when the turn is re-run, so a re-run posts once.
	if len(kb.keys) == 0 || statelog.CheckCallerOpID(kb.keys[0]) != nil {
		t.Fatalf("the comment carries operation key %q, which is no id this "+
			"engine minted", kb.keys)
	}
	rerun := newFakeKB()
	if got := callWork(t, kbRegistry(t, builtin.PageDeps{Reader: rerun, Writer: rerun,
		Mentions: fakeMentions{"pm"}}), builtin.CommentOnPageTool, map[string]any{"asks": false,
		"page": "p1", "body": "@pm is this still right?",
	}); got.Failed || len(rerun.keys) != 1 || rerun.keys[0] != kb.keys[0] {
		t.Errorf("a re-run of the same turn's comment carried key %v, want %q — "+
			"a re-run turn would post twice", rerun.keys, kb.keys[0])
	}
	if !slices.Equal(kb.comments[0].Mentions, []string{"pm"}) {
		t.Errorf("mentions = %v", kb.comments[0].Mentions)
	}

	kb.readErr = errors.New("the projection is not hydrated yet")
	for _, name := range []string{builtin.ListPagesTool, builtin.GetPageTool} {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		got := callWork(t, reg, name, declaredOf(entry.Tool,
			map[string]any{"page": "p1"}))
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

// AN EDIT IS THE SAME TOOL, because it is the same gesture, putting words on a
// page: a second name in the registry is a second thing to learn and get wrong.
func TestCommentOnPageEditsWhenGivenACommentID(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	out := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{"asks": false,
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
	// AND IT DID NOT ALSO POST ONE, which would say the thing twice.
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

	if out := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{"asks": false,
		"page": "p1", "body": "a first thought",
	}); out.Failed {
		t.Fatalf("comment failed: %s", out.Output)
	}
	if len(kb.comments) != 1 || len(kb.edits) != 0 {
		t.Errorf("%d comments and %d edits, want 1 and 0", len(kb.comments), len(kb.edits))
	}
}

// EVERY PAGE READ A SEAT MAKES NAMES ITS LEVEL, the seat surface's default.
//
// A seat must see its own writes: a turn that created a page and read the
// container back at whatever this node held would find it missing and create
// it again. Not a literal `session` either: nothing populates a seat query's
// [statelog.Query.Session], so that level degrades to the local prefix under a
// stronger name.
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
// `complete: false` is coverage, not staleness: the rows may miss pages a
// deferred record would have created, and a model that reads a short list as
// the whole truth writes the duplicate.
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

func (p *partialKB) List(_ context.Context, _ doclet.Filter,
	fresh statelog.Freshness,
) (doclet.Listing, error) {
	return doclet.Listing{
		Pages:    []doclet.Summary{{ID: "p1", Title: "Deploy Runbook"}},
		Level:    fresh.Level,
		Complete: false,
	}, nil
}

// A SAVE THAT ALSO RENAMES REPORTS THE LATER OF THE TWO WRITES, AND WAITS FOR
// BOTH.
//
// save_page is two records (the content contends for the page, the address for
// the title), so the tool combines their answers. The rename's is not always
// the later: a rename to the title the page already has appends nothing, so its
// position is zero and its revision what this node had applied, BELOW the
// save's. Taken outright, it reports the edit at a revision predating it and
// settles on the earlier position.
func TestASaveThatAlsoRenamesReportsTheLaterWriteAndWaitsForBoth(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	// The idempotent rename: nothing appended, and the revision this node
	// had already applied, three behind the save's.
	kb.rename = func(pageID, title string) doclet.Written {
		return doclet.Written{
			Page:     doclet.Page{ID: pageID, Title: title, Version: 5},
			Revision: 8,
			Outcome:  doclet.Written{}.Outcome,
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
	// THE SAVE IS WAITED FOR BEFORE THE RENAME. A rename decides from this
	// node's applied rows, so one issued first answers with the pre-save
	// version, which the model passes to its next save.
	if len(kb.renames[0].awaitedBefore) != 1 || kb.renames[0].awaitedBefore[0] != 11 {
		t.Errorf("the rename was made having waited for %v, want the save's "+
			"own position", kb.renames[0].awaitedBefore)
	}
}

// AND WHEN THE RENAME LANDS A RECORD, THE TURN WAITS FOR IT: it is the second
// write, so settling on the save's leaves a re-read looking at the old title.
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

// A RENAME THROUGH save_page IS STILL A RENAME, and it is the container
// lead's.
//
// The tool's gate decides the SAVE, which any colleague may make; a `title` is
// [authz.ActionPageRename] on the stored page's container, or a colleague
// renames any page by editing it. The refusal lands NOTHING, the body half
// included: a caller refused part of a call finds no other part done. The
// control is the same edit without a title.
func TestARenameThroughSaveIsDecidedAsARename(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Pages:     builtin.PageDeps{Reader: kb, Writer: kb},
		Authorize: builtin.Decide(chartRefuses),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	renaming := callWorkAs(colleagueCaller(), t, reg, builtin.SavePageTool,
		map[string]any{"page": "p1", "base_version": 4, "body": "new",
			"title": "Deploy Runbook v2"})
	if !renaming.Failed || !errors.Is(renaming.Cause, builtin.ErrRefused) {
		t.Fatalf("a colleague who leads nothing renamed a page: %s",
			renaming.Output)
	}
	if len(kb.saved)+len(kb.renames) != 0 {
		t.Errorf("a refused rename still saved %d and renamed %d",
			len(kb.saved), len(kb.renames))
	}
	// THE CONTROL: the same colleague, the same edit, no title.
	if plain := callWorkAs(colleagueCaller(), t, reg, builtin.SavePageTool,
		map[string]any{"page": "p1", "base_version": 4, "body": "new"}); plain.Failed {
		t.Fatalf("the control was refused too, so the case asserts nothing: %s",
			plain.Output)
	}
}

// A DRAFT IS PUBLISHED THROUGH save_page, AND ONLY BY WHOEVER TIDIES THE PAGE.
//
// The status the call names reaches the store, the one place a draft is
// published. A status the page has moves nothing; none named leaves it, so an
// edit to a draft keeps it a draft; the trash is refused before anything lands.
// Moving the status decides whether the company reads the page, so it is
// [authz.ActionPagePublish] on the stored page, never the save. The refusal
// lands NOTHING, the body included; the control names the page's own status.
//
// Mutation: leave `status` out of the doclet.Save the tool builds and the
// publish saves nothing; skip the ask and a colleague who leads nothing
// publishes anybody's draft.
func TestADraftIsPublishedThroughSaveByWhoeverTidiesThePage(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.page.Page.Status = doclet.StatusDraft
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	published := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "status": "published"})
	if published.Failed {
		t.Fatalf("publishing a draft was refused: %s", published.Output)
	}
	if len(kb.saved) != 1 || kb.saved[0].Status == nil || *kb.saved[0].Status != doclet.StatusPublished {
		t.Fatalf("the store was handed %+v, want one save naming published", kb.saved)
	}
	if kb.saved[0].Body != nil {
		t.Errorf("a publish that sent no body saved one: %q", *kb.saved[0].Body)
	}

	// NO STATUS NAMED, NONE MOVED: the page keeps the one it has.
	callWork(t, reg, builtin.SavePageTool, map[string]any{"page": "p1", "base_version": 4, "body": "more"})
	if len(kb.saved) != 2 || kb.saved[1].Status != nil {
		t.Errorf("an edit that named no status handed the store %+v", kb.saved[1].Status)
	}

	// THE TRASH IS NOT A STATUS A SAVE NAMES, and neither is anything else.
	for _, status := range []any{"trashed", "archived", 7} {
		got := callWork(t, reg, builtin.SavePageTool, map[string]any{
			"page": "p1", "base_version": 4, "status": status})
		if !got.Failed || !strings.Contains(got.Output, "`published` or `draft`") {
			t.Errorf("a save naming status %v = %q, want it refused naming the two it takes", status, got.Output)
		}
	}
	if len(kb.saved) != 2 {
		t.Errorf("a refused status still saved: %d saves", len(kb.saved))
	}

	// DECIDED AS A PUBLISH, on the stored page.
	strict := newFakeKB()
	strict.page.Page.Status = doclet.StatusDraft
	held := tools.NewRegistry()
	if _, err := builtin.Register(held, builtin.Deps{
		Pages:     builtin.PageDeps{Reader: strict, Writer: strict},
		Authorize: builtin.Decide(chartRefuses),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	refused := callWorkAs(colleagueCaller(), t, held, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "new", "status": "published"})
	if !refused.Failed || !errors.Is(refused.Cause, builtin.ErrRefused) {
		t.Fatalf("a colleague who leads nothing published somebody's draft: %s", refused.Output)
	}
	if len(strict.saved) != 0 {
		t.Errorf("a refused publish still saved %d", len(strict.saved))
	}
	// THE CONTROL: the same colleague, the same edit, the status the page has.
	if plain := callWorkAs(colleagueCaller(), t, held, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "new", "status": "draft"}); plain.Failed {
		t.Fatalf("the control was refused too, so the case asserts nothing: %s", plain.Output)
	}
}

// A PAGE WRITE STATES ITS OUTCOME AND ITS POSITION, which a caller outside a
// turn (an assistant, the dashboard) hands back as `min_position` so its next
// read includes the write. A save that also renames answers for BOTH records:
// the later position and the less certain outcome, since `applied` over an
// unconfirmed rename would vouch for a write nobody can.
func TestAPageWriteAnswersItsOutcomeAndPosition(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.rename = func(pageID, title string) doclet.Written {
		return doclet.Written{
			Page: doclet.Page{ID: pageID, Title: title, Version: 5}, Revision: 13,
			Outcome: statelog.Result{
				Outcome:  statelog.OutcomePending,
				Position: statelog.Position{Stream: "CREWLET_PAGES_LOG", Seq: 13},
			},
		}
	}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb, Await: kb.await})

	saved := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "rewritten",
	})
	if !strings.Contains(saved.Output, `"outcome": "applied"`) ||
		!strings.Contains(saved.Output, `"position": "CREWLET_PAGES_LOG@0:11"`) {

		t.Errorf("a save answered %s, want its applied outcome at its own position", saved.Output)
	}
	both := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "rewritten", "title": "Deploy Guide",
	})
	if !strings.Contains(both.Output, `"outcome": "pending"`) ||
		!strings.Contains(both.Output, `"position": "CREWLET_PAGES_LOG@0:13"`) {

		t.Errorf("a save that also renamed answered %s, want the rename's pending "+
			"outcome at the rename's position", both.Output)
	}
}

// A LISTING CUT AT ITS LIMIT SAYS HOW MANY THERE ARE IN ALL, or a model reads
// fifty pages of four hundred as the whole container and writes a page that
// is already there.
func TestACutListingTellsTheModelTheTotal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		total int
		want  bool
	}{
		"cut at its limit": {total: 412, want: true},
		// THE CONTROL: a whole listing carries no total, or the case would
		// pass on a tool that always renders it.
		"whole": {total: 1, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kb := &cutKB{total: tc.total}
			reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})
			res := callWork(t, reg, builtin.ListPagesTool, map[string]any{"container": "ENG"})
			if res.Failed {
				t.Fatalf("list_pages: %s", res.Output)
			}
			if got := strings.Contains(res.Output, `"total"`); got != tc.want {
				t.Errorf("total rendered = %v, want %v:\n%s", got, tc.want, res.Output)
			}
		})
	}
}

// cutKB answers one page of a listing that matched `total`.
type cutKB struct {
	fakeKB
	total int
}

func (c *cutKB) List(_ context.Context, _ doclet.Filter,
	fresh statelog.Freshness,
) (doclet.Listing, error) {
	return doclet.Listing{
		Pages: []doclet.Summary{{ID: "p1", Title: "Deploy Runbook"}},
		Total: c.total, Level: fresh.Level, Complete: true,
	}, nil
}

// A PAGE BODY IS TOLD THE ADDRESS THE BACKLINKS READ. Left to guess, a seat
// writes `[[ENG/Title]]`, which nothing resolves. The example each writing
// tool carries must be one [doclet.Links] reads back, or the help teaches a
// link the target's "Linked from" never sees.
func TestAPageBodyIsToldTheLinkTheBacklinksRead(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	const id = "3f6c9a52-7d24-4517-83b0-39d026dd1cf2"
	example := regexp.MustCompile(`\[its title\]\(([^)\s]*)<page id>\)`)
	for _, name := range []string{builtin.WritePageTool, builtin.SavePageTool} {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
		body, _ := props["body"].(map[string]any)
		desc, _ := body["description"].(string)
		m := example.FindStringSubmatch(desc)
		if m == nil {
			t.Errorf("%s: body says nothing about how to link a page: %q", name, desc)
			continue
		}
		link := "[x](" + m[1] + id + ")"
		if got := doclet.Links("See " + link + "."); !slices.Equal(got, []string{id}) {
			t.Errorf("%s: the example link %s reads back as %v — the backlinks never see it",
				name, link, got)
		}
	}
}

// A PAGE REMARK MADE AGAIN AFTER A DIFFERENT ONE CARRIES ITS REPEAT COUNT in
// its operation key, so the store derives a second comment rather than the
// first one's retry; a remark repeated with nothing between carries the same
// key and stays one.
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
			Name: builtin.CommentOnPageTool, Arguments: map[string]any{"asks": false, "page": "p1", "body": body},
		})
		if err != nil || res.Failed {
			t.Fatalf("comment %q = (%+v, %v)", body, res, err)
		}
	}
	keys := kb.keys
	if len(keys) != 4 {
		t.Fatalf("four remarks reached the store as %d", len(keys))
	}
	if keys[1] != keys[0] {
		t.Errorf("a remark repeated with nothing between carried key %q after %q — "+
			"a retry would post it twice", keys[1], keys[0])
	}
	if keys[3] == keys[0] {
		t.Errorf("the remark made again after \"unblocked\" carried the first "+
			"one's key %q, so its id is the first one's and it is that one's "+
			"retry (keys %v)", keys[0], keys)
	}
}

// A PAGE REMARK CARRIES THE CALL'S OWN ARGUMENTS in its operation key, so a
// call that named the page another way is another call. The repeat count
// compares arguments as typed while the store's parts are what the call
// resolved to, so without the digest the last call below takes the "p1 "
// call's key and is answered with that comment's success, writing nothing.
// Held end to end in internal/doclet by
// TestASeatsRemarkAfterAnotherCallIsAnotherCommentHoweverItNamedThePage.
//
// Mutation: drop the digest from callOpKey's identity and keys[4] equals
// keys[2].
func TestAPageRemarkNamedAnotherWayIsAnotherCall(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	turn := workTurn(t)
	turn.Calls = turnctx.NewCallLog()
	surface := tools.NewSurface("execute", reg.Snapshot(),
		[]string{builtin.CommentOnPageTool}).ForTurn(turn)
	for _, call := range [][2]string{{"p1", "ok"}, {"p1", "ok"}, {"p1 ", "ok"},
		{"p1", "other"}, {"p1", "ok"}} {
		res, err := surface.Execute(t.Context(), llm.ToolCall{
			Name: builtin.CommentOnPageTool, Arguments: map[string]any{"asks": false, "page": call[0], "body": call[1]},
		})
		if err != nil || res.Failed {
			t.Fatalf("comment %q on %q = (%+v, %v)", call[1], call[0], res, err)
		}
	}
	keys := kb.keys
	if len(keys) != 5 {
		t.Fatalf("five remarks reached the store as %d", len(keys))
	}
	if keys[1] != keys[0] {
		t.Errorf("a remark repeated with nothing between carried key %q after %q", keys[1], keys[0])
	}
	if keys[2] == keys[0] {
		t.Errorf("a remark naming the page another way carried the first one's key %q", keys[0])
	}
	if keys[4] == keys[2] {
		t.Errorf("\"ok\" on p1 after other calls carried key %q, the one the call "+
			"naming \"p1 \" carried — it is that comment's retry and writes nothing "+
			"(keys %v)", keys[4], keys)
	}
}

// A PAGE WRITE WHOSE OUTCOME IS UNKNOWN IS NEVER REPORTED AS DONE: no id, no
// revision. A comment answered `unknown` WITHOUT publishing (its operation
// minted before this node's ledger lost rows, as a re-run of a turn whose
// trigger predates the ledger's sweep is) would otherwise read as posted, and
// nobody would see the remark.
func TestAPageWriteWhoseOutcomeIsUnknownIsNotReportedAsDone(t *testing.T) {
	t.Parallel()
	calls := map[string]map[string]any{
		builtin.WritePageTool: {"title": "New Page", "body": "text", "container": "ENG"},
		builtin.SavePageTool:  {"page": "p1", "body": "text", "base_version": 4},
		"comment":             {"page": "p1", "body": "a remark", "asks": false},
		"edit":                {"page": "p1", "body": "a remark", "edit": "m1"},
	}
	for _, unvouched := range []bool{false, true} {
		for name, args := range calls {
			kb := newFakeKB()
			kb.outcome = &statelog.Result{Outcome: statelog.OutcomeUnknown,
				OpID: "01a0c450-6c00-7011-a233-445566778899", Unvouched: unvouched}
			reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
			tool := name
			if name == "comment" || name == "edit" {
				tool = builtin.CommentOnPageTool
			}
			out := callWork(t, reg, tool, args)
			if !out.Failed {
				t.Errorf("%s (unvouched %v) with an unknown outcome reported success: %s",
					name, unvouched, out.Output)
				continue
			}
			for _, leak := range []string{`"comment_id"`, `"revision"`, `"id"`} {
				if strings.Contains(out.Output, leak) {
					t.Errorf("%s answered %s beside an unknown outcome: %s", name, leak,
						out.Output)
				}
			}
			if !strings.Contains(out.Output, "unknown") ||
				!strings.Contains(out.Output, "do not report it as done") {
				t.Errorf("%s never says the outcome is unknown: %s", name, out.Output)
			}
			if unvouched && !strings.Contains(out.Output, "cannot tell") {
				t.Errorf("%s's unvouched unknown never says this node cannot tell: %s",
					name, out.Output)
			}
		}
	}

	// WHAT A REPEAT IS DEPENDS ON THE CALLER. A seat's comment is derived
	// from its turn, so its repeat is the same comment (posted once under a
	// lost acknowledgement; unvouched, nothing published and the same
	// answer): SAFE, never "a second comment". An operator's has no turn to
	// derive from, so its repeat is a new one.
	operator := func(context.Context, *turnctx.Turn) (doclet.Actor, error) {
		return doclet.Actor{Handle: "founder", Kind: doclet.AuthorOperator,
			OperatorID: "founder"}, nil
	}
	for _, tc := range []struct {
		name      string
		operator  bool
		unvouched bool
		want      []string
		refuse    []string
	}{
		{name: "a seat under a lost acknowledgement",
			want: []string{"Call comment_on_page again with exactly the same arguments",
				"lands once"}},
		{name: "a seat the ledger cannot vouch for", unvouched: true,
			want: []string{"get_page before doing anything else",
				"call comment_on_page again with exactly the same arguments",
				"that is safe"},
			refuse: []string{"a second call is a new operation"}},
		{name: "an operator", operator: true,
			want:   []string{"get_page before making it again", "a second comment"},
			refuse: []string{"exactly the same arguments", "that is safe"}},
		{name: "an operator the ledger cannot vouch for", operator: true, unvouched: true,
			want:   []string{"get_page before making it again", "a second comment"},
			refuse: []string{"exactly the same arguments", "that is safe"}},
	} {
		kb := newFakeKB()
		kb.outcome = &statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op",
			Unvouched: tc.unvouched}
		deps := builtin.PageDeps{Reader: kb, Writer: kb}
		call := callWork
		if tc.operator {
			deps.Actor, call = operator, callNoTurn
		}
		out := call(t, kbRegistry(t, deps), builtin.CommentOnPageTool, calls["comment"])
		for _, want := range tc.want {
			if !strings.Contains(out.Output, want) {
				t.Errorf("%s: the unknown comment never says %q: %s", tc.name,
					want, out.Output)
			}
		}
		for _, bad := range tc.refuse {
			if strings.Contains(out.Output, bad) {
				t.Errorf("%s: the unknown comment says %q, which is not what "+
					"its repeat is: %s", tc.name, bad, out.Output)
			}
		}
	}
}

// AND ONE THAT LANDED SAYS HOW: its outcome and where it is durable, as every
// tracker write already does.
func TestAPageWriteReportsItsOutcomeAndPosition(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		builtin.WritePageTool:     {"title": "New Page", "body": "text", "container": "ENG"},
		builtin.SavePageTool:      {"page": "p1", "body": "text", "base_version": 4},
		builtin.CommentOnPageTool: {"asks": false, "page": "p1", "body": "a remark"},
	} {
		kb := newFakeKB()
		pending := landedAt(21)
		pending.Outcome = statelog.OutcomePending
		kb.outcome = &pending
		out := callWork(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb}), name, args)
		if out.Failed {
			t.Fatalf("%s failed: %s", name, out.Output)
		}
		if !strings.Contains(out.Output, `"outcome": "pending"`) ||
			!strings.Contains(out.Output, `"position": "CREWLET_PAGES_LOG`) {
			t.Errorf("%s does not report its outcome and position: %s", name, out.Output)
		}
	}
}

// A NEW PAGE IS WRITTEN PUBLISHED OR AS A DRAFT, and in no other status: a page
// written straight into the trash would be deleted before anybody read it.
func TestWritePageTakesADraftAndRefusesAnyOtherStatus(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

	for _, args := range []map[string]any{
		{"title": "Pricing", "body": "a thought", "container": "PROD", "status": "draft"},
		{"title": "Brief", "body": "settled", "container": "PROD"},
		{"title": "Roadmap", "body": "settled", "container": "PROD", "status": ""},
	} {
		if got := callWork(t, reg, builtin.WritePageTool, args); got.Failed {
			t.Fatalf("%v was refused: %s", args, got.Output)
		}
	}
	got := []doclet.Status{kb.created[0].Status, kb.created[1].Status, kb.created[2].Status}
	if !slices.Equal(got, []doclet.Status{doclet.StatusDraft, doclet.StatusPublished, doclet.StatusPublished}) {
		t.Fatalf("the pages were written as %v, want a draft and two published", got)
	}

	for _, status := range []any{"trashed", "archived", 7, true} {
		result := callWork(t, reg, builtin.WritePageTool, map[string]any{
			"title": "Elsewhere", "body": "x", "container": "PROD", "status": status,
		})
		if !result.Failed || !strings.Contains(result.Output, "`draft`") {
			t.Errorf("status %v was not refused naming the two it takes: %s", status, result.Output)
		}
	}
	if len(kb.created) != 3 {
		t.Errorf("a refused status reached the store: %d creates", len(kb.created))
	}
}

// A FOLDER IS A NEW PAGE WITH AN EMPTY BODY, and the tool says so on `body`
// rather than refusing the empty one.
func TestWritePageFilesAnEmptyBodyAsAFolder(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	if got := callWork(t, reg, builtin.WritePageTool, map[string]any{
		"title": "Designs", "body": "", "container": "ENG",
	}); got.Failed {
		t.Fatalf("an empty body was refused: %s", got.Output)
	}
	if len(kb.created) != 1 || kb.created[0].Body != "" {
		t.Fatalf("the folder reached the store as %+v", kb.created)
	}
	entry, _ := reg.Lookup(builtin.WritePageTool)
	body := entry.Tool.Parameters()["properties"].(map[string]any)["body"].(map[string]any)
	if !strings.Contains(body["description"].(string), "folder") {
		t.Errorf("write_page's body does not say an empty one is a folder: %q", body["description"])
	}
}

// A WATCH IS TRUE OR FALSE. Anything else is refused rather than read as "no
// change", which would tell a caller that sent "false" as text that the save
// landed while they still followed the page.
func TestSavePageRefusesAWatchThatIsNotABoolean(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	for _, watch := range []any{"false", 0, "yes"} {
		got := callWork(t, reg, builtin.SavePageTool, map[string]any{
			"page": "p1", "base_version": 4, "watch": watch,
		})
		if !got.Failed || !strings.Contains(got.Output, "true or false") {
			t.Errorf("watch %v was not refused: %s", watch, got.Output)
		}
	}
	if len(kb.saved) != 0 {
		t.Fatalf("a malformed watch reached the store: %+v", kb.saved)
	}
	if got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "watch": false,
	}); got.Failed {
		t.Fatalf("a proper unwatch failed: %s", got.Output)
	}
	if len(kb.saved) != 1 || kb.saved[0].Watch == nil || *kb.saved[0].Watch {
		t.Fatalf("the unwatch reached the store as %+v", kb.saved)
	}
}

// A RENAME IS TO THE TITLE THE CHECK READS (ADR-0059): one reader per declared
// type, so a string argument is read by argString, which writes a number as
// JSON does, never by fmt.Sprint, which writes a float by `%g`.
//
// Mutation: read the title with fmt.Sprint again and the rename is to "1e+06".
func TestARenameIsToTheTitleTheCheckReads(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	if got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "title": 1000000.0,
	}); got.Failed {
		t.Fatalf("the rename failed: %s", got.Output)
	}
	if len(kb.renames) != 1 || kb.renames[0].title != "1000000" {
		t.Errorf("the title 1000000 renamed the page as %+v, want \"1000000\"", kb.renames)
	}
}

// THE PAGE COMMENT TOOL SENDS A PERSON'S QUESTION TO A WORK ITEM: its words say
// `asks` reaches the agents named and that a person is asked with the work item
// tool, so an agent following them is never refused for asking a person here.
func TestThePageCommentToolSendsAPersonsQuestionToAWorkItem(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	entry, ok := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb}).Lookup(builtin.CommentOnPageTool)
	if !ok {
		t.Fatal("comment_on_page is not registered")
	}
	body := entry.Tool.Parameters()["properties"].(map[string]any)["body"].(map[string]any)["description"].(string)
	for name, said := range map[string]string{"the tool": entry.Tool.Description(), "body": body} {
		if !strings.Contains(said, builtin.CommentOnWorkTool) || strings.Contains(said, "anyone you @-mention") ||
			strings.Contains(said, "colleague by handle") {
			t.Errorf("%s says %q, want a person asked with %s and no invitation to ask anyone here",
				name, said, builtin.CommentOnWorkTool)
		}
	}
}
