package operator_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
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
	if s := operator.New(operator.Options{}); s != nil {
		t.Errorf("a company with no native backend got a surface serving %v", s.Tools())
	}
}

// THE TRACKER AND THE KNOWLEDGE BASE ARE SEPARATE GRANTS. A company can run
// the native tracker on Confluence, or the native wiki on Jira, and a surface
// that offered both halves whenever it had one would hand an assistant tools
// that fail at the call.
func TestEachHalfIsOfferedOnItsOwn(t *testing.T) {
	t.Parallel()
	only := operator.New(operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: operator.WorkActor(nil),
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

	actor, err := operator.WorkActor(nil)(ctx, nil)
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
	page, err := operator.PageActor(ctx, nil)
	if err != nil {
		t.Fatalf("PageActor: %v", err)
	}
	if page.Kind != pages.AuthorOperator || page.OperatorID != "ops-bot" {
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

// AND A BOUND TOKEN ALSO SAYS WHO IT IS, which is a different field from who
// it is attributed as.
//
// `contact.crewlet_operator_id` binds a Tier A token to a human seat, and that
// binding answers the question the attribution rule above deliberately does
// not: whose inbox, whose pins, whose queue. Left unresolved, a founder's own
// assistant marked a person record named after their credential and their own
// screen — which asks under their seat — showed an inbox where nothing had
// ever been read.
//
// THE AUTHOR IS ASSERTED IN THE SAME CASE, because the one wrong fix here is
// to let the seat take the author field over: that is a write attributed to a
// person the caller named, and a tracker whose author field is chosen by the
// writer is not an audit trail.
func TestABoundTokenCarriesTheSeatItNames(t *testing.T) {
	t.Parallel()
	chart := func() *org.Organization {
		o := &org.Organization{
			Name: "Nimbus",
			Roles: []*org.Role{
				{Name: "Jane Founder", Kind: org.KindHuman,
					Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
				{Name: "CTO"},
			},
		}
		o.Normalize()
		return o
	}

	bound, err := operator.WorkActor(chart)(auth.WithOperator(t.Context(), "founder"), nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if bound.Seat != "jane-founder" {
		t.Errorf("a bound token resolved to seat %q, want jane-founder — the "+
			"person tools key their record on this", bound.Seat)
	}
	if bound.Record() != "jane-founder" {
		t.Errorf("the record this token writes is %q, want the person's",
			bound.Record())
	}
	// THE ATTRIBUTION IS UNTOUCHED.
	if bound.Handle != "founder" || bound.Kind != tracker.AuthorOperator ||
		bound.OperatorID != "founder" {

		t.Errorf("a bound token is attributed as %+v — the author is the "+
			"credential and the kind says it is not a seat", bound)
	}
	// AND THE READ ASKS ABOUT BOTH NAMES, because the rows this person
	// left behind before the binding carry the credential's.
	if got := bound.Party().Handles(); len(got) != 2 ||
		got[0] != "jane-founder" || got[1] != "founder" {

		t.Errorf("the party is %v, want the seat first and the credential "+
			"behind it", got)
	}
	// AND THE KIND IS A PERSON'S, which is the OTHER thing an authority
	// gate asks: a lead relation is between two people in the chart, so
	// every gate resolves it for [builtin.Actor.Record] and then falls
	// back to [tracker.AuthorKind.Person] — the arm that carries a
	// re-route, a project's policy and somebody's queue when the walk
	// finds nothing. Kept `operator` rather than folded into `human`,
	// which is what keeps the author field honest, so the predicate is
	// what the gates have to turn on.
	if !bound.Kind.Person() {
		t.Errorf("a bound token's kind %q is not a person's, so every "+
			"authority gate refuses the person holding it", bound.Kind)
	}

	// AN UNBOUND TOKEN IS AN ORDINARY STATE — an operator outside the org
	// chart — and it writes under its own id exactly as before.
	unbound, err := operator.WorkActor(chart)(auth.WithOperator(t.Context(), "ci"), nil)
	if err != nil {
		t.Fatalf("WorkActor for an unbound token: %v", err)
	}
	if unbound.Seat != "" {
		t.Errorf("an unbound token resolved to seat %q", unbound.Seat)
	}
	if unbound.Record() != "ci" {
		t.Errorf("an unbound token writes %q's record, want its own", unbound.Record())
	}
	// AND IT IS A PERSON'S CREDENTIAL TOO, which is the only authority it
	// can ever hold: no ancestor walk reaches a name the chart does not
	// have, so without this arm an unbound token could not re-route an
	// item, declare a field or order a queue at all.
	if !unbound.Kind.Person() {
		t.Errorf("an unbound token's kind %q is not a person's", unbound.Kind)
	}

	// AND A BUILD WITH NO CHART LOADED IS THE SAME ORDINARY STATE rather
	// than a refusal: the surface is up before a company config is, and a
	// write with no writer is the only thing this surface may not record.
	for name, none := range map[string]func() *org.Organization{
		"no chart seam":   nil,
		"no chart loaded": func() *org.Organization { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := operator.WorkActor(none)(
				auth.WithOperator(t.Context(), "founder"), nil)
			if err != nil {
				t.Fatalf("WorkActor: %v", err)
			}
			if got.Seat != "" || got.Record() != "founder" {
				t.Errorf("with %s the actor is %+v", name, got)
			}
		})
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
			if _, err := operator.WorkActor(nil)(ctx, nil); err == nil {
				t.Error("a write with no operator was attributed rather than refused")
			}
			if _, err := operator.PageActor(ctx, nil); err == nil {
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
	if !auth.AlwaysGuarded(operator.MCPPath) {
		t.Fatalf("%s is not on the always-guarded list, so allow_anonymous_read "+
			"opens a surface that files work", operator.MCPPath)
	}
	if strings.HasPrefix(operator.MCPPath, "/mcp/") {
		t.Fatalf("%s is under the sandbox bridge's exempt prefix", operator.MCPPath)
	}
}

// EVERY TOOL AN OPERATOR IS OFFERED IS ONE A SEAT HAS. Not a subset check for
// tidiness: a name here that no seat tool answers would be a second
// implementation, which is what this whole seam exists to avoid.
func TestTheOperatorCatalogueIsDrawnFromTheSeatOne(t *testing.T) {
	t.Parallel()
	s := operator.New(operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: operator.WorkActor(nil),
		},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{}, Actor: operator.PageActor},
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

func (stubPageWriter) Rename(context.Context, pages.Actor, string, string, bool, pages.CallKey) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) Comment(context.Context, pages.Actor, string, pages.NewComment) (pages.Comment, pages.Written, error) {
	return pages.Comment{}, pages.Written{}, nil
}

func (stubPageWriter) EditComment(context.Context, pages.Actor, string, string, string,
	pages.CallKey) (pages.Comment, pages.Written, error) {
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
	s := operator.New(operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: operator.WorkActor(nil),
		},
		Pages: builtin.PageDeps{
			Reader: stubPageReader{}, Writer: stubPageWriter{},
			Actor: operator.PageActor,
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
	if got := s.Annotations(tracker.ListWorkItemsTool); !crewletmcp.ReadOnlyProven(got) {
		t.Errorf("the board read advertises %+v, which is not a proven "+
			"read", got)
	}
	// AND A VERB THIS COMPANY IS NOT SERVED ADVERTISES NOTHING. This case
	// once asserted the ranked search here, on a surface wired with no
	// search backend — and passed, because the hints were answered for any
	// name at all rather than for what the surface actually lists.
	if slices.Contains(s.Tools(), tracker.SearchWorkItemsTool) {
		t.Fatalf("the ranked search is served with no search backend wired")
	}
	if got := s.Annotations(tracker.SearchWorkItemsTool); got != (tools.Annotations{}) {
		t.Errorf("a verb this surface does not serve is reported with hints "+
			"%+v — a check for what is advertised passes on what is not", got)
	}
	if got := s.Annotations(tracker.MergeWorkItemTool); got.Destructive != crewletmcp.Yes {
		t.Errorf("a fold advertises %+v — it closes somebody's item on every "+
			"board and moves its subtasks, which is what the flag asks", got)
	}
}
