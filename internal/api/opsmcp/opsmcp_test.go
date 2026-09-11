package opsmcp_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
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
	only := opsmcp.New(opsmcp.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Actor: opsmcp.WorkActor,
		},
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
	if actor.Kind != tracker.AuthorOperator {
		t.Errorf("an operator write is attributed as %q", actor.Kind)
	}
	if actor.OperatorID != "ops-bot" {
		t.Errorf("the record names the operator %q", actor.OperatorID)
	}
	// THE AUTHOR IS THE TOKEN'S OWN NAME, and the KIND is what says it is
	// not a seat. An empty author is the one thing this surface must never
	// record — a history row nobody can attribute — so the discriminator
	// is the kind, which every renderer and every recipient rule already
	// reads, rather than the emptiness of a string.
	if actor.Handle != "ops-bot" {
		t.Errorf("an operator write is authored by %q, want the token's name",
			actor.Handle)
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
	s := opsmcp.New(opsmcp.Options{
		Work:  builtin.WorkDeps{Reader: stubWorkReader{}, Writer: stubWorkWriter, Actor: opsmcp.WorkActor},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{}, Actor: opsmcp.PageActor},
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

// The tracker halves this surface needs to EXIST. What is under test here is
// which tools are offered and who a write is attributed to — neither of which
// reaches a store — so the stubs answer the shapes and nothing else.
type stubWorkReader struct{}

func (stubWorkReader) Tasks(context.Context, tracker.Query, time.Time) (tracker.Answer, error) {
	return tracker.Answer{}, nil
}

func (stubWorkReader) Task(context.Context, string, tracker.DetailWants,
	statelog.ReadLevel) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{}, tracker.ErrNoTask
}

func (stubWorkReader) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (stubWorkReader) Goals(context.Context, tracker.GoalQuery) (tracker.GoalListing, error) {
	return tracker.GoalListing{}, nil
}

type stubWorkWriterT struct{}

func stubWorkWriter(builtin.Actor) builtin.WorkWriter { return stubWorkWriterT{} }

func (stubWorkWriterT) CreateTask(context.Context, string, tracker.Task,
	*tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

func (stubWorkWriterT) UpdateTask(context.Context, string, string, string, uint64,
	tracker.TaskPatch, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

type stubPageReader struct{}

func (stubPageReader) List(context.Context, pages.Filter,
	statelog.ReadLevel) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (stubPageReader) Get(context.Context, string,
	statelog.ReadLevel) (pages.Detail, error) {
	return pages.Detail{}, pages.ErrNotFound
}

// stubPageWriter is the knowledge base's write surface as this catalogue
// check needs it: present, so the surface is built, and never called.
type stubPageWriter struct{}

func (stubPageWriter) Create(context.Context, pages.Actor, pages.NewPage) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) SavePage(context.Context, pages.Actor, string, pages.Save) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) Rename(context.Context, pages.Actor, string, string, bool) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) Comment(context.Context, pages.Actor, string, pages.NewComment) (pages.Comment, pages.Written, error) {
	return pages.Comment{}, pages.Written{}, nil
}

func (stubPageWriter) EditComment(context.Context, pages.Actor, string, string, string) (pages.Comment, pages.Written, error) {
	return pages.Comment{}, pages.Written{}, nil
}
