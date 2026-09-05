package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
)

type fakeKB struct {
	page pages.Detail

	created  []pages.NewPage
	saved    []pages.Save
	comments []pages.NewComment
	actors   []pages.Actor

	readErr  error
	writeErr error
}

func newFakeKB() *fakeKB {
	return &fakeKB{page: pages.Detail{
		Page:     pages.Page{ID: "p1", Container: "ENG", Title: "Deploy Runbook", Version: 4},
		Revision: 9,
	}}
}

func (f *fakeKB) List(context.Context, pages.Filter) ([]pages.Summary, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return []pages.Summary{{ID: f.page.Page.ID, Title: f.page.Page.Title}}, nil
}

func (f *fakeKB) Get(_ context.Context, ref string) (pages.Detail, error) {
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
	return pages.Written{Page: pages.Page{ID: "p1", Version: 5}, Revision: 11}, nil
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
	deliverables := reg.Deliverables()
	for _, name := range builtin.PageWrites() {
		if !slices.Contains(deliverables, name) {
			t.Errorf("%s does not count as a delivery", name)
		}
	}
	for _, name := range []string{builtin.ListPagesTool, builtin.GetPageTool} {
		if slices.Contains(deliverables, name) {
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

func (l *listRecorder) List(_ context.Context, f pages.Filter) ([]pages.Summary, error) {
	l.filters = append(l.filters, f)
	return nil, nil
}

func (l *listRecorder) Get(context.Context, string) (pages.Detail, error) {
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
