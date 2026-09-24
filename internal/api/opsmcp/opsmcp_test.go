package opsmcp_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
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
			Merges: stubWorkMerger, Actor: builtin.PrincipalActor,
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
	// A TIER A TOKEN, composed as the guard composes one.
	ctx := iam.WithPrincipal(t.Context(), iam.Principal{
		ID:    uuid.NewSHA1(auth.TokenNamespace, []byte("ops-bot")),
		Login: iam.TokenLogin("ops-bot"), Kind: iam.KindMachine,
		Stage: iam.StageActive,
	})

	actor, err := builtin.PrincipalActor(ctx, nil)
	if err != nil {
		t.Fatalf("PrincipalActor: %v", err)
	}
	if actor.Kind != tracker.AuthorOperator {
		t.Errorf("an operator write is attributed as %q", actor.Kind)
	}
	if actor.OperatorID != "token:ops-bot" {
		t.Errorf("the record names the operator %q", actor.OperatorID)
	}
	// THE AUTHOR IS THE TOKEN'S WHOLE LOGIN, colon and all: the colon is
	// what keeps the name out of the seat namespace, so the stripped id
	// would be a name a seat can also hold. And the KIND says it is not a
	// seat. An empty author is the one thing this surface must never
	// record — a history row nobody can attribute — so the discriminator
	// is the kind, which every renderer and every recipient rule already
	// reads, rather than the emptiness of a string.
	if actor.Handle != "token:ops-bot" {
		t.Errorf("an operator write is authored by %q, want the token's name",
			actor.Handle)
	}

	// AND THE KNOWLEDGE BASE RECORDS THE SAME OPERATOR THE SAME WAY, which
	// is the claim rather than the coincidence: the two histories are read
	// TOGETHER on the audit screen, and one person under two names there is
	// two people to whoever is reading it.
	//
	// It was not. The handle was left empty here, and `pages.Actor.Name`
	// falls back to `"operator:" + OperatorID` for an actor without one — so
	// a founder's work commit said `founder` and their page commit said
	// `operator:founder`, three rows apart in one feed, and a reader
	// filtering on a name matched half of what they did. The kind is already
	// its own column on both rows, so the prefix was a second encoding of a
	// fact the row carries.
	page, err := builtin.PrincipalPageActor(ctx, nil)
	if err != nil {
		t.Fatalf("PrincipalPageActor: %v", err)
	}
	if page.Kind != pages.AuthorOperator || page.OperatorID != "token:ops-bot" {
		t.Errorf("a page write is attributed as %+v", page)
	}
	if page.Handle != actor.Handle {
		t.Errorf("one operator is recorded as %q by the tracker and %q by the "+
			"knowledge base", actor.Handle, page.Handle)
	}
	// THROUGH `Name`, which is what actually lands in the history row: the
	// field agreeing is the mechanism, and the rendered name is the property.
	if got := page.Name(); got != actor.Handle {
		t.Errorf("a page history row records the author as %q where the tracker "+
			"records %q — the audit feed reads both and shows one person twice",
			got, actor.Handle)
	}
}

// A REQUEST WITH NO OPERATOR IS REFUSED, not written as nobody. This surface
// writes to the company, and a write with no writer is the one thing it must
// never record — so the failure is at the actor rather than deeper, where it
// would already have landed.
func TestAWriteWithNoOperatorIsRefused(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]context.Context{
		"no operator on the context":   context.Background(),
		"a resolver that found nobody": iam.WithAnonymous(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := builtin.PrincipalActor(ctx, nil); err == nil {
				t.Error("a write with no operator was attributed rather than refused")
			}
			if _, err := builtin.PrincipalPageActor(ctx, nil); err == nil {
				t.Error("a page write with no operator was attributed rather than refused")
			}
		})
	}
}

// THE SURFACE IS NEVER ANONYMOUS, and it is the auth package that says so.
// Mounting it under /mcp/ — which is exempt wholesale so a sandbox box with
// no API token can reach its seat's tools — would have put a writable company
// surface behind no credential at all.
//
// ASKED AS "IS IT EXEMPT" rather than "is it on the guarded list", because the
// list is gone: guarded is what a route IS now, and the only way this surface
// opens again is by landing on the exemption.
func TestTheOperatorSurfaceIsNeverAnonymous(t *testing.T) {
	t.Parallel()
	for _, path := range []string{opsmcp.Path, opsmcp.Path + "/", opsmcp.Path + "/tools"} {
		if auth.Unguarded(path) {
			t.Fatalf("%s is exempt from the guard, so a surface that files "+
				"work is reachable with no credential", path)
		}
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
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: builtin.PrincipalActor,
		},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{}, Actor: builtin.PrincipalPageActor},
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
	statelog.Freshness) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{}, tracker.ErrNoTask
}

func (stubWorkReader) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (stubWorkReader) ExpandedQuery(_ context.Context, params map[string]any,
	_ tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (stubWorkReader) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (stubWorkReader) Person(context.Context, tracker.PersonQuery, time.Time) (tracker.PersonState, error) {
	return tracker.PersonState{}, nil
}

func (stubWorkReader) Thread(context.Context, tracker.ThreadQuery,
	statelog.Freshness) (tracker.ResolvedThread, error) {
	return tracker.ResolvedThread{}, nil
}

type stubWorkWriterT struct{}

func stubWorkWriter(builtin.Actor) builtin.WorkWriter { return stubWorkWriterT{} }

// stubWorkMerger is the same stub in its second shape, for the fold — which is
// a SEQUENCE and therefore its own seam. See builtin.WorkMerger.
func stubWorkMerger(builtin.Actor) builtin.WorkMerger { return stubWorkWriterT{} }

func (stubWorkWriterT) MergeDuplicates(context.Context, string, string, string,
	bool, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

func (stubWorkWriterT) CreateTask(context.Context, string, tracker.Task,
	*tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

func (stubWorkWriterT) UpdateTask(context.Context, string, string, string, uint64,
	tracker.TaskPatch, tracker.ChangeKind, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

type stubPageReader struct{}

func (stubPageReader) List(context.Context, pages.Filter,
	statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (stubPageReader) Get(context.Context, string,
	statelog.Freshness) (pages.Detail, error) {
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

// TestEveryToolAnOperatorIsOfferedCarriesItsHints is the finding. This surface
// published a name, a description and a schema and NOTHING else, so an
// operator's own AI assistant — the premise of the whole endpoint — saw
// `search_work_items` and `remove_work_item` as identically unannotated. A
// client that asks before a destructive call had nothing to ask on, and a
// client that skips the prompt for a read prompted on every one.
func TestEveryToolAnOperatorIsOfferedCarriesItsHints(t *testing.T) {
	t.Parallel()
	s := opsmcp.New(opsmcp.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: builtin.PrincipalActor,
		},
		Pages: builtin.PageDeps{
			Reader: stubPageReader{}, Writer: stubPageWriter{},
			Actor: builtin.PrincipalPageActor,
		},
	})
	if s == nil {
		t.Fatal("a company on both native backends got no surface")
	}
	for _, name := range s.Tools() {
		if got := s.Annotations(name); got == (tools.Annotations{}) {
			t.Errorf("%q is advertised with no hints at all — a client "+
				"cannot tell it from an irreversible write", name)
		}
	}

	// AND THE TWO ENDS OF THE RANGE ARE WHAT THEY CLAIM, or the check
	// above would pass on a surface that annotated everything the same.
	if got := s.Annotations(tracker.SearchWorkItemsTool); !crewletmcp.ReadOnlyProven(got) {
		t.Errorf("the ranked search advertises %+v, which is not a proven "+
			"read", got)
	}
	if got := s.Annotations(tracker.MergeWorkItemTool); got.Destructive != crewletmcp.Yes {
		t.Errorf("a fold advertises %+v — it closes somebody's item on every "+
			"board and moves its subtasks, which is what the flag asks", got)
	}
}
