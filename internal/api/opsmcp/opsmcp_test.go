package opsmcp_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/work"
)

// A COMPANY ON JIRA GETS NO SURFACE AT ALL. An endpoint that exists and lists
// no tools reads to an operator as broken; one that is not there matches what
// their config says.
func TestNoNativeBackendServesNothing(t *testing.T) {
	t.Parallel()
	if s := opsmcp.New(opsmcp.Options{}); s != nil {
		t.Errorf("a company with no native backend got a surface serving %v", s.Tools())
	}
}

// THE TRACKER AND THE KNOWLEDGE BASE ARE SEPARATE GRANTS. A company can run
// the native tracker on Confluence, or the native wiki on Jira, and a surface
// that offered both halves whenever it had one would hand an assistant tools
// that fail at the call.
func TestEachHalfIsOfferedOnItsOwn(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	tracker, err := work.NewStore(work.Options{Documents: docs})
	if err != nil {
		t.Fatalf("work store: %v", err)
	}

	only := opsmcp.New(opsmcp.Options{
		Work: builtin.WorkDeps{Writer: tracker, Actor: opsmcp.WorkActor},
	})
	if only == nil {
		t.Fatal("a company with only the native tracker got no surface")
	}
	names := only.Tools()
	if !slices.Contains(names, "create_work_item") {
		t.Errorf("the tracker half serves %v, without create_work_item", names)
	}
	for _, name := range names {
		if strings.Contains(name, "page") {
			t.Errorf("a company with no native knowledge base was offered %q", name)
		}
	}
}

// A WRITE IS ATTRIBUTED TO THE CREDENTIAL, never to a seat and never to a
// name the caller chose.
//
// The alternative — letting the caller name a seat to act as — was rejected
// because it lets anybody holding the token write as anybody, and a tracker
// whose author field is chosen by the writer is not an audit trail.
func TestAnOperatorWriteCarriesTheTokensOwnLabel(t *testing.T) {
	t.Parallel()
	ctx := auth.WithOperator(t.Context(), "ops-bot")

	actor, err := opsmcp.WorkActor(ctx, nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if actor.Kind != work.AuthorOperator {
		t.Errorf("an operator write is attributed as %q", actor.Kind)
	}
	if actor.OperatorID != "ops-bot" {
		t.Errorf("the record names the operator %q", actor.OperatorID)
	}
	// THE HANDLE IS EMPTY, deliberately. A token is not a seat, and a
	// name in the handle field would render as a colleague in every
	// thread it appeared in.
	if actor.Handle != "" {
		t.Errorf("an operator write carries the handle %q, which reads as a seat", actor.Handle)
	}

	page, err := opsmcp.PageActor(ctx, nil)
	if err != nil {
		t.Fatalf("PageActor: %v", err)
	}
	if page.Kind != pages.AuthorOperator || page.OperatorID != "ops-bot" || page.Handle != "" {
		t.Errorf("a page write is attributed as %+v", page)
	}
}

// A REQUEST WITH NO OPERATOR IS REFUSED, not written as nobody. This surface
// writes to the company, and a write with no writer is the one thing it must
// never record — so the failure is at the actor rather than deeper, where it
// would already have landed.
func TestAWriteWithNoOperatorIsRefused(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]context.Context{
		"no operator on the context": context.Background(),
		"an empty operator id":       auth.WithOperator(context.Background(), ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := opsmcp.WorkActor(ctx, nil); err == nil {
				t.Error("a write with no operator was attributed rather than refused")
			}
			if _, err := opsmcp.PageActor(ctx, nil); err == nil {
				t.Error("a page write with no operator was attributed rather than refused")
			}
		})
	}
}

// THE SURFACE IS ALWAYS GUARDED, and it is the auth package that says so.
// Mounting it under /mcp/ — which is exempt wholesale so a sandbox box with
// no API token can reach its seat's tools — would have put a writable company
// surface behind no credential at all.
func TestTheOperatorSurfaceIsNeverAnonymous(t *testing.T) {
	t.Parallel()
	if !auth.AlwaysGuarded(opsmcp.Path) {
		t.Fatalf("%s is not on the always-guarded list, so allow_anonymous_read "+
			"opens a surface that files work", opsmcp.Path)
	}
	if strings.HasPrefix(opsmcp.Path, "/mcp/") {
		t.Fatalf("%s is under the sandbox bridge's exempt prefix", opsmcp.Path)
	}
}

// EVERY TOOL AN OPERATOR IS OFFERED IS ONE A SEAT HAS. Not a subset check for
// tidiness: a name here that no seat tool answers would be a second
// implementation, which is what this whole seam exists to avoid.
func TestTheOperatorCatalogueIsDrawnFromTheSeatOne(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	tracker, err := work.NewStore(work.Options{Documents: docs})
	if err != nil {
		t.Fatalf("work store: %v", err)
	}
	wiki, err := pages.NewStore(pages.Options{Documents: docs})
	if err != nil {
		t.Fatalf("pages store: %v", err)
	}
	s := opsmcp.New(opsmcp.Options{
		Work:  builtin.WorkDeps{Reader: stubWorkReader{}, Writer: tracker, Actor: opsmcp.WorkActor},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: wiki, Actor: opsmcp.PageActor},
	})
	if s == nil {
		t.Fatal("a company on both native backends got no surface")
	}
	seat := append(builtin.WorkWrites(), builtin.PageWrites()...)
	for _, name := range seat {
		if !slices.Contains(s.Tools(), name) {
			t.Errorf("a seat can call %q and an operator cannot", name)
		}
	}
	// AND THE TURN-ONLY TOOLS ARE ABSENT. A diary belongs to a seat, a
	// skill is loaded into a phase, and a colleague ask is answered by
	// waking a seat — there is nobody here for any of the three.
	for _, name := range []string{"reflect_and_persist", "use_skill", "a2a_ask", "run_sandbox"} {
		if slices.Contains(s.Tools(), name) {
			t.Errorf("the operator surface offers %q, which only means something inside a turn", name)
		}
	}
}

type stubWorkReader struct{}

func (stubWorkReader) List(context.Context, work.Filter) ([]work.Summary, error) { return nil, nil }
func (stubWorkReader) Get(context.Context, string) (work.Detail, error) {
	return work.Detail{}, work.ErrNotFound
}

type stubPageReader struct{}

func (stubPageReader) List(context.Context, pages.Filter) ([]pages.Summary, error) {
	return nil, nil
}

func (stubPageReader) Get(context.Context, string) (pages.Detail, error) {
	return pages.Detail{}, pages.ErrNotFound
}
