package operator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/config"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The act transport is the dashboard's write path, and every case here turns
// on its one rule: a write is made by the PERSON the presented token is bound
// to, and by nobody else (ADR-0024). The cases drive the real handler behind
// the real bearer guard, because "who reached it" is decided by the guard and
// "who may act" by the handler, and a case that stood either in would prove
// the half it did not stand in for.

// A request id a client minted, and a second one.
const (
	requestA = "0f7c1a4e-9b2d-4e51-8c3a-6d7e8f9a0b1c"
	requestB = "5a1d2c3b-7e6f-4a9b-8c0d-1e2f3a4b5c6d"
)

// boundChart binds the token `founder` to Jane Founder's seat and leaves the
// token `ci` bound to nobody. It ALSO names the reserved anonymous id on a
// seat, as a chart built without validation could — the transport must still
// see nobody there.
func boundChart() *org.Organization {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Jane Founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
			{Name: "Night Shift", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: auth.AnonymousOperator}},
			{Name: "CTO"},
		},
	}
	o.Normalize()
	return o
}

// recordingWork is a tracker writer that records every create it is asked
// for — the actor the surface resolved and the operation id it derived — and
// answers with a position, as the real writer does.
type recordingWork struct {
	mu      sync.Mutex
	actors  []builtin.Actor
	opIDs   []string
	outcome statelog.Outcome
}

type recordingWriter struct {
	w     *recordingWork
	actor builtin.Actor
}

func (r *recordingWork) writer(actor builtin.Actor) builtin.WorkWriter {
	return recordingWriter{w: r, actor: actor}
}

func (w recordingWriter) CreateTaskAsking(ctx context.Context, opID string,
	task tracker.Task, _ tracker.Comment, notify *tracker.Notify) (tracker.WriteResult, error) {

	return w.CreateTask(ctx, opID, task, notify)
}

func (w recordingWriter) CreateTask(_ context.Context, opID string, _ tracker.Task,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	w.w.actors = append(w.w.actors, w.actor)
	w.w.opIDs = append(w.w.opIDs, opID)
	outcome := w.w.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	return tracker.WriteResult{
		Result: statelog.Result{
			Outcome:  outcome,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4711},
			Version:  4711,
		},
		Key: "ENG-9",
	}, nil
}

func (w recordingWriter) UpdateTask(context.Context, string, string, string, uint64,
	tracker.TaskPatch, tracker.ChangeKind, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

func (r *recordingWork) writes() ([]builtin.Actor, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]builtin.Actor(nil), r.actors...), append([]string(nil), r.opIDs...)
}

// actSurface is the operator surface over a recording tracker and a stub
// knowledge base, bound by [boundChart].
func actSurface(t *testing.T, work *recordingWork) *operator.Server {
	t.Helper()
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: work.writer,
			Actor: operator.WorkActor(boundChart),
		},
		Pages: builtin.PageDeps{
			Reader: stubPageReader{}, Writer: stubPageWriter{},
			Actor: operator.PageActor,
		},
		Org: boundChart,
	})
	if s == nil {
		t.Fatal("a company on both native backends got no surface")
	}
	return s
}

// guarded is the act route behind the real bearer guard, as the app mounts
// it: `founder`, `cofounder` and `ci` are all valid tokens, and `ci` is
// nobody's.
func guarded(s *operator.Server, disabled bool) http.Handler {
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder", Token: "founder-secret"},
		{ID: "cofounder", Token: "cofounder-secret"},
		{ID: "ci", Token: "ci-secret"},
	}
	b.API.Auth.Disabled = disabled
	mux := http.NewServeMux()
	mux.Handle(operator.ActPattern, s.ActHandler())
	return auth.New(&b).Middleware(mux)
}

// act posts one request and decodes the JSON answer.
func act(t *testing.T, h http.Handler, token, tool, contentType, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+tool, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var answer map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s answered %d with a body that is not JSON: %q", tool, rec.Code, rec.Body)
		}
	}
	return rec.Code, answer
}

// createBody is one create_work_item request under a request id.
func createBody(requestID string) string {
	raw, _ := json.Marshal(map[string]any{
		"request_id": requestID,
		"args": map[string]any{
			"title": "Rotate the signing key", "project": "ENG",
		},
	})
	return string(raw)
}

// NOBODY BUT A PERSON ACTS. A disabled guard's caller is nobody — even on a
// chart that names the reserved id, which the chart's own validation refuses
// and this transport must not depend on — and a token no seat binds is a
// credential acting as itself, which is what /operator/mcp is for. Each is
// refused `unbound`, naming the remedy that fits it, and the tracker is never
// reached. The bound token beside them is served, so a transport that refused
// everybody fails here too.
func TestAnUnboundOrAnonymousCallerCannotAct(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, token string
		disabled    bool
		status      int
		code        httpjson.Code
		remedy      string
	}{
		{"a disabled guard's caller", "", true, http.StatusForbidden, operator.CodeUnbound, "api.auth"},
		{"an unbound token", "ci-secret", false, http.StatusForbidden, operator.CodeUnbound,
			"contact.crewlet_operator_id: ci"},
		{"no credential", "", false, http.StatusUnauthorized, httpjson.CodeInvalidToken, ""},
		{"a wrong credential", "guess", false, http.StatusUnauthorized, httpjson.CodeInvalidToken, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := &recordingWork{}
			h := guarded(actSurface(t, work), tc.disabled)
			status, answer := act(t, h, tc.token, tracker.CreateWorkItemTool,
				"application/json", createBody(requestA))
			if status != tc.status || answer["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", status, answer, tc.status, tc.code)
			}
			if tc.remedy != "" && !strings.Contains(answer["hint"].(string), tc.remedy) {
				t.Errorf("the refusal's hint %q does not name the remedy %q",
					answer["hint"], tc.remedy)
			}
			if actors, _ := work.writes(); len(actors) != 0 {
				t.Errorf("a caller who is not a person reached the tracker as %+v", actors)
			}
		})
	}

	work := &recordingWork{}
	status, answer := act(t, guarded(actSurface(t, work), false), "founder-secret",
		tracker.CreateWorkItemTool, "application/json", createBody(requestA))
	if status != http.StatusOK {
		t.Fatalf("the bound person was refused: %d %v", status, answer)
	}
}

// A PERSON'S WRITE IS SIGNED BY THEIR TOKEN AND NAMES THEM. The author is the
// credential and the kind `operator`, exactly as on the MCP transport — a
// dashboard write reads in an audit as the same person's assistant's would —
// and the bound seat rides beside them as the person whose own state it is.
// The answer carries the tool's own receipt with the outcome and the position
// lifted beside it, which is the floor the caller's next read waits for.
func TestAnActWriteRecordsTheTokenAsAuthorAndTheBoundSeat(t *testing.T) {
	t.Parallel()
	work := &recordingWork{}
	status, answer := act(t, guarded(actSurface(t, work), false), "founder-secret",
		tracker.CreateWorkItemTool, "application/json; charset=utf-8", createBody(requestA))
	if status != http.StatusOK {
		t.Fatalf("the bound person's create answered %d %v", status, answer)
	}
	actors, _ := work.writes()
	if len(actors) != 1 {
		t.Fatalf("one act wrote %d times", len(actors))
	}
	got := actors[0]
	if got.Handle != "founder" || got.Kind != tracker.AuthorOperator || got.OperatorID != "founder" {
		t.Errorf("the write is attributed as %+v — the author is the token and "+
			"the kind says it is not a seat", got)
	}
	if got.Seat != "jane-founder" {
		t.Errorf("the write names the seat %q, want the person the token is bound to", got.Seat)
	}
	if got.RequestKey != "founder/"+requestA {
		t.Errorf("the write carries the request key %q, want the request's own id "+
			"scoped to the token that sent it", got.RequestKey)
	}

	if answer["tool"] != tracker.CreateWorkItemTool || answer["outcome"] != "applied" ||
		answer["position"] != "CREWLET_TRACKER_LOG@1:4711" {

		t.Errorf("the act answered %v, want the tool, applied and the position", answer)
	}
	receipt, ok := answer["receipt"].(map[string]any)
	if !ok || receipt["key"] != "ENG-9" {
		t.Errorf("the receipt is %v, want the tool's own answer carrying the key it minted",
			answer["receipt"])
	}
}

// A RETRY IS ONE WRITE. A person whose write answered nothing sends it again
// under the SAME request id, and the operation id every derived write carries
// is then the first attempt's rather than a new one. A new
// gesture carries a new id and is a new write. The id is compared in its
// canonical spelling, so a client that upper-cased it on the retry is still
// retrying.
func TestARetriedActWithOneRequestIdIsOneWrite(t *testing.T) {
	t.Parallel()
	work := &recordingWork{outcome: statelog.OutcomeUnknown}
	h := guarded(actSurface(t, work), false)
	for _, id := range []string{requestA, strings.ToUpper(requestA), requestB} {
		status, answer := act(t, h, "founder-secret", tracker.CreateWorkItemTool,
			"application/json", createBody(id))
		if status != http.StatusOK || answer["outcome"] != "unknown" {
			t.Fatalf("request %s answered %d %v, want 200 carrying the unknown outcome",
				id, status, answer)
		}
		if answer["position"] != "CREWLET_TRACKER_LOG@1:4711" {
			// The fake answers a position on unknown too; the lift reads
			// what the tool said rather than inventing a rule of its own.
			t.Errorf("request %s answered position %v", id, answer["position"])
		}
	}
	_, ops := work.writes()
	if len(ops) != 3 {
		t.Fatalf("three acts reached the writer %d times", len(ops))
	}
	if ops[0] != ops[1] {
		t.Errorf("a retry of one request wrote under %q and then %q — the ledger "+
			"cannot collapse it, so it is a second item", ops[0], ops[1])
	}
	if ops[2] == ops[0] {
		t.Errorf("a second gesture wrote under the first one's operation %q", ops[0])
	}
	if !strings.Contains(ops[0], requestA) {
		t.Errorf("the operation %q is not seeded from the request id", ops[0])
	}

	// AND ONE ID FROM ANOTHER TOKEN IS ANOTHER REQUEST. The key is the
	// credential's and the request's together, so nobody collapses — or
	// suppresses — a write of somebody else's by sending its id first.
	other := &recordingWork{}
	founder := guarded(actSurface(t, other), false)
	cofounder := guarded(cofounderSurface(t, other), false)
	act(t, founder, "founder-secret", tracker.CreateWorkItemTool, "application/json", createBody(requestA))
	act(t, cofounder, "cofounder-secret", tracker.CreateWorkItemTool, "application/json", createBody(requestA))
	if _, crossed := other.writes(); len(crossed) != 2 || crossed[0] == crossed[1] {
		t.Errorf("two people sending one request id wrote under %v — the second "+
			"would be collapsed into the first person's write", crossed)
	}
}

// AND ON EVERY STORE, not only the tracker's create. The same request id sent
// twice reaches a knowledge-base write under one call key — the key its
// operation is derived from — and a project write under one operation, and a
// second request id reaches each under another. These were the writes whose
// operations were minted fresh per call, so a retried page comment or tag
// declaration was a second record.
func TestARetriedActIsOneWriteOnEveryStore(t *testing.T) {
	t.Parallel()
	kb := &recordingPages{}
	project := &recordingProject{}
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: (&recordingWork{}).writer,
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return project },
			Actor:         operator.WorkActor(boundChart),
		},
		Pages: builtin.PageDeps{Reader: kb, Writer: kb, Actor: operator.PageActor},
		Org:   boundChart,
	})
	h := guarded(s, false)
	for _, id := range []string{requestA, requestA, requestB} {
		comment := `{"request_id":"` + id + `","args":{"page":"Runbook","body":"the step is wrong"}}`
		if status, answer := act(t, h, "founder-secret", builtin.CommentOnPageTool,
			"application/json", comment); status != http.StatusOK {
			t.Fatalf("comment_on_page answered %d %v", status, answer)
		}
		tags := `{"request_id":"` + id + `","args":{"project":"ENG","tags_add":[{"slug":"ops","label":"Ops"}]}}`
		if status, answer := act(t, h, "founder-secret", tracker.WriteProjectTool,
			"application/json", tags); status != http.StatusOK {
			t.Fatalf("write_project answered %d %v", status, answer)
		}
	}
	for store, ops := range map[string][]string{"the knowledge base": kb.keys(), "the project": project.ops()} {
		if len(ops) != 3 {
			t.Fatalf("%s took %d writes for three acts", store, len(ops))
		}
		if ops[0] != ops[1] || !strings.Contains(ops[0], requestA) {
			t.Errorf("%s took one request as %q and then %q — a retry the ledger "+
				"cannot collapse", store, ops[0], ops[1])
		}
		if ops[2] == ops[0] {
			t.Errorf("%s took a second gesture under the first one's %q", store, ops[0])
		}
	}
}

// recordingProject is a project writer recording the operation every tag
// write arrives under.
type recordingProject struct {
	mu    sync.Mutex
	opIDs []string
}

func (p *recordingProject) WriteProject(context.Context, string, string, tracker.ProjectEdit,
	tracker.ProjectAuthority) (tracker.WriteResult, error) {
	return tracker.WriteResult{}, nil
}

func (p *recordingProject) WriteTags(_ context.Context, opID, _ string, _ tracker.TagEdit,
	_ tracker.TagAuthority) (tracker.WriteResult, error) {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.opIDs = append(p.opIDs, opID)
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (p *recordingProject) EnsureTags(context.Context, string, string, []string) ([]string, []string, error) {
	return nil, nil, nil
}

func (p *recordingProject) ops() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.opIDs)
}

// cofounderSurface is [actSurface] over a chart that also binds a second
// person, behind a guard that holds both tokens.
func cofounderSurface(t *testing.T, work *recordingWork) *operator.Server {
	t.Helper()
	chart := func() *org.Organization {
		o := boundChart()
		o.Roles = append(o.Roles, &org.Role{Name: "Kim Cofounder", Kind: org.KindHuman,
			Contact: &org.HumanContact{CrewletOperatorID: "cofounder"}})
		o.Normalize()
		return o
	}
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: work.writer, Actor: operator.WorkActor(chart),
		},
		Org: chart,
	})
	if s == nil {
		t.Fatal("no surface")
	}
	return s
}

// A READ IS NOT SERVED HERE. The socket answers every question the dashboard
// asks; a second read path would be a second answer to them.
func TestAReadToolIsRefusedOnTheActTransport(t *testing.T) {
	t.Parallel()
	s := actSurface(t, &recordingWork{})
	body := `{"request_id":"` + requestA + `","args":{"container":"workspace"}}`
	status, answer := act(t, guarded(s, false), "founder-secret", tracker.ListWorkItemsTool,
		"application/json", body)
	if status != http.StatusBadRequest || answer["error"] != string(operator.CodeReadOnlyTool) {
		t.Fatalf("a read answered %d %v, want 400 read_only_tool", status, answer)
	}
	if slices.Contains(s.Acts(), tracker.ListWorkItemsTool) {
		t.Errorf("the act list %v offers a read", s.Acts())
	}
	// AND A NAME THE CATALOGUE DOES NOT HOLD IS NOT FOUND, rather than
	// resolved to something near it.
	status, answer = act(t, guarded(s, false), "founder-secret", "run_sandbox",
		"application/json", body)
	if status != http.StatusNotFound || answer["error"] != string(operator.CodeUnknownTool) {
		t.Fatalf("an unknown tool answered %d %v, want 404 unknown_tool", status, answer)
	}
}

// A CROSS-SITE FORM CANNOT WRITE. A browser posts `text/plain`, a urlencoded
// or a multipart form to any origin with no preflight, and a body shaped like
// JSON would otherwise reach a write; only a body declared as JSON is read.
// A malformed envelope and an absent or nil request id are refused before any
// tool runs, each by its own code.
func TestAFormPostCannotReachTheActTransport(t *testing.T) {
	t.Parallel()
	good := createBody(requestA)
	cases := []struct {
		name, contentType, body string
		status                  int
		code                    httpjson.Code
	}{
		{"text/plain", "text/plain", good, http.StatusUnsupportedMediaType, operator.CodeUnsupportedMediaType},
		{"a urlencoded form", "application/x-www-form-urlencoded", good,
			http.StatusUnsupportedMediaType, operator.CodeUnsupportedMediaType},
		{"a multipart form", "multipart/form-data; boundary=x", good,
			http.StatusUnsupportedMediaType, operator.CodeUnsupportedMediaType},
		{"no content type", "", good, http.StatusUnsupportedMediaType, operator.CodeUnsupportedMediaType},
		{"JSON in another charset", "application/json; charset=latin1", good,
			http.StatusUnsupportedMediaType, operator.CodeUnsupportedMediaType},
		{"a body that is not JSON", "application/json", "title=x", http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"a key the envelope does not take", "application/json",
			`{"requestId":"` + requestA + `","args":{}}`, http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"two objects", "application/json", good + good, http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"no request id", "application/json", `{"args":{"title":"x"}}`,
			http.StatusBadRequest, operator.CodeInvalidRequestID},
		{"a request id that is not a uuid", "application/json", `{"request_id":"r-1","args":{}}`,
			http.StatusBadRequest, operator.CodeInvalidRequestID},
		{"the nil uuid", "application/json",
			`{"request_id":"00000000-0000-0000-0000-000000000000","args":{}}`,
			http.StatusBadRequest, operator.CodeInvalidRequestID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := &recordingWork{}
			status, answer := act(t, guarded(actSurface(t, work), false), "founder-secret",
				tracker.CreateWorkItemTool, tc.contentType, tc.body)
			if status != tc.status || answer["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", status, answer, tc.status, tc.code)
			}
			if actors, _ := work.writes(); len(actors) != 0 {
				t.Errorf("a refused request reached the tracker")
			}
		})
	}
}

// EVERY REFUSAL CLASS ANSWERS ONE STATUS, and no transport code is also a
// class. The first is what lets a client decide what to do next from the
// status alone; the second is what lets it branch on `error` without knowing
// which half of the vocabulary a code came from.
func TestEveryRefusalCodeMapsToOneStatus(t *testing.T) {
	t.Parallel()
	for _, class := range crewletmcp.Refusals {
		status, ok := operator.RefusalStatus(class)
		if !ok {
			t.Errorf("the refusal class %q answers no status on the act transport", class)
			continue
		}
		if status < 400 || status > 599 {
			t.Errorf("the refusal class %q answers %d, which is not a refusal", class, status)
		}
	}
	if _, ok := operator.RefusalStatus(crewletmcp.Refusal("teapot")); ok {
		t.Error("a class this build does not know was given a status")
	}
	if _, ok := operator.RefusalStatus(""); ok {
		t.Error("an unclassified failure was given a status")
	}
	seen := map[string]bool{}
	for _, code := range operator.ActTransportCodes {
		if seen[string(code)] {
			t.Errorf("%q is listed twice", code)
		}
		seen[string(code)] = true
		if crewletmcp.Refusal(code).Valid() {
			t.Errorf("the transport code %q is also a refusal class", code)
		}
	}
	if len(operator.ActTransportCodes) < 8 {
		t.Fatalf("the transport lists %d codes; this check is certifying nothing",
			len(operator.ActTransportCodes))
	}
}

// A TOOL'S REFUSAL REACHES THE PERSON AS ITS CLASS, with the tool's own
// sentence as the detail, at the status the class maps to — here the one a
// create against a project the stub tracker does not know comes back with.
func TestAToolRefusalIsAnsweredByItsClass(t *testing.T) {
	t.Parallel()
	body := `{"request_id":"` + requestA + `","args":{"item":"ENG-404","status":"done"}}`
	status, answer := act(t, guarded(actSurface(t, &recordingWork{}), false), "founder-secret",
		tracker.UpdateWorkItemTool, "application/json", body)
	want, _ := operator.RefusalStatus(crewletmcp.RefusalNotFound)
	if status != want || answer["error"] != string(crewletmcp.RefusalNotFound) {
		t.Fatalf("an update of a missing item answered %d %v, want %d not_found",
			status, answer, want)
	}
	if detail, _ := answer["detail"].(string); !strings.Contains(detail, "ENG-404") ||
		answer["tool"] != tracker.UpdateWorkItemTool {

		t.Errorf("the refusal %v does not carry the tool and its own sentence", answer)
	}
}

// THE LARGEST PAGE THE KNOWLEDGE BASE ACCEPTS FITS THE BODY, as the browser
// sends it: JSON.stringify escapes a quote, a backslash and a newline to two
// bytes each, so a legal page of nothing but those doubles on the wire, and a
// cap at the page's own size refused a page the store would have taken.
func TestTheLargestLegalPageFitsTheActBody(t *testing.T) {
	t.Parallel()
	pageBody := strings.Repeat("\"\\\n", pages.MaxBody/3)
	pageBody += strings.Repeat("\"", pages.MaxBody-len(pageBody))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// JSON.stringify does not escape <, > and &; Go's encoder does unless
	// told otherwise, and this is the browser's envelope being measured.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{
		"request_id": requestA,
		"args": map[string]any{
			"container": "ENG", "title": strings.Repeat("T", pages.MaxTitle),
			"body": pageBody, "labels": []string{"runbook", "security"},
			"message": strings.Repeat("m", 512),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= pages.MaxBody*2 {
		t.Fatalf("the envelope is %d bytes; the case is not measuring the worst escape", buf.Len())
	}
	status, answer := act(t, guarded(actSurface(t, &recordingWork{}), false), "founder-secret",
		builtin.WritePageTool, "application/json", buf.String())
	if status != http.StatusOK {
		t.Fatalf("the largest legal page (%d bytes on the wire, cap %d) answered %d %v",
			buf.Len(), operator.MaxActBody, status, answer)
	}
	// AND THE CAP STILL BINDS just past it.
	over := `{"request_id":"` + requestA + `","args":{"body":"` +
		strings.Repeat("x", operator.MaxActBody) + `"}}`
	if status, answer := act(t, guarded(actSurface(t, &recordingWork{}), false), "founder-secret",
		builtin.WritePageTool, "application/json", over); status != http.StatusRequestEntityTooLarge ||
		answer["error"] != string(httpjson.CodeBodyTooLarge) {

		t.Errorf("a body past the cap answered %d %v, want 413 body_too_large", status, answer)
	}
}

// THE OUTCOME IS THE TOOL'S, lifted and never invented. A write that appended
// nothing answers `applied` at no position, since the state asked for already
// holds and there is nothing to wait for; an outcome this build does not know
// is `unknown`, the one answer that cannot tell a caller to stop looking at a
// write nobody can vouch for.
func TestTheActAnswerLiftsTheToolsOwnOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, output string
		outcome      statelog.Outcome
		position     string
	}{
		{"a write that appended nothing", `{"key":"ENG-1"}`, statelog.OutcomeApplied, ""},
		{"an empty outcome", `{"outcome":"","position":null}`, statelog.OutcomeApplied, ""},
		{"a pending write", `{"outcome":"pending","position":"L@1:2"}`, statelog.OutcomePending, "L@1:2"},
		{"an outcome this build does not know", `{"outcome":"landed","position":"L@1:2"}`,
			statelog.OutcomeUnknown, "L@1:2"},
		{"a sentence", `done`, statelog.OutcomeApplied, ""},
	}
	for _, tc := range cases {
		got := operator.ReceiptOf("t", tc.output)
		position := ""
		if got.Position != nil {
			position = *got.Position
		}
		if got.Outcome != tc.outcome || position != tc.position {
			t.Errorf("%s: answered %s at %q, want %s at %q", tc.name,
				got.Outcome, position, tc.outcome, tc.position)
		}
		if !json.Valid(got.Receipt) {
			t.Errorf("%s: the receipt %q is not JSON", tc.name, got.Receipt)
		}
	}
}

// A PAGE WRITE STATES ITS OUTCOME AND POSITION, as every work write always
// has. It answered neither until this transport needed them, so a person's
// assistant that wrote a page had nothing to hand back as `min_position`, and
// the read after the write could miss it.
func TestAPageWriteStatesItsOutcomeAndPosition(t *testing.T) {
	t.Parallel()
	kb := &positionedPages{}
	s := newSurface(t, operator.Options{
		Pages: builtin.PageDeps{Reader: kb, Writer: kb, Actor: operator.PageActor},
		Org:   boundChart,
	})
	body := `{"request_id":"` + requestA + `","args":{"page":"Runbook","body":"the step is wrong"}}`
	status, answer := act(t, guarded(s, false), "founder-secret", builtin.CommentOnPageTool,
		"application/json", body)
	if status != http.StatusOK || answer["outcome"] != "pending" ||
		answer["position"] != "CREWLET_PAGES_LOG@2:88" {

		t.Fatalf("a page comment answered %d %v, want pending at CREWLET_PAGES_LOG@2:88",
			status, answer)
	}
}

// positionedPages is a knowledge base whose comment lands pending at a known
// position.
type positionedPages struct{ recordingPages }

func (p *positionedPages) Comment(ctx context.Context, actor pages.Actor, id string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {

	comment, _, err := p.recordingPages.Comment(ctx, actor, id, in)
	return comment, pages.Written{Revision: 88, Outcome: statelog.Result{
		Outcome:  statelog.OutcomePending,
		Position: statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 2, Seq: 88},
	}}, err
}
