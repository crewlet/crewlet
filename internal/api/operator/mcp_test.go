package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SURFACE MISSING WHAT IT CANNOT SERVE WITHOUT IS NOT BUILT, and each
// refusal names the field. A surface with nothing to read its catalogue from,
// one wired with no authority decision, and one that would write to the
// company and record nothing about who called it are each a wiring mistake
// that would otherwise look exactly like a surface that worked.
func TestNewRefusesASurfaceMissingARequirement(t *testing.T) {
	t.Parallel()
	halves := fixed(operator.Halves{})
	decide := builtin.Decide(authz.NoChart{})
	audit := &auditLog{}
	for name, tc := range map[string]struct {
		opts operator.Options
		want error
	}{
		"no halves":   {operator.Options{Authorize: decide, Audit: audit}, operator.ErrNoHalves},
		"no decision": {operator.Options{Halves: halves, Audit: audit}, operator.ErrNoAuthorize},
		"no audit":    {operator.Options{Halves: halves, Authorize: decide}, operator.ErrNoAudit},
	} {
		s, err := operator.New(tc.opts)
		if !errors.Is(err, tc.want) || s != nil {
			t.Errorf("%s: answered (%v, %v), want %v", name, s, err, tc.want)
		}
	}
}

// A COMPANY THAT KEEPS NEITHER HALF HERE IS ANSWERED AS THE ROUTE'S ABSENCE.
// An endpoint that exists and lists no tools reads to an operator as broken;
// one that is not there matches what their config says.
func TestAnEmptyCatalogueAnswersAsTheRoutesAbsence(t *testing.T) {
	t.Parallel()
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{})})
	if got := s.Tools(); len(got) != 0 {
		t.Fatalf("a company with no native backend is served %v", got)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, operator.MCPPath, strings.NewReader(`{}`))
	r = r.WithContext(iam.WithPrincipal(r.Context(), machine("token:ops", iam.GrantStateRead)))
	s.MCPHandler().ServeHTTP(rec, r)
	if code := errorCode(t, rec); rec.Code != http.StatusNotFound ||
		code != string(httpjson.CodeNoRoute) {

		t.Errorf("an empty catalogue answered %d %q, want the mux's own 404 no_route",
			rec.Code, code)
	}
}

// THE CATALOGUE IS READ FOR EVERY CALL, never captured when the surface was
// built.
//
// A node's tracker and knowledge base come up with its FIRST company, which a
// node that booted with none meets at an apply long after its API started
// serving. Built once, the surface served the halves the node had at wiring —
// none — for the life of the process: the node a company was bootstrapped on
// never served its own operator surface until it restarted. Until the company
// arrives, every transport answers `503 no_active_revision`, which is what
// tells "not up yet" from "not this company's".
func TestTheCatalogueIsReadForEveryCall(t *testing.T) {
	t.Parallel()
	var up atomic.Bool
	s := newSurface(t, operator.Options{
		Halves: func() (operator.Halves, bool) {
			if !up.Load() {
				return operator.Halves{}, false
			}
			return operator.Halves{Work: builtin.WorkDeps{
				Reader: stubWorkReader{}, Writer: stubWorkWriter,
			}}, true
		},
	})
	founder := machine("token:founder", iam.GrantStateRead, iam.GrantWorkWrite)
	serve := func(h http.Handler, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Idempotency-Key", statelog.NewOpID(time.Now(), ""))
		r.SetPathValue("tool", tracker.CreateWorkItemTool)
		r = r.WithContext(iam.WithPrincipal(r.Context(), founder))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	act := operator.ActPathPrefix + tracker.CreateWorkItemTool
	create := `{"args":{"title":"x","project":"ENG"}}`

	if got := s.Tools(); got != nil {
		t.Errorf("a node with no company lists %v", got)
	}
	acts, err := s.Acts(t.Context(), founder)
	if err != nil || len(acts) != 0 || acts == nil {
		t.Errorf("a node with no company offers %v (%v), want an empty list", acts, err)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"mcp": serve(s.MCPHandler(), operator.MCPPath, `{}`),
		"act": serve(s.ActHandler(), act, create),
	} {
		if code := errorCode(t, rec); rec.Code != http.StatusServiceUnavailable ||
			code != string(httpjson.CodeNoActiveRevision) {

			t.Errorf("%s before the company answered %d %q, want 503 "+
				"no_active_revision", name, rec.Code, code)
		}
	}

	up.Store(true)
	if !slices.Contains(s.Tools(), tracker.CreateWorkItemTool) {
		t.Fatalf("the company arrived and the surface still lists %v", s.Tools())
	}
	if rec := serve(s.ActHandler(), act, create); rec.Code != http.StatusOK {
		t.Errorf("the company arrived and the act answered %d %s", rec.Code, rec.Body)
	}
}

// THE TRACKER AND THE KNOWLEDGE BASE ARE SEPARATE HALVES. A company can run
// the native tracker on Confluence, or the native wiki on Jira, and a surface
// that offered both halves whenever it had one would hand an assistant tools
// that fail at the call.
func TestEachHalfIsOfferedOnItsOwn(t *testing.T) {
	t.Parallel()
	only := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Moves: stubWorkMover,
		},
	})})
	names := only.Tools()
	if !slices.Contains(names, tracker.CreateWorkItemTool) {
		t.Errorf("the tracker half serves %v, without create_work_item", names)
	}
	for _, name := range names {
		if strings.Contains(name, "page") {
			t.Errorf("a company with no native knowledge base was offered %q", name)
		}
	}
}

// A WRITE IS ATTRIBUTED TO THE REQUEST'S PRINCIPAL, never to a name the caller
// chose — and the tracker and the knowledge base record one principal the same
// way.
//
// The alternative — letting the caller name a seat to act as — was rejected
// because it lets anybody holding a credential write as anybody, and a tracker
// whose author field is chosen by the writer is not an audit trail.
func TestAnOperatorWriteIsAttributedToItsPrincipal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		principal iam.Principal
		author    string
		kind      tracker.AuthorKind
		operator  string
	}{
		// A TIER A TOKEN, composed as the guard composes one: its whole
		// login, colon and all — the colon is what keeps the name out of
		// the seat namespace — and the KIND says it is not a seat.
		"an unbound token": {
			principal: iam.Principal{
				ID:    uuid.NewSHA1(auth.TokenNamespace, []byte("ops-bot")),
				Login: iam.TokenLogin("ops-bot"), Kind: iam.KindMachine,
				Stage: iam.StageActive,
			},
			author: "token:ops-bot", kind: tracker.AuthorOperator, operator: "token:ops-bot",
		},
		// A PERSON THE DIRECTORY BINDS TO A SEAT writes as that seat, with
		// kind human — what lets the tracker leave them out of the wake
		// their own change sends — and the credential beside it.
		"a bound person": {
			principal: person("jane.founder", "jane-founder", iam.GrantWorkWrite),
			author:    "jane-founder", kind: tracker.AuthorHuman, operator: "jane.founder",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := iam.WithPrincipal(t.Context(), tc.principal)
			actor, err := operator.WorkActor(ctx, nil)
			if err != nil {
				t.Fatalf("WorkActor: %v", err)
			}
			if actor.Handle != tc.author || actor.Kind != tc.kind ||
				actor.OperatorID != tc.operator {

				t.Errorf("a write is attributed as %+v, want %s (%s) through %s",
					actor, tc.author, tc.kind, tc.operator)
			}
			// AND THE KNOWLEDGE BASE RECORDS THE SAME AUTHOR THE SAME WAY:
			// the two histories are read TOGETHER on the audit screen, and
			// one person under two names there is two people to whoever
			// reads it. Through `Name`, which is what lands in the row.
			page, err := operator.PageActor(ctx, nil)
			if err != nil {
				t.Fatalf("PageActor: %v", err)
			}
			if string(page.Kind) != string(actor.Kind) || page.OperatorID != tc.operator {
				t.Errorf("a page write is attributed as %+v", page)
			}
			if got := page.Name(); got != actor.Handle {
				t.Errorf("a page history row records %q where the tracker records %q",
					got, actor.Handle)
			}
		})
	}
}

// A REQUEST WITH NO PRINCIPAL IS REFUSED, not written as nobody. This surface
// writes to the company, and a write with no writer is the one thing it must
// never record — so the failure is at the actor rather than deeper, where it
// would already have landed.
func TestAWriteWithNoPrincipalIsRefused(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]context.Context{
		"no principal on the context":  context.Background(),
		"a resolver that found nobody": iam.WithAnonymous(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := operator.WorkActor(ctx, nil); err == nil {
				t.Error("a write with no principal was attributed rather than refused")
			}
			if _, err := operator.PageActor(ctx, nil); err == nil {
				t.Error("a page write with no principal was attributed rather than refused")
			}
		})
	}
}

// THE SURFACE IS NEVER ANONYMOUS, and it is the auth package that says so.
// Mounting it under /mcp/ — which is exempt wholesale so a sandbox box with
// no API token can reach its seat's tools — would have put a writable company
// surface behind no credential at all.
//
// ASKED AS "IS IT EXEMPT" rather than "is it on the guarded list", because
// guarded is what a route IS: the only way this surface opens again is by
// landing on the exemption.
func TestTheOperatorSurfaceIsNeverAnonymous(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		operator.MCPPath, operator.MCPPath + "/", operator.MCPPath + "/tools",
		operator.ActPathPrefix + tracker.CreateWorkItemTool,
	} {
		if auth.Unguarded(path) {
			t.Errorf("%s is exempt from the guard, so a surface that files "+
				"work is reachable with no credential", path)
		}
		if strings.HasPrefix(path, "/mcp/") {
			t.Errorf("%s is under the sandbox bridge's exempt prefix", path)
		}
	}
}

// A REQUEST THE GUARD DID NOT ADMIT IS REFUSED IN THE ENGINE'S OWN WORDS, on
// both of this surface's HTTP transports: JSON with the guard's own code, never
// net/http's text/plain, and never served.
//
// TWO ANSWERS, because they send a caller opposite ways: nobody presented a
// credential this node accepts is `401 invalid_token`, and a request no
// resolver answered — a route mounted around the guard, or an identity this
// node could not read — is `503 identity_unavailable`, which must never send
// somebody to reset a working credential.
func TestARequestTheGuardDidNotAdmitIsRefusedAsJSON(t *testing.T) {
	t.Parallel()
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: stubWorkWriter},
	})})
	for transport, h := range map[string]struct {
		handler http.Handler
		path    string
	}{
		"mcp": {s.MCPHandler(), operator.MCPPath},
		"act": {s.ActHandler(), operator.ActPathPrefix + tracker.CreateWorkItemTool},
	} {
		for name, tc := range map[string]struct {
			ctx    func(context.Context) context.Context
			status int
			code   httpjson.Code
		}{
			"nobody":     {iam.WithAnonymous, http.StatusUnauthorized, httpjson.CodeInvalidToken},
			"unresolved": {func(c context.Context) context.Context { return c }, http.StatusServiceUnavailable, httpjson.CodeIdentityUnavailable},
		} {
			r := httptest.NewRequest(http.MethodPost, h.path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.SetPathValue("tool", tracker.CreateWorkItemTool)
			r = r.WithContext(tc.ctx(r.Context()))
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, r)
			if rec.Code != tc.status {
				t.Errorf("%s, %s: answered %d, want %d", transport, name, rec.Code, tc.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s, %s: the refusal is %q, want application/json",
					transport, name, ct)
			}
			if code := errorCode(t, rec); code != string(tc.code) {
				t.Errorf("%s, %s: the refusal is %q, want the code %s",
					transport, name, rec.Body.String(), tc.code)
			}
		}
	}
}

// EVERY TOOL AN OPERATOR IS OFFERED IS ONE A SEAT HAS. Not a subset check for
// tidiness: a name here that no seat tool answers would be a second
// implementation, which is what this whole seam exists to avoid.
func TestTheOperatorCatalogueIsDrawnFromTheSeatOne(t *testing.T) {
	t.Parallel()
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Moves: stubWorkMover,
		},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{}},
	})})
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

// TestEveryToolAnOperatorIsOfferedCarriesItsHints is the finding. This surface
// published a name, a description and a schema and NOTHING else, so an
// operator's own AI assistant — the premise of the whole endpoint — saw
// `search_work_items` and `remove_work_item` as identically unannotated. A
// client that asks before a destructive call had nothing to ask on, and a
// client that skips the prompt for a read prompted on every one.
func TestEveryToolAnOperatorIsOfferedCarriesItsHints(t *testing.T) {
	t.Parallel()
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Moves: stubWorkMover,
		},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{}},
	})})
	for _, name := range s.Tools() {
		if got := s.Annotations(name); got == (tools.Annotations{}) {
			t.Errorf("%q is advertised with no hints at all — a client "+
				"cannot tell it from an irreversible write", name)
		}
	}

	// AND THE TWO ENDS OF THE RANGE ARE WHAT THEY CLAIM, or the check
	// above would pass on a surface that annotated everything the same.
	if got := s.Annotations(tracker.ListWorkItemsTool); !crewletmcp.ReadOnlyProven(got) {
		t.Errorf("the board read advertises %+v, which is not a proven read", got)
	}
	// AND A VERB THIS COMPANY IS NOT SERVED ADVERTISES NOTHING: answering
	// the hints for any name at all reported a verb that is not listed as
	// annotated, so a check for what is advertised passed on what is not.
	if slices.Contains(s.Tools(), tracker.SearchWorkItemsTool) {
		t.Fatalf("the ranked search is served with no search backend wired")
	}
	if got := s.Annotations(tracker.SearchWorkItemsTool); got != (tools.Annotations{}) {
		t.Errorf("a verb this surface does not serve is reported with hints %+v", got)
	}
	if got := s.Annotations(tracker.MergeWorkItemTool); got.Destructive != crewletmcp.Yes {
		t.Errorf("a fold advertises %+v — it closes somebody's item on every "+
			"board and moves its subtasks, which is what the flag asks", got)
	}
}

// errorCode is the `error` of a JSON refusal, failing on a body that is not
// JSON.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answered %d with a body that is not JSON: %q", rec.Code, rec.Body)
	}
	code, _ := body["error"].(string)
	return code
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

func (stubWorkWriterT) CreateTaskAsking(context.Context, string, tracker.Task,
	tracker.Comment, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

// stubWorkMover is its third shape, for the cross-project move.
func stubWorkMover(builtin.Actor) builtin.WorkMover { return stubWorkWriterT{} }

func (stubWorkWriterT) MoveTaskToProject(context.Context, string, string, string,
	*tracker.Notify) (tracker.WriteResult, error) {

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

func (stubPageWriter) EditComment(context.Context, pages.Actor, string, string,
	string) (pages.Comment, pages.Written, error) {
	return pages.Comment{}, pages.Written{}, nil
}
