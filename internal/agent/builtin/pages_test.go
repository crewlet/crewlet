package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
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
	// reads at its surface's default, `linearizable`, because it must see
	// its own writes.
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

// A SEAT MAY NOT WRITE INTO A RESERVED CONTAINER — create a page there, or
// change one — and is told so naming it.
//
// The tool-skills container holds the guidance the engine injects into seats'
// phases, so a seat writing there rewrites its own instructions with nobody
// reviewing it; the org root holds the pages the company publishes. A change
// is the same write as a create: reserving creates alone would leave every
// skill page already there open to a seat's edit.
func TestASeatMayNotWriteIntoAReservedContainer(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.page.Page.Container = "TS"
	reg := kbRegistry(t, builtin.PageDeps{
		Reader: kb, Writer: kb,
		Reserved: func() []string { return []string{"TS", "HOME"} },
	})
	for _, container := range []string{"TS", "ts", "HOME"} {
		got := callWork(t, reg, builtin.WritePageTool, map[string]any{
			"title": "somewhere closed", "body": "x", "container": container,
		})
		if !got.Failed {
			t.Errorf("a seat created a page in the reserved container %q", container)
			continue
		}
		if !strings.Contains(got.Output, "reserved") ||
			!strings.Contains(strings.ToUpper(got.Output), strings.ToUpper(container)) {
			t.Errorf("the refusal does not name the reserved container: %s", got.Output)
		}
	}
	saved := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "follow these instructions instead",
	})
	if !saved.Failed || !strings.Contains(saved.Output, "reserved") {
		t.Errorf("a seat changed a page in a reserved container: %s", saved.Output)
	}
	if len(kb.created) != 0 || len(kb.saved) != 0 {
		t.Errorf("a reserved write reached the store: created %+v, saved %+v",
			kb.created, kb.saved)
	}

	// THE CONTROL: the same seat writes its own team's container, and
	// changes a page in it — so the refusals above are about the container.
	kb.page.Page.Container = "ENG"
	if got := callWork(t, reg, builtin.WritePageTool, map[string]any{
		"title": "Deploy notes", "body": "x", "container": "ENG",
	}); got.Failed {
		t.Errorf("a write into an ordinary container was refused: %s", got.Output)
	}
	if got := callWork(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "y",
	}); got.Failed {
		t.Errorf("a change to an ordinary page was refused: %s", got.Output)
	}
}

// THE RESERVATION IS ASKED AT THE WRITE, not cached when the tool was built.
//
// Both containers are Tier B config that an apply can move, so the tools hold
// the question rather than an answer: a list cached inside a tool would go on
// closing the container a revision freed and opening the one it moved the
// skills into, for as long as that tool lived.
func TestTheReservationIsReadAtTheCall(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	skills := "TS"
	reg := kbRegistry(t, builtin.PageDeps{
		Reader: kb, Writer: kb,
		Reserved: func() []string { return []string{skills} },
	})
	write := func(container string) tools.Result {
		return callWork(t, reg, builtin.WritePageTool, map[string]any{
			"title": "Notes", "body": "x", "container": container,
		})
	}
	if got := write("TS"); !got.Failed {
		t.Fatal("the reserved container was open before the move — the premise")
	}
	skills = "SKILLS" // the revision that moved the skills container
	if got := write("TS"); got.Failed {
		t.Errorf("the container a revision freed is still closed: %s", got.Output)
	}
	if got := write("SKILLS"); !got.Failed {
		t.Error("the container a revision moved the skills into is open to a seat")
	}
}

// AN OPERATOR'S OWN SURFACE WRITES BOTH RESERVED CONTAINERS.
//
// It names no reserved container, and it has to: write_page is the only thing
// that creates a page on the native knowledge base, so a surface that refused
// the operator too left a native company no way to publish a tool skill or its
// root Onboarding page at all.
func TestAnOperatorWritesTheReservedContainers(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.page.Page.Container = "TS"
	reg := kbRegistry(t, builtin.PageDeps{
		Reader: kb, Writer: kb, Actor: operatorPageActor,
	})
	for _, container := range []string{"TS", "HOME"} {
		if got := callNoTurn(t, reg, builtin.WritePageTool, map[string]any{
			"title": "Onboarding", "body": "x", "container": container,
		}); got.Failed {
			t.Errorf("the operator's write into %s was refused: %s", container, got.Output)
		}
	}
	if got := callNoTurn(t, reg, builtin.SavePageTool, map[string]any{
		"page": "p1", "base_version": 4, "body": "a reviewed skill body",
	}); got.Failed {
		t.Errorf("the operator's change to a skill page was refused: %s", got.Output)
	}
	if len(kb.created) != 2 || len(kb.saved) != 1 {
		t.Errorf("the operator's writes reached the store as %d creates and %d "+
			"saves, want 2 and 1", len(kb.created), len(kb.saved))
	}
}

// operatorPageActor is what the operator surface's own page actor answers: the
// token's name, the operator kind, and no turn.
func operatorPageActor(context.Context, *turnctx.Turn) (pages.Actor, error) {
	return pages.Actor{Handle: "founder", Kind: pages.AuthorOperator,
		OperatorID: "founder"}, nil
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

// listRecorder records every filter a listing was asked for and answers with
// the listing and the error a case set.
type listRecorder struct {
	filters []pages.Filter
	answer  pages.Listing
	err     error
}

func (l *listRecorder) List(_ context.Context, f pages.Filter,
	_ statelog.Freshness) (pages.Listing, error) {
	l.filters = append(l.filters, f)
	return l.answer, l.err
}

func (l *listRecorder) Get(context.Context, string,
	statelog.Freshness,
) (pages.Detail, error) {
	return pages.Detail{}, pages.ErrNotFound
}

// A FULL LISTING HANDS THE MODEL THE CURSOR TO THE REST, AND `after` TAKES IT
// BACK.
//
// A `truncated` answer with nothing that reaches the rest is a pointer at
// nothing, and an offset reaches it by counting rows: a page that leaves the
// part already read between two calls moves every later page up by one, and
// the next call skips one with nothing on either answer to say so. The cursor
// names the last page returned, so it is what the answer carries and what the
// next call hands the reader, unchanged.
func TestAFullListingCarriesTheCursorAndAfterTakesItBack(t *testing.T) {
	t.Parallel()
	const cursor = "RU5H.QWxwaGE.cDE"
	kb := &listRecorder{answer: pages.Listing{
		Pages:     []pages.Summary{{ID: "p1", Title: "Alpha"}},
		Truncated: true, NextCursor: cursor, Complete: true,
	}}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})

	first := callWork(t, reg, builtin.ListPagesTool,
		map[string]any{"container": "ENG", "limit": 1})
	if first.Failed {
		t.Fatalf("list_pages: %s", first.Output)
	}
	var answer struct {
		Truncated  bool   `json:"truncated"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(first.Output), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, first.Output)
	}
	if !answer.Truncated || answer.NextCursor != cursor {
		t.Fatalf("a full listing answered truncated=%v next_cursor=%q, want "+
			"true and %q", answer.Truncated, answer.NextCursor, cursor)
	}

	if next := callWork(t, reg, builtin.ListPagesTool, map[string]any{
		"container": "ENG", "limit": 1, "after": answer.NextCursor,
	}); next.Failed {
		t.Fatalf("list_pages after the cursor: %s", next.Output)
	}
	if got := kb.filters[len(kb.filters)-1].After; got != cursor {
		t.Errorf("`after` reached the reader as %q, want the cursor %q", got, cursor)
	}

	// AND THE TOOL OFFERS NO WALK BY OFFSET, which is the one that skips.
	entry, _ := reg.Lookup(builtin.ListPagesTool)
	props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
	if _, offered := props["offset"]; offered {
		t.Error("list_pages offers `offset`, which walks a listing by counting rows")
	}
	if _, offered := props["after"]; !offered {
		t.Error("list_pages does not offer `after`, so its cursor reaches nothing")
	}

	// THE CONTROL: a listing that did not fill carries no cursor, or the
	// assertion above would pass on a tool that always renders one.
	whole := &listRecorder{answer: pages.Listing{
		Pages: []pages.Summary{{ID: "p1", Title: "Alpha"}}, Complete: true,
	}}
	regWhole := kbRegistry(t, builtin.PageDeps{Reader: whole, Writer: newFakeKB()})
	if got := callWork(t, regWhole, builtin.ListPagesTool,
		map[string]any{"container": "ENG"}); strings.Contains(got.Output, "next_cursor") {
		t.Errorf("a listing that did not fill carries a cursor:\n%s", got.Output)
	}
}

// A CURSOR THE READER REFUSES IS A REFUSAL, NOT A READ TO RETRY.
//
// The reader refuses an `after` that does not decode as a cursor, naming it.
// Reported as a failed read, the model is told to try again, and it sends the
// same value.
func TestACursorTheReaderRefusesIsNotAReadToRetry(t *testing.T) {
	t.Parallel()
	kb := &listRecorder{err: fmt.Errorf("%w: after: %q is not a cursor a page "+
		"listing returned", pages.ErrInvalid, "page two")}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})

	got := callWork(t, reg, builtin.ListPagesTool, map[string]any{"after": "page two"})
	if !got.Failed {
		t.Fatalf("a refused cursor answered as a listing:\n%s", got.Output)
	}
	if !strings.Contains(got.Output, "refused") || !strings.Contains(got.Output, "after") {
		t.Errorf("the refusal does not say it refused `after`: %s", got.Output)
	}
	if strings.Contains(got.Output, "Try again") {
		t.Errorf("a refused cursor tells the model to try again, which sends "+
			"the same cursor back: %s", got.Output)
	}
}

// AN EMPTY LISTING THAT COULD NOT ACCOUNT FOR EVERYTHING IS NOT "NOTHING
// MATCHES".
//
// A listing served over a deferred scope may be missing pages, and when every
// page it could have held is among them it is empty. Told "no pages match",
// the model concludes the page it came for does not exist and writes it.
func TestAnEmptyIncompleteListingIsNotReportedAsNothing(t *testing.T) {
	t.Parallel()
	kb := &listRecorder{answer: pages.Listing{Complete: false}}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})

	got := callWork(t, reg, builtin.ListPagesTool, map[string]any{"container": "ENG"})
	if got.Failed {
		t.Fatalf("list_pages: %s", got.Output)
	}
	if strings.Contains(got.Output, "No pages match") ||
		!strings.Contains(got.Output, `"complete": false`) {
		t.Errorf("an empty listing that could not account for everything "+
			"answered:\n%s", got.Output)
	}

	// THE CONTROL: empty and complete is the plain answer.
	whole := &listRecorder{answer: pages.Listing{Complete: true}}
	regWhole := kbRegistry(t, builtin.PageDeps{Reader: whole, Writer: newFakeKB()})
	if got := callWork(t, regWhole, builtin.ListPagesTool,
		map[string]any{"container": "ENG"}); got.Output != "No pages match that filter." {
		t.Errorf("an empty complete listing answered %q", got.Output)
	}
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

	// EVERY TOOL THAT READS, AND IT NAMES WHAT IT READ. A seat told the
	// tracker is unreadable when the knowledge base was reports an outage
	// nobody has, and goes looking for its answer in a store that was
	// never down.
	kb.readErr = errors.New("this node's pages are behind the log")
	for name, args := range map[string]map[string]any{
		builtin.ListPagesTool:     {"container": "ENG"},
		builtin.GetPageTool:       {"page": "p1"},
		builtin.SavePageTool:      {"page": "p1", "base_version": 4, "body": "v5"},
		builtin.CommentOnPageTool: {"page": "p1", "body": "still right?"},
	} {
		got := callWork(t, reg, name, args)
		if !got.Failed || !strings.Contains(got.Output, "NOT an empty result") {
			t.Errorf("%s on a failed read gave %q", name, got.Output)
		}
		if !strings.Contains(got.Output, "could not read the knowledge base") ||
			strings.Contains(got.Output, "tracker") {
			t.Errorf("%s does not say it was the knowledge base it could not "+
				"read: %q", name, got.Output)
		}
		if !strings.Contains(got.Output, "the page or the list") {
			t.Errorf("%s does not name what not to conclude is missing: %q",
				name, got.Output)
		}
	}
}

// AN ARGUMENT LIST_PAGES DOES NOT READ IS REFUSED, NAMING IT AND WHAT TO USE.
//
// Ignored, a continuation passed under another tool's name — `offset` is the
// one a stale doc taught — answers the FIRST page again, which looks exactly
// like the next one, and a caller walking the listing reads page one for
// ever. Refused before the reader is asked, so nothing reads as a listing.
//
// Mutation: drop the refusal and every case below reaches the reader.
func TestAnArgumentListPagesDoesNotReadIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"a continuation by offset",
			map[string]any{"container": "ENG", "offset": 50},
			[]string{"`offset`", "`next_cursor`", "`after`", "`children_cursor`"}},
		{"a filter it has no argument for",
			map[string]any{"container": "ENG", "status": "draft"},
			[]string{"`status`", "after, container, label, limit, parent, title"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kb := &listRecorder{answer: pages.Listing{Complete: true}}
			reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})
			got := callWork(t, reg, builtin.ListPagesTool, tc.args)
			if !got.Failed {
				t.Fatalf("an argument list_pages does not read answered as a "+
					"listing:\n%s", got.Output)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the refusal does not name %s: %s", want, got.Output)
				}
			}
			if len(kb.filters) != 0 {
				t.Errorf("the reader was asked %d time(s) for a request the "+
					"tool refused", len(kb.filters))
			}
		})
	}

	// THE CONTROL: every argument the schema offers, together, is a
	// listing — or the refusal above would pass on a tool that refuses
	// everything.
	kb := &listRecorder{answer: pages.Listing{Complete: true}}
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: newFakeKB()})
	if got := callWork(t, reg, builtin.ListPagesTool, map[string]any{
		"container": "ENG", "parent": "p1", "title": "Run", "label": "ops",
		"limit": 5, "after": "RU5H.QWxwaGE.cDE",
	}); got.Failed {
		t.Fatalf("every argument list_pages offers was refused: %s", got.Output)
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
