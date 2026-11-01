package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A SURFACE MISSING WHAT IT CANNOT SERVE WITHOUT IS NOT BUILT, and each
// refusal names the field. A surface with nothing to read its catalogue from,
// one wired with no authority decision, one that would write to the company
// and record nothing about who called it, and one without a seat control — or
// without any seam a control is required with, its announcer, its chart, its
// signed wire, its run records — are each a wiring mistake that would
// otherwise look exactly like a surface that worked: the last two a company
// told it cannot pause a seat that every node could pause, or offered a
// control that refuses every call as unavailable.
//
// AND EVERY SEAM OF A CONTROL'S DEPENDENCIES HAS ITS ROW, read off builtin's
// types themselves, but the actor the surface sets per call and a pause's
// clock, whose nil is the wall clock. New's refusals are a list written beside
// builtin's, and it missed the run records a note to a turn's coding run is
// filed on: steer_turn was listed, and refused every call, on a surface New
// built without them.
//
// Mutation: drop any one of New's refusals and its row is built; drop a seam's
// row and the walk over the types names it.
func TestNewRefusesASurfaceMissingARequirement(t *testing.T) {
	t.Parallel()
	halves := fixed(operator.Halves{})
	decide := builtin.Decide(authz.NoChart{})
	audit := &auditLog{}
	whole := operator.Options{Halves: halves, Authorize: decide, Audit: audit,
		Runs: builtin.RunDeps{Desk: nowhere{}},
		Pauses: builtin.SeatPauseDeps{Pauses: nowhere{}, Announce: nowhere{},
			Org: nowhereChart},
		Steer: builtin.SteerDeps{Asker: nowhere{}, Wire: nowhereWire, Runs: nowhere{}}}
	without := func(drop func(*operator.Options)) operator.Options {
		opts := whole
		drop(&opts)
		return opts
	}
	rows := map[string]struct {
		opts  operator.Options
		want  error
		names string
	}{
		"no halves": {without(func(o *operator.Options) { o.Halves = nil }),
			operator.ErrNoHalves, "Options.Halves"},
		"no decision": {without(func(o *operator.Options) { o.Authorize = nil }),
			operator.ErrNoAuthorize, "Options.Authorize"},
		"no audit": {without(func(o *operator.Options) { o.Audit = nil }),
			operator.ErrNoAudit, "Options.Audit"},
		"no run desk": {without(func(o *operator.Options) { o.Runs.Desk = nil }),
			operator.ErrNoSeatControls, "Options.Runs.Desk"},
		"no pause record": {without(func(o *operator.Options) { o.Pauses.Pauses = nil }),
			operator.ErrNoSeatControls, "Options.Pauses.Pauses"},
		"no pause announcer": {without(func(o *operator.Options) { o.Pauses.Announce = nil }),
			operator.ErrNoSeatControls, "Options.Pauses.Announce"},
		"no pause chart": {without(func(o *operator.Options) { o.Pauses.Org = nil }),
			operator.ErrNoSeatControls, "Options.Pauses.Org"},
		"no fleet asker": {without(func(o *operator.Options) { o.Steer.Asker = nil }),
			operator.ErrNoSeatControls, "Options.Steer.Asker"},
		"no steer wire": {without(func(o *operator.Options) { o.Steer.Wire = nil }),
			operator.ErrNoSeatControls, "Options.Steer.Wire"},
		"no run records": {without(func(o *operator.Options) { o.Steer.Runs = nil }),
			operator.ErrNoSeatControls, "Options.Steer.Runs"},
	}
	held := make(map[string]bool, len(rows))
	for name, tc := range rows {
		held[tc.names] = true
		s, err := operator.New(tc.opts)
		if !errors.Is(err, tc.want) || s != nil {
			t.Errorf("%s: answered (%v, %v), want %v", name, s, err, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s: refused %q, which does not name %s", name, err, tc.names)
		}
	}
	perCall := map[string]bool{"Runs.Actor": true, "Pauses.Actor": true,
		"Pauses.Now": true, "Steer.Actor": true}
	for control, deps := range map[string]any{"Runs": whole.Runs,
		"Pauses": whole.Pauses, "Steer": whole.Steer} {

		fields := reflect.TypeOf(deps)
		for i := range fields.NumField() {
			seam := control + "." + fields.Field(i).Name
			if !perCall[seam] && !held["Options."+seam] {
				t.Errorf("Options.%s is a seam of a seat control no row holds New to", seam)
			}
		}
	}
	// THE CONTROL: with every requirement met the surface is built, so each
	// refusal above is its own field's doing.
	if s, err := operator.New(whole); err != nil || s == nil {
		t.Fatalf("a surface with every requirement was refused: %v", err)
	}
}

// A COMPANY THAT KEEPS NEITHER HALF HERE IS STILL SERVED ITS SEAT CONTROLS.
//
// Answering a parked run, pausing and resuming a seat and steering a running
// turn read and write the fleet's own records, which every node reaches
// whatever its company runs: a company on Jira and Confluence pauses a seat
// here exactly as a native one does. So once the company is up the catalogue
// is never empty — the surface used to answer an empty one as the route's
// absence, a branch no engine could reach once these were wired — and an
// assistant connected to such a company is offered exactly these.
func TestACompanyWithNoNativeHalfIsStillServedItsSeatControls(t *testing.T) {
	t.Parallel()
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{})})
	want := []string{builtin.AnswerExecutorTool, builtin.PauseSeatTool,
		builtin.ResumeSeatTool, builtin.SteerTurnTool}
	slices.Sort(want)
	got := s.Tools()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("a company with no native half is served %v, want the seat controls %v",
			got, want)
	}
	sess := dialOperator(t, s, machine("token:ops", iam.GrantWorklet))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Errorf("over MCP the surface lists %v, want %v", names, want)
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
	founder := machine("token:founder", iam.GrantWorklet)
	serve := func(h http.Handler, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Idempotency-Key", statelog.NewOpID(time.Now(), ""))
		r.SetPathValue("tool", worklet.CreateWorkItemTool)
		r = r.WithContext(iam.WithPrincipal(r.Context(), founder))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	act := operator.ActPathPrefix + worklet.CreateWorkItemTool
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
	if !slices.Contains(s.Tools(), worklet.CreateWorkItemTool) {
		t.Fatalf("the company arrived and the surface still lists %v", s.Tools())
	}
	if rec := serve(s.ActHandler(), act, create); rec.Code != http.StatusOK {
		t.Errorf("the company arrived and the act answered %d %s", rec.Code, rec.Body)
	}
}

// THE TRACKER AND THE KNOWLEDGE BASE ARE SEPARATE HALVES. A company can run
// Worklet on Confluence, or Doclet on Jira, and a surface
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
	if !slices.Contains(names, worklet.CreateWorkItemTool) {
		t.Errorf("the tracker half serves %v, without create_work_item", names)
	}
	for _, name := range names {
		if strings.Contains(name, "page") {
			t.Errorf("a company with no Doclet was offered %q", name)
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
		kind      worklet.AuthorKind
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
			author: "token:ops-bot", kind: worklet.AuthorOperator, operator: "token:ops-bot",
		},
		// A PERSON THE DIRECTORY BINDS TO A SEAT writes as that seat, with
		// kind human — what lets the tracker leave them out of the wake
		// their own change sends — and the credential beside it.
		"a bound person": {
			principal: person("jane.founder", "jane-founder", iam.GrantWorklet),
			author:    "jane-founder", kind: worklet.AuthorHuman, operator: "jane.founder",
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
		operator.ActPathPrefix + worklet.CreateWorkItemTool,
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
		"act": {s.ActHandler(), operator.ActPathPrefix + worklet.CreateWorkItemTool},
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
			r.SetPathValue("tool", worklet.CreateWorkItemTool)
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
	for _, name := range []string{"reflect_and_persist", "use_skill", "a2a_ask"} {
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
	if got := s.Annotations(worklet.ListWorkItemsTool); !crewletmcp.ReadOnlyProven(got) {
		t.Errorf("the board read advertises %+v, which is not a proven read", got)
	}
	// AND A VERB THIS COMPANY IS NOT SERVED ADVERTISES NOTHING: answering
	// the hints for any name at all reported a verb that is not listed as
	// annotated, so a check for what is advertised passed on what is not.
	if slices.Contains(s.Tools(), worklet.SearchWorkItemsTool) {
		t.Fatalf("the ranked search is served with no search backend wired")
	}
	if got := s.Annotations(worklet.SearchWorkItemsTool); got != (tools.Annotations{}) {
		t.Errorf("a verb this surface does not serve is reported with hints %+v", got)
	}
	if got := s.Annotations(worklet.MergeWorkItemTool); got.Destructive != crewletmcp.Yes {
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

func (stubWorkReader) Tasks(context.Context, worklet.Query, time.Time) (worklet.Answer, error) {
	return worklet.Answer{}, nil
}

func (stubWorkReader) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {

	return worklet.TaskDetail{}, worklet.ErrNoTask
}

func (stubWorkReader) Views(context.Context, worklet.ViewQuery) (worklet.ViewListing, error) {
	return worklet.ViewListing{}, nil
}

func (stubWorkReader) ExpandedQuery(_ context.Context, params map[string]any,
	_ worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error) {

	return worklet.ParseQuery(worklet.MapParams(params), now, loc)
}

func (stubWorkReader) Catalogue(context.Context, worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return worklet.CatalogueAnswer{}, nil
}

func (stubWorkReader) Person(context.Context, worklet.PersonQuery, time.Time) (worklet.PersonState, error) {
	return worklet.PersonState{}, nil
}

func (stubWorkReader) Thread(context.Context, worklet.ThreadQuery,
	statelog.Freshness) (worklet.ResolvedThread, error) {
	return worklet.ResolvedThread{}, nil
}

type stubWorkWriterT struct{}

func stubWorkWriter(builtin.Actor) builtin.WorkWriter { return stubWorkWriterT{} }

// stubWorkMerger is the same stub in its second shape, for the fold — which is
// a SEQUENCE and therefore its own seam. See builtin.WorkMerger.
func stubWorkMerger(builtin.Actor) builtin.WorkMerger { return stubWorkWriterT{} }

func (stubWorkWriterT) MergeDuplicates(context.Context, string, string, string,
	bool, *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, nil
}

func (stubWorkWriterT) CreateTaskAsking(context.Context, string, worklet.Task,
	worklet.Comment, *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, nil
}

// stubWorkMover is its third shape, for the cross-project move.
func stubWorkMover(builtin.Actor) builtin.WorkMover { return stubWorkWriterT{} }

func (stubWorkWriterT) MoveTaskToProject(context.Context, string, string, string,
	*worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, nil
}

func (stubWorkWriterT) CreateTask(context.Context, string, worklet.Task,
	*worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, nil
}

func (stubWorkWriterT) UpdateTask(context.Context, string, string, string, uint64,
	worklet.TaskPatch, worklet.ChangeKind, *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, nil
}

type stubPageReader struct{}

func (stubPageReader) List(context.Context, doclet.Filter,
	statelog.Freshness) (doclet.Listing, error) {
	return doclet.Listing{}, nil
}

func (stubPageReader) Get(context.Context, string,
	statelog.Freshness) (doclet.Detail, error) {
	return doclet.Detail{}, doclet.ErrNotFound
}

// stubPageWriter is the knowledge base's write surface as this catalogue
// check needs it: present, so the surface is built, and never called.
type stubPageWriter struct{}

func (stubPageWriter) Create(context.Context, doclet.Actor, doclet.NewPage) (doclet.Written, error) {
	return doclet.Written{}, nil
}

func (stubPageWriter) SavePage(context.Context, doclet.Actor, string, doclet.Save) (doclet.Written, error) {
	return doclet.Written{}, nil
}

func (stubPageWriter) Rename(context.Context, doclet.Actor, string, string, bool) (doclet.Written, error) {
	return doclet.Written{}, nil
}

func (stubPageWriter) Comment(context.Context, doclet.Actor, string, doclet.NewComment) (doclet.Comment, doclet.Written, error) {
	return doclet.Comment{}, doclet.Written{}, nil
}

func (stubPageWriter) EditComment(context.Context, doclet.Actor, string, string,
	string) (doclet.Comment, doclet.Written, error) {
	return doclet.Comment{}, doclet.Written{}, nil
}
