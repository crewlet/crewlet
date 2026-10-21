package operator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/modelnote"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The act transport is the dashboard's write path. Every case drives the real
// handler behind a stand-in for the request guard — a table of bearers and the
// principals they resolve to — because WHO is calling is decided by the guard
// and is a value on the request by the time this handler runs (ADR-0024): any
// principal the guard resolved may act, and what each one may do is the
// authority table's, exactly as on every other surface.

// The principals the cases act as: two people the identity directory binds to
// seats, a pipeline's token bound to nobody, and a token that uses the
// knowledge base and not the tracker.
var (
	founder   = person("jane.founder", "jane-founder", iam.Apps...)
	cofounder = person("kim.cofounder", "kim-cofounder", iam.Apps...)
	pipeline  = machine("token:ci", iam.GrantWorklet)
	reader    = machine("token:reader", iam.GrantDoclet)
)

// company is the chart the cases run in: the two people's seats, and an agent.
func company() *org.Organization {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Jane Founder", Kind: org.KindHuman},
			{Name: "Kim Cofounder", Kind: org.KindHuman},
			{Name: "CTO"},
		},
	}
	o.Normalize()
	return o
}

// recordingWork is a tracker writer that records every create it is asked
// for — the actor the surface resolved, the operation id it derived and the
// id of the task it would file — and answers with a position, as the real
// writer does.
type recordingWork struct {
	mu      sync.Mutex
	actors  []builtin.Actor
	opIDs   []string
	taskIDs []string
	// fields is every create's custom-field values, as the tool encoded
	// them for the writer.
	fields  []map[string]json.RawMessage
	outcome statelog.Outcome
	// caveats is what every create answers it was not refused for.
	caveats []caveat.Caveat
}

type recordingWriter struct {
	w     *recordingWork
	actor builtin.Actor
}

func (r *recordingWork) writer(actor builtin.Actor) builtin.WorkWriter {
	return recordingWriter{w: r, actor: actor}
}

func (w recordingWriter) CreateTaskAsking(ctx context.Context, opID string,
	task worklet.Task, _ worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error) {

	return w.CreateTask(ctx, opID, task, notify)
}

func (w recordingWriter) CreateTask(_ context.Context, opID string, task worklet.Task,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	w.w.actors = append(w.w.actors, w.actor)
	w.w.opIDs = append(w.w.opIDs, opID)
	w.w.taskIDs = append(w.w.taskIDs, task.ID)
	w.w.fields = append(w.w.fields, task.Fields)
	outcome := w.w.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	return worklet.WriteResult{
		Result: statelog.Result{
			Outcome:  outcome,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4711},
			Version:  4711,
		},
		Key:     "ENG-9",
		Caveats: w.w.caveats,
	}, nil
}

func (w recordingWriter) UpdateTask(context.Context, string, string, string, uint64,
	worklet.TaskPatch, worklet.ChangeKind, *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, nil
}

func (r *recordingWork) writes() ([]builtin.Actor, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]builtin.Actor(nil), r.actors...), append([]string(nil), r.opIDs...)
}

// fieldsFiled is every create's custom-field values, in order.
func (r *recordingWork) fieldsFiled() []map[string]json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), r.fields...)
}

// filed is the id of every task a create was asked to file, in order.
func (r *recordingWork) filed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.taskIDs...)
}

// actSurface is the operator surface over a recording tracker and a stub
// knowledge base, in [company].
func actSurface(t *testing.T, work *recordingWork) *operator.Server {
	t.Helper()
	return newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: work.writer},
			Pages: builtin.PageDeps{
				Reader: stubPageReader{}, Writer: stubPageWriter{},
			},
		}),
		Org:       company,
		Authorize: builtin.Decide(leadsNobody{}),
	})
}

// guarded is the act route behind the guard's stand-in, as the app mounts it.
func guarded(s *operator.Server) http.Handler {
	dir := &directory{people: map[string]iam.Principal{
		"founder": founder, "cofounder": cofounder, "ci": pipeline, "reader": reader,
	}}
	mux := http.NewServeMux()
	mux.Handle(operator.ActPattern, s.ActHandler())
	return dir.middleware(mux)
}

// act posts one request under key — no header where it is empty — and decodes
// the JSON answer.
func act(t *testing.T, h http.Handler, bearer, tool, contentType, body,
	key string) (int, map[string]any) {

	t.Helper()
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+tool, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		r.Header.Set(opkey.Header, key)
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

// newKey is an operation key as the dashboard mints one per gesture.
func newKey() string { return statelog.NewOpID(time.Now(), "") }

// createArgs is the one create_work_item call these cases make.
func createArgs() map[string]any {
	return map[string]any{"title": "Rotate the signing key", "project": "ENG"}
}

// createBody is that call as the act transport's body.
func createBody() string {
	raw, _ := json.Marshal(map[string]any{"args": createArgs()})
	return string(raw)
}

// ANY PRINCIPAL THE GUARD RESOLVED MAY ACT, and is decided by the authority
// table on what it holds. A person bound to a seat and a pipeline's token
// bound to nobody are both served — a credential acting as itself is a
// legitimate author, attributed under its own login — while a request the
// guard found nobody behind is `401` and one it could not resolve is `503`,
// and neither reaches the tracker.
func TestAnyResolvedPrincipalMayAct(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, bearer string
		status       int
		code         httpjson.Code
	}{
		{"a bound person", "founder", http.StatusOK, ""},
		{"an unbound token", "ci", http.StatusOK, ""},
		{"no credential", "", http.StatusUnauthorized, httpjson.CodeInvalidToken},
		{"a wrong credential", "guess", http.StatusUnauthorized, httpjson.CodeInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := &recordingWork{}
			status, answer := act(t, guarded(actSurface(t, work)), tc.bearer,
				worklet.CreateWorkItemTool, "application/json", createBody(), newKey())
			if status != tc.status || (tc.code != "" && answer["error"] != string(tc.code)) {
				t.Fatalf("answered %d %v, want %d %s", status, answer, tc.status, tc.code)
			}
			if actors, _ := work.writes(); (status == http.StatusOK) != (len(actors) == 1) {
				t.Errorf("an answer of %d reached the tracker %d times", status, len(actors))
			}
		})
	}
}

// THE AUTHORITY TABLE DECIDES, in the envelope every surface answers a refusal
// with: `403 unauthorized`, the rule's reason and the grants that would have
// admitted the caller — so a screen can say what would change the answer —
// and nothing written.
func TestAnActTheTableRefusesIsAnsweredInTheEnvelope(t *testing.T) {
	t.Parallel()
	work := &recordingWork{}
	status, answer := act(t, guarded(actSurface(t, work)), "reader",
		worklet.CreateWorkItemTool, "application/json", createBody(), newKey())
	if status != http.StatusForbidden || answer["error"] != string(httpjson.CodeUnauthorized) {
		t.Fatalf("a reader's create answered %d %v, want 403 unauthorized", status, answer)
	}
	grants, _ := answer["grants"].([]any)
	if answer["reason"] == nil || !slices.Contains(grants, any(string(iam.GrantWorklet))) {
		t.Errorf("the refusal %v does not name its reason and the grant that "+
			"would have admitted the caller", answer)
	}
	if actors, _ := work.writes(); len(actors) != 0 {
		t.Errorf("a refused act reached the tracker as %+v", actors)
	}
}

// A WRITE IS MADE AS ITS PRINCIPAL: a bound person as their seat, kind human,
// with their login as the credential; an unbound token under its whole login,
// kind operator. The answer carries the tool's own receipt with the outcome and
// the position lifted beside it — the floor the caller's next read waits for —
// and the operation the request was made under, which is what every write it
// made derived its id from.
func TestAnActWriteIsMadeAsItsPrincipal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		bearer, author, credential string
		kind                       worklet.AuthorKind
	}{
		"a bound person":   {"founder", "jane-founder", "jane.founder", worklet.AuthorHuman},
		"an unbound token": {"ci", "token:ci", "token:ci", worklet.AuthorOperator},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			work := &recordingWork{}
			key := newKey()
			status, answer := act(t, guarded(actSurface(t, work)), tc.bearer,
				worklet.CreateWorkItemTool, "application/json; charset=utf-8",
				createBody(), key)
			if status != http.StatusOK {
				t.Fatalf("the create answered %d %v", status, answer)
			}
			actors, ops := work.writes()
			if len(actors) != 1 {
				t.Fatalf("one act wrote %d times", len(actors))
			}
			got := actors[0]
			if got.Handle != tc.author || got.Kind != tc.kind || got.OperatorID != tc.credential {
				t.Errorf("the write is attributed as %+v, want %s (%s) through %s",
					got, tc.author, tc.kind, tc.credential)
			}
			op, _ := answer["op_id"].(string)
			if got.WorkKey != op || statelog.CheckCallerOpID(op) != nil {
				t.Errorf("the write was made under %q and the answer names %q — "+
					"the op_id a retry sends back must be the one it was made under",
					got.WorkKey, op)
			}
			at, _ := statelog.OpMintedAt(key)
			if derived, _ := statelog.OpMintedAt(ops[0]); !derived.Equal(at) {
				t.Errorf("the write's operation carries the instant %v, want the "+
					"key's own %v", derived, at)
			}
			if answer["tool"] != worklet.CreateWorkItemTool || answer["outcome"] != "applied" ||
				answer["position"] != "CREWLET_TRACKER_LOG@1:4711" {

				t.Errorf("the act answered %v, want the tool, applied and the position", answer)
			}
			receipt, ok := answer["receipt"].(map[string]any)
			if !ok || receipt["key"] != "ENG-9" {
				t.Errorf("the receipt is %v, want the tool's own answer carrying the key",
					answer["receipt"])
			}
		})
	}
}

// A RETRY IS ONE WRITE. A person whose write answered nothing sends it again
// under the SAME key — or the op_id the answer named, which is the same
// operation — and every write it derives is then the first attempt's rather
// than a new one. A new gesture carries a new key and is a new write.
//
// AND ONE KEY FROM ANOTHER PRINCIPAL IS ANOTHER OPERATION. The key is scoped by
// who sent it, so nobody collapses — or suppresses — a write of somebody
// else's by sending its key first.
func TestARetriedActIsOneWrite(t *testing.T) {
	t.Parallel()
	work := &recordingWork{}
	h := guarded(actSurface(t, work))
	first, second := newKey(), newKey()
	_, answer := act(t, h, "founder", worklet.CreateWorkItemTool, "application/json",
		createBody(), first)
	returned, _ := answer["op_id"].(string)
	for _, key := range []string{first, returned, second} {
		if status, answer := act(t, h, "founder", worklet.CreateWorkItemTool,
			"application/json", createBody(), key); status != http.StatusOK {
			t.Fatalf("key %s answered %d %v", key, status, answer)
		}
	}
	_, ops := work.writes()
	ids := work.filed()
	if len(ops) != 4 {
		t.Fatalf("four acts reached the writer %d times", len(ops))
	}
	if ops[0] != ops[1] || ops[0] != ops[2] || ids[0] != ids[1] || ids[0] != ids[2] {
		t.Errorf("one request sent three times wrote under %v filing %v — the "+
			"ledger cannot collapse it, so it is a second item", ops[:3], ids[:3])
	}
	if ops[3] == ops[0] {
		t.Errorf("a second gesture wrote under the first one's operation %q", ops[0])
	}

	other := &recordingWork{}
	both := guarded(actSurface(t, other))
	act(t, both, "founder", worklet.CreateWorkItemTool, "application/json", createBody(), first)
	act(t, both, "cofounder", worklet.CreateWorkItemTool, "application/json", createBody(), first)
	// AND THE OP_ID THE FIRST PERSON WAS ANSWERED, sent by somebody else,
	// is scoped again under the sender rather than taken as it is.
	act(t, both, "cofounder", worklet.CreateWorkItemTool, "application/json", createBody(), returned)
	if _, crossed := other.writes(); len(crossed) != 3 ||
		crossed[1] == crossed[0] || crossed[2] == crossed[0] {

		t.Errorf("two people sending one key wrote under %v — the second "+
			"would be collapsed into the first person's write", crossed)
	}
}

// A WRITE NOBODY CAN VOUCH FOR IS ANSWERED AS ONE: 503 `unavailable` WITH
// `outcome: unknown` beside it, the operation to retry under, and a
// Retry-After — the shape an interrupted call answers.
//
// `unavailable` is also what a tool answers when it refused BEFORE writing
// anything, and a client reads a 503 with no outcome as "nothing was written":
// a person told that about a create that landed files it again under a new
// gesture — a second item. So the class alone is not the answer; the outcome
// is. And the way on is the person's own: the same request under the same key,
// never an `op_id` argument, which this transport refuses.
func TestAnActWhoseWriteIsUnknownSaysSo(t *testing.T) {
	t.Parallel()
	work := &recordingWork{outcome: statelog.OutcomeUnknown}
	key := newKey()
	h := guarded(actSurface(t, work))
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+worklet.CreateWorkItemTool,
		strings.NewReader(createBody()))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer founder")
	r.Header.Set(opkey.Header, key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer is not JSON: %q", rec.Body)
	}
	if rec.Code != http.StatusServiceUnavailable || answer["error"] != "unavailable" {
		t.Fatalf("an unknown create answered %d %v, want 503 unavailable", rec.Code, answer)
	}
	if answer["outcome"] != string(statelog.OutcomeUnknown) {
		t.Errorf("an unknown create answered outcome %v, want unknown — the class "+
			"alone reads as a refusal that wrote nothing", answer["outcome"])
	}
	actors, _ := work.writes()
	if len(actors) != 1 || answer["op_id"] != actors[0].WorkKey {
		t.Errorf("the answer names op_id %v; the write was made under %+v",
			answer["op_id"], actors)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a write this node can settle by being asked again answered no Retry-After")
	}
	if detail, has := answer["tool_detail"]; has {
		t.Errorf("an unknown answered the tool's own sentence, written for a "+
			"model: %v", detail)
	}
}

// AND ON EVERY STORE, not only the tracker's create. The same key sent twice
// reaches a knowledge-base write under one operation key and a project write
// under one operation, and a second key reaches each under another. These
// were the writes whose operations were minted fresh per call, so a retried
// page comment or tag declaration was a second record.
func TestARetriedActIsOneWriteOnEveryStore(t *testing.T) {
	t.Parallel()
	kb := &recordingPages{}
	project := &recordingProject{}
	h := guarded(newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{
				Reader: stubWorkReader{}, Writer: (&recordingWork{}).writer,
				ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return project },
			},
			Pages: builtin.PageDeps{Reader: kb, Writer: kb},
		}),
		Org: company,
	}))
	const (
		comment = `{"args":{"page":"Runbook","body":"the step is wrong"}}`
		tags    = `{"args":{"project":"ENG","tags_add":[{"slug":"ops","label":"Ops"}]}}`
	)
	first, second := newKey(), newKey()
	for _, key := range []string{first, first, second} {
		if status, answer := act(t, h, "founder", builtin.CommentOnPageTool,
			"application/json", comment, key); status != http.StatusOK {
			t.Fatalf("comment_on_page answered %d %v", status, answer)
		}
		if status, answer := act(t, h, "founder", worklet.WriteProjectTool,
			"application/json", tags, key); status != http.StatusOK {
			t.Fatalf("write_project answered %d %v", status, answer)
		}
	}
	for store, ops := range map[string][]string{
		"the knowledge base": kb.keys(),
		"the project":        project.ops(),
	} {
		if len(ops) != 3 {
			t.Fatalf("%s took %d writes for three acts", store, len(ops))
		}
		if ops[0] == "" || ops[0] != ops[1] {
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

func (p *recordingProject) WriteProject(context.Context, string, string, worklet.ProjectEdit,
	worklet.ProjectAuthority) (worklet.WriteResult, error) {
	return worklet.WriteResult{}, nil
}

func (p *recordingProject) WriteTags(_ context.Context, opID, _ string, _ worklet.TagEdit,
	_ worklet.TagAuthority) (worklet.WriteResult, error) {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.opIDs = append(p.opIDs, opID)
	return worklet.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (p *recordingProject) EnsureTags(context.Context, string, string, []string) ([]string, []caveat.Caveat, error) {
	return nil, nil, nil
}

func (p *recordingProject) ops() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.opIDs)
}

// THE ANSWER IS OFFERED TO SOMEBODY WITHOUT THE TRACKER.
//
// A person kept off the board may still post the answer to a question an open
// ask on a work item puts to them — and whether one does is the item's, which
// nobody has read when a screen asks what to enable. The table says so
// ([authz.ErrUnresolved] for the comment), which is offerable, so the comment
// control is enabled and the tool decides on the item; the board's ordinary
// writes, which no ask admits, are not offered at all.
func TestActsOffersTheAnswerToAPersonWithoutWorklet(t *testing.T) {
	t.Parallel()
	s := actSurface(t, &recordingWork{})
	acts, err := s.Acts(t.Context(), reader)
	if err != nil {
		t.Fatalf("Acts: %v", err)
	}
	if !slices.Contains(acts, worklet.CommentOnWorkTool) {
		t.Errorf("somebody without Worklet is not offered %s, so an ask put "+
			"to them could never be answered from a screen: %v",
			worklet.CommentOnWorkTool, acts)
	}
	for _, write := range []string{worklet.CreateWorkItemTool, worklet.UpdateWorkItemTool} {
		if slices.Contains(acts, write) {
			t.Errorf("somebody without Worklet is offered %s, which no ask "+
				"admits them to: %v", write, acts)
		}
	}
}

// A READ IS NOT SERVED HERE. The socket answers every question the dashboard
// asks; a second read path would be a second answer to them. And a name the
// catalogue does not hold is not found, rather than resolved to something
// near it.
func TestAReadToolIsRefusedOnTheActTransport(t *testing.T) {
	t.Parallel()
	s := actSurface(t, &recordingWork{})
	body := `{"args":{"container":"workspace"}}`
	status, answer := act(t, guarded(s), "founder", worklet.ListWorkItemsTool,
		"application/json", body, newKey())
	if status != http.StatusBadRequest || answer["error"] != string(httpjson.CodeReadOnlyTool) {
		t.Fatalf("a read answered %d %v, want 400 read_only_tool", status, answer)
	}
	acts, err := s.Acts(t.Context(), founder)
	if err != nil {
		t.Fatalf("Acts: %v", err)
	}
	if slices.Contains(acts, worklet.ListWorkItemsTool) {
		t.Errorf("the act list %v offers a read", acts)
	}
	if !slices.Contains(acts, worklet.CreateWorkItemTool) {
		t.Errorf("the act list %v does not offer the create the founder may make", acts)
	}
	// AND WHAT THE TABLE WOULD NEVER ADMIT IS NOT OFFERED: a screen enables
	// exactly the controls a press of could be served.
	// THE ERROR IS READ: a list that could not be decided is nil, which
	// contains nothing and passed this check while it decided nothing.
	if acts, err := s.Acts(t.Context(), reader); err != nil {
		t.Errorf("Acts for a reader: %v", err)
	} else if slices.Contains(acts, worklet.CreateWorkItemTool) {
		t.Errorf("a reader is offered %v, including a create the table refuses them", acts)
	}
	status, answer = act(t, guarded(s), "founder", "no_such_tool",
		"application/json", body, newKey())
	if status != http.StatusNotFound || answer["error"] != string(httpjson.CodeUnknownTool) {
		t.Fatalf("an unknown tool answered %d %v, want 404 unknown_tool", status, answer)
	}
}

// A CROSS-SITE FORM CANNOT WRITE. A browser posts `text/plain`, a urlencoded
// or a multipart form to any origin with no preflight, and a body shaped like
// JSON would otherwise reach a write; only a body declared as JSON is read. A
// malformed body and an absent or unusable operation key are refused before
// any tool runs, each by its own code.
func TestAFormPostCannotReachTheActTransport(t *testing.T) {
	t.Parallel()
	good := createBody()
	cases := []struct {
		name, contentType, body, key string
		status                       int
		code                         httpjson.Code
	}{
		{"text/plain", "text/plain", good, newKey(), http.StatusUnsupportedMediaType,
			httpjson.CodeUnsupportedMediaType},
		{"a urlencoded form", "application/x-www-form-urlencoded", good, newKey(),
			http.StatusUnsupportedMediaType, httpjson.CodeUnsupportedMediaType},
		{"a multipart form", "multipart/form-data; boundary=x", good, newKey(),
			http.StatusUnsupportedMediaType, httpjson.CodeUnsupportedMediaType},
		{"no content type", "", good, newKey(), http.StatusUnsupportedMediaType,
			httpjson.CodeUnsupportedMediaType},
		{"JSON in another charset", "application/json; charset=latin1", good, newKey(),
			http.StatusUnsupportedMediaType, httpjson.CodeUnsupportedMediaType},
		{"a body that is not JSON", "application/json", "title=x", newKey(),
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		// A KEY THE BODY DOES NOT TAKE, an operation above all: a client
		// naming one in the body rather than the header believed its retry
		// was the same write.
		{"an operation id in the body", "application/json",
			`{"op_id":"` + newKey() + `","args":{}}`, newKey(),
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"two objects", "application/json", good + good, newKey(),
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"arguments that are not an object", "application/json",
			`{"args":["title"]}`, newKey(), http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"no operation key", "application/json", good, "",
			http.StatusBadRequest, httpjson.CodeOpIDInvalid},
		{"a key that is not an operation id", "application/json", good, "r-1",
			http.StatusBadRequest, httpjson.CodeOpIDInvalid},
		{"the nil uuid", "application/json", good, uuid.Nil.String(),
			http.StatusBadRequest, httpjson.CodeOpIDInvalid},
		// A RANDOM UUID CARRIES NO INSTANT, and an operation derived from
		// it would read as minted before every row a node's operation
		// ledger ever lost: answered `unknown` without being made, first
		// attempt included, on every node old enough to have swept one.
		{"a version 4 uuid", "application/json", good, uuid.NewString(),
			http.StatusBadRequest, httpjson.CodeOpIDInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := &recordingWork{}
			status, answer := act(t, guarded(actSurface(t, work)), "founder",
				worklet.CreateWorkItemTool, tc.contentType, tc.body, tc.key)
			if status != tc.status || answer["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", status, answer, tc.status, tc.code)
			}
			if tc.code == httpjson.CodeOpIDInvalid && answer["field"] != opkey.Header {
				t.Errorf("the key's refusal names %v, want the header it is sent in",
					answer["field"])
			}
			if actors, _ := work.writes(); len(actors) != 0 {
				t.Errorf("a refused request reached the tracker")
			}
		})
	}
}

// EVERY REFUSAL CLASS ANSWERS ONE STATUS, UNDER ITS OWN NAME, and no transport
// code is also a class. The first is what lets a client decide what to do next
// from the status alone; the second is what lets it branch on `error` without
// knowing which half of the vocabulary a code came from. Asked of [operator.Fail]
// itself, the one writer every transport answers a refusal through, so what is
// held is what goes out: a class waiting can clear is a 503 carrying a
// Retry-After, every other a 4xx, and a class this build does not know is an
// opaque 500 rather than a status nobody chose.
func TestEveryRefusalCodeMapsToOneStatus(t *testing.T) {
	t.Parallel()
	answer := func(class crewletmcp.Refusal) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		operator.Fail(rec, crewletmcp.Classify(class, errors.New("the reason")),
			caveat.NotAnOption(), newKey(), nil)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%q answered a body that is not JSON: %q", class, rec.Body)
		}
		return rec, body
	}
	for _, class := range crewletmcp.Refusals {
		rec, body := answer(class)
		code, _ := body["error"].(string)
		// A FAULT IS THE ONE CLASS ANSWERED 500, and the only 500 a class
		// may answer: every other one is a refusal the caller acts on.
		fault := class == crewletmcp.RefusalInternalError
		switch {
		case rec.Code < 400 || rec.Code > 599:
			t.Errorf("the refusal class %q answers %d, which is no refusal", class, rec.Code)
		case fault != (rec.Code == http.StatusInternalServerError):
			t.Errorf("the refusal class %q answers %d; only a fault is a 500", class, rec.Code)
		case code != string(class):
			t.Errorf("the refusal class %q is answered as %q — a client reads one "+
				"vocabulary whichever transport refused it", class, code)
		case !httpjson.Code(code).Valid():
			t.Errorf("the code %q has no sentence on the envelope's table", code)
		}
		if again, _ := answer(class); again.Code != rec.Code {
			t.Errorf("the refusal class %q answered %d and then %d", class,
				rec.Code, again.Code)
		}
		if comeBack := rec.Code == http.StatusServiceUnavailable; comeBack &&
			rec.Header().Get("Retry-After") == "" {
			t.Errorf("the refusal class %q answers 503 with no Retry-After", class)
		}
		if after := rec.Header().Get("Retry-After"); fault && after != "" {
			t.Errorf("a fault told its caller to come back in %s seconds", after)
		}
	}
	if rec, _ := answer("teapot"); rec.Code != http.StatusInternalServerError {
		t.Errorf("a class this build does not know answered %d, want an opaque 500", rec.Code)
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
		if !code.Valid() {
			t.Errorf("the transport code %q has no sentence on the envelope's table", code)
		}
	}
	if len(operator.ActTransportCodes) < 8 {
		t.Fatalf("the transport lists %d codes; this check is certifying nothing",
			len(operator.ActTransportCodes))
	}
}

// A TOOL'S REFUSAL REACHES THE PERSON AS ITS CLASS, at the status the class
// maps to (here the one an update of an item the stub tracker does not hold
// comes back with), naming the tool and the item, and in its code's own
// sentence: nobody has worded this refusal for a person, so it carries no
// detail, and the tool's sentence (written for a model) is never it
// (ADR-0060).
func TestAToolRefusalIsAnsweredByItsClass(t *testing.T) {
	t.Parallel()
	body := `{"args":{"item":"ENG-404","status":"done"}}`
	status, answer := act(t, guarded(actSurface(t, &recordingWork{})), "founder",
		worklet.UpdateWorkItemTool, "application/json", body, newKey())
	if status != http.StatusNotFound || answer["error"] != string(crewletmcp.RefusalNotFound) {
		t.Fatalf("an update of a missing item answered %d %v, want 404 not_found",
			status, answer)
	}
	if answer["tool"] != worklet.UpdateWorkItemTool || answer["item"] != "ENG-404" {
		t.Errorf("the refusal %v does not name the tool and the item", answer)
	}
	if detail, has := answer["detail"]; has {
		t.Errorf("a refusal nobody worded for a person answered a detail: %v", detail)
	}
	if answer["message"] != httpjson.CodeNotFound.Message() {
		t.Errorf("the refusal's sentence is %v, want its code's own", answer["message"])
	}
}

// A TOOL REFUSED UNDER AN If-Match HEADER IS ANSWERED AS [operator.FailTool]
// ANSWERS IT, BUT FOR A STALE VERSION, which is HTTP's failed precondition
// (`412 version_moved`, [operator.FailIfMatched]); and either way the tool's
// own sentence is told to the model that made the call and is never in the
// person's answer. A route that folded the header answers both halves through
// one function ([operator.FailToolIfMatched]), so the header moves the status
// of a stale version and nothing about who hears which words.
//
// Mutation: answer the stale class 409 under the header, any other class
// differently from FailTool, or drop FailToolIfMatched's modelnote.Tell, and
// this goes red naming it.
func TestAToolRefusedUnderIfMatchIsFailToolsAnswerButForAStaleVersion(t *testing.T) {
	t.Parallel()
	const model = "update_work_item refused that: the model's sentence, naming what to send."
	answer := func(fail func(http.ResponseWriter, *http.Request, tools.Result, string,
		httpjson.Detail), cause error) (*httptest.ResponseRecorder, string) {

		ctx, heard := modelnote.Expect(t.Context())
		r := httptest.NewRequestWithContext(ctx, http.MethodPatch, "/worklet/items/ENG-1", nil)
		rec := httptest.NewRecorder()
		// ANY PERSON'S WORDS: what is held is that both answers carry them.
		fail(rec, r, tools.Result{Failed: true, Output: model, Cause: cause,
			PersonRefusal: caveat.NotAnOption()}, "the-key",
			httpjson.Detail{"tool": worklet.UpdateWorkItemTool})
		return rec, heard.Said()
	}
	causes := map[string]error{
		"an outcome nobody can establish": crewletmcp.ErrOutcomeUnknown,
		"an unclassified failure":         errors.New("a tool that forgot to classify"),
	}
	for _, class := range crewletmcp.Refusals {
		causes[string(class)] = crewletmcp.Classify(class, errors.New("the reason"))
	}
	for name, cause := range causes {
		plain, toldPlain := answer(operator.FailTool, cause)
		matched, toldMatched := answer(operator.FailToolIfMatched, cause)
		if toldMatched != model || toldPlain != model {
			t.Errorf("%s: the model heard %q under If-Match and %q without it, want "+
				"the tool's sentence both times", name, toldMatched, toldPlain)
		}
		if strings.Contains(matched.Body.String(), "refused that") {
			t.Errorf("%s: the person's answer under If-Match carries the model's "+
				"sentence: %s", name, matched.Body)
		}
		if name == string(crewletmcp.RefusalStaleVersion) {
			body, class := decode(t, matched), decode(t, plain)
			if matched.Code != http.StatusPreconditionFailed ||
				body["error"] != string(httpjson.CodeVersionMoved) ||
				body["detail"] != class["detail"] || body["tool"] != worklet.UpdateWorkItemTool {
				t.Errorf("a stale version under If-Match answered %d %v, want 412 "+
					"version_moved carrying the person's words and what it was about",
					matched.Code, body)
			}
			continue
		}
		if matched.Code != plain.Code || matched.Body.String() != plain.Body.String() ||
			matched.Header().Get("Retry-After") != plain.Header().Get("Retry-After") {
			t.Errorf("%s: under If-Match %d %s, and through FailTool %d %s: only a "+
				"stale version may differ", name, matched.Code, matched.Body, plain.Code,
				plain.Body)
		}
	}
}

// THE LARGEST PAGE THE KNOWLEDGE BASE ACCEPTS FITS THE BODY, as the browser
// sends it: JSON.stringify escapes a quote, a backslash and a newline to two
// bytes each, so a legal page of nothing but those doubles on the wire, and a
// cap at the page's own size refused a page the store would have taken.
func TestTheLargestLegalPageFitsTheActBody(t *testing.T) {
	t.Parallel()
	pageBody := strings.Repeat("\"\\\n", doclet.MaxBody/3)
	pageBody += strings.Repeat("\"", doclet.MaxBody-len(pageBody))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// JSON.stringify does not escape <, > and &; Go's encoder does unless
	// told otherwise, and this is the browser's envelope being measured.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{
		"args": map[string]any{
			"container": "ENG", "title": strings.Repeat("T", doclet.MaxTitle),
			"body": pageBody, "labels": []string{"runbook", "security"},
			"message": strings.Repeat("m", 512),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= doclet.MaxBody*2 {
		t.Fatalf("the envelope is %d bytes; the case is not measuring the worst escape", buf.Len())
	}
	status, answer := act(t, guarded(actSurface(t, &recordingWork{})), "founder",
		builtin.WritePageTool, "application/json", buf.String(), newKey())
	if status != http.StatusOK {
		t.Fatalf("the largest legal page (%d bytes on the wire, cap %d) answered %d %v",
			buf.Len(), operator.MaxActBody, status, answer)
	}
	// AND THE CAP STILL BINDS just past it.
	over := `{"args":{"body":"` + strings.Repeat("x", operator.MaxActBody) + `"}}`
	if status, answer := act(t, guarded(actSurface(t, &recordingWork{})), "founder",
		builtin.WritePageTool, "application/json", over, newKey()); status != http.StatusRequestEntityTooLarge ||
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
		got := operator.ReceiptOf("t", tools.Result{Output: tc.output})
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
// has, so a person's assistant that wrote a page has something to hand back as
// `min_position` and the read after the write includes it.
func TestAPageWriteStatesItsOutcomeAndPosition(t *testing.T) {
	t.Parallel()
	kb := &positionedPages{}
	s := newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{Pages: builtin.PageDeps{Reader: kb, Writer: kb}}),
		Org:    company,
	})
	body := `{"args":{"page":"Runbook","body":"the step is wrong"}}`
	status, answer := act(t, guarded(s), "founder", builtin.CommentOnPageTool,
		"application/json", body, newKey())
	if status != http.StatusOK || answer["outcome"] != "pending" ||
		answer["position"] != "CREWLET_PAGES_LOG@2:88" {

		t.Fatalf("a page comment answered %d %v, want pending at CREWLET_PAGES_LOG@2:88",
			status, answer)
	}
}

// positionedPages is a knowledge base whose comment lands pending at a known
// position.
type positionedPages struct{ recordingPages }

func (p *positionedPages) Comment(ctx context.Context, actor doclet.Actor, id string,
	in doclet.NewComment) (doclet.Comment, doclet.Written, error) {

	comment, _, err := p.recordingPages.Comment(ctx, actor, id, in)
	return comment, doclet.Written{Revision: 88, Outcome: statelog.Result{
		Outcome:  statelog.OutcomePending,
		Position: statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 2, Seq: 88},
	}}, err
}

// A PAGE CREATE NOBODY ANSWERED NAMES WHERE TO LOOK.
//
// A new page has no id to name: write_page addresses it by its container and
// its title, so the answer to a create whose outcome nobody can establish names
// the container beside the tool — the object to read to see whether the page
// landed. It named the tool alone, because no declared object argument covered
// a page that does not exist yet.
//
// Mutation: drop `container` from the pages domain's objects (builtin's
// writerclass.go) and the answer names no container.
func TestAPageCreateNobodyAnsweredNamesItsContainer(t *testing.T) {
	t.Parallel()
	kb := &unansweredPages{}
	s := newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{Pages: builtin.PageDeps{Reader: kb, Writer: kb}}),
		Org:    company,
	})
	body := `{"args":{"container":"ENG","title":"Rollback runbook","body":"step one"}}`
	status, answer := act(t, guarded(s), "founder", builtin.WritePageTool,
		"application/json", body, newKey())
	if status != http.StatusServiceUnavailable ||
		answer["outcome"] != string(statelog.OutcomeUnknown) {

		t.Fatalf("a create nobody answered answered %d %v, want 503 with outcome "+
			"unknown", status, answer)
	}
	if answer["tool"] != builtin.WritePageTool || answer["container"] != "ENG" {
		t.Errorf("the answer names tool %v and container %v, want %s in ENG — "+
			"nothing else in it says where the page would be", answer["tool"],
			answer["container"], builtin.WritePageTool)
	}
}

// unansweredPages is a knowledge base whose create was sent to a data node
// that never answered.
type unansweredPages struct{ recordingPages }

func (*unansweredPages) Create(context.Context, doclet.Actor, doclet.NewPage) (doclet.Written, error) {
	return doclet.Written{}, fmt.Errorf("%w (pages.create, asked of data-b)",
		estate.ErrOutcomeUnknown)
}

// AN INTERRUPTED CALL SAYS, IN A FIELD, THAT NOBODY KNOWS WHETHER IT LANDED.
//
// Two of the transport's 503s carry the class `unavailable`: a tool that
// refused before writing anything (a node in maintenance, a sealed log), and
// a call whose context ended while the write was in flight. They mean
// opposite things to a person — "nothing happened, try again" and "it may
// have happened" — and the sentence alone is prose no client should parse.
// So the interrupted answer carries `outcome: unknown` and the operation to
// retry under, and the refusal does not, which is how the dashboard tells a
// person the truth about each. A caller that hung up is answered nothing, and
// the access log reads it as the client's.
func TestAnInterruptedActSaysItsOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("answer is not JSON: %q", rec.Body)
		}
		return body
	}
	key := newKey()
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+"create_work_item", nil)

	rec := httptest.NewRecorder()
	operator.Interrupted(rec, r, key, "create_work_item", context.DeadlineExceeded)
	body := decode(t, rec)
	if rec.Code != http.StatusServiceUnavailable || body["error"] != "unavailable" {
		t.Fatalf("an interrupted call answered %d %v, want 503 unavailable", rec.Code, body)
	}
	if body["outcome"] != string(statelog.OutcomeUnknown) || body["op_id"] != key {
		t.Errorf("an interrupted call answered %v, want outcome unknown under "+
			"op_id %s — a client reading only the class tells a person nothing "+
			"happened", body, key)
	}

	rec = httptest.NewRecorder()
	operator.Fail(rec, crewletmcp.Classify(crewletmcp.RefusalUnavailable,
		errors.New("the tracker is sealed for maintenance")),
		caveat.Refusal{}, key,
		httpjson.Detail{"tool": "create_work_item"})
	body = decode(t, rec)
	if rec.Code != http.StatusServiceUnavailable || body["error"] != "unavailable" {
		t.Fatalf("a tool's unavailable refusal answered %d %v", rec.Code, body)
	}
	if _, has := body["outcome"]; has {
		t.Errorf("a tool's refusal claims an outcome %v — it wrote nothing, and "+
			"saying unknown would tell a person it may have", body["outcome"])
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a refusal a wait can clear answered no Retry-After")
	}

	gone, cancel := context.WithCancel(t.Context())
	cancel()
	rec = httptest.NewRecorder()
	operator.Interrupted(rec, r.WithContext(gone), key, "create_work_item", context.Canceled)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("a call whose caller hung up was recorded as %d — a 5xx counts "+
			"a closed tab as this node failing", rec.Code)
	}
}

// THE NOTE ON A CALL'S ARGUMENTS IS THE MODEL'S, NEVER A PERSON'S (ADR-0059,
// ADR-0060): a person's receipt ([operator.PersonsReceipt], which the act
// route and the human write surface both answer through) is the tool's answer
// and carries neither the note nor a field for it, and the note is told in
// process to the model that wrote the call, the one reader it is worded for.
//
// Mutation: carry the note beside the receipt and its text is in the answer;
// drop TellNote and the assistant's model hears nothing.
func TestTheArgumentsNoteIsTheModelsAndNeverAPersons(t *testing.T) {
	t.Parallel()
	const note = "`ops[0].opp` is not an argument"
	result := tools.Result{Output: `{"key": "ENG-9", "outcome": "applied"}`, Note: note}
	answer := operator.ReceiptOf(worklet.CreateWorkItemTool, result)
	var receipt map[string]any
	if err := json.Unmarshal(answer.Receipt, &receipt); err != nil || receipt["key"] != "ENG-9" {
		t.Errorf("the receipt is %s (%v), want the tool's own JSON answer", answer.Receipt, err)
	}
	raw, _ := json.Marshal(answer)
	if strings.Contains(string(raw), "is not an argument") || strings.Contains(string(raw), "note") {
		t.Errorf("a person's answer carries the model's note: %s", raw)
	}
	ctx, heard := modelnote.Expect(t.Context())
	operator.TellNote(httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), result)
	if heard.Said() != note {
		t.Errorf("the model heard %q, want the note", heard.Said())
	}
}

// A PERSON IS ANSWERED A CAVEAT IN A PERSON'S WORDS, and never the model's.
//
// The incident: answering a decision in the dashboard left a toast that never
// went away, telling a person to "Use ask: backend-engineer", the receipt's
// `warnings` written for a model and shown verbatim. The act route answers
// each caveat's person phrasing with what it asks of the person, and takes the
// model's list off the receipt, so no screen reading the receipt can show it.
func TestAnActAnswersCaveatsInAPersonsWordsAndNeverTheModels(t *testing.T) {
	t.Parallel()
	gone := caveat.DefaultAssigneeGone("ENG", "bo")
	work := &recordingWork{caveats: []caveat.Caveat{gone}}
	h := guarded(actSurface(t, work))

	status, answer := act(t, h, "founder", "create_work_item", "application/json",
		createBody(), newKey())
	if status != http.StatusOK {
		t.Fatalf("the create answered %d: %v", status, answer)
	}
	receipt, _ := answer["receipt"].(map[string]any)
	if receipt == nil {
		t.Fatalf("the answer carries no receipt: %v", answer)
	}
	if said, carried := receipt[builtin.WarningsKey]; carried {
		t.Errorf("the receipt still carries the model's words, which a screen "+
			"reading it would show a person: %v", said)
	}
	if receipt["key"] != "ENG-9" {
		t.Errorf("the rest of the receipt was not kept: %v", receipt)
	}
	want := []any{map[string]any{"code": string(caveat.CodeDefaultAssigneeGone),
		"message": gone.Person(), "action": string(caveat.ActionRequired)}}
	if got := answer["caveats"]; !reflect.DeepEqual(got, want) {
		t.Errorf("caveats = %v, want %v", got, want)
	}

	// AND A WRITE WITH NOTHING TO ADD ANSWERS AN EMPTY LIST, never null,
	// so a client reads one shape.
	quiet := guarded(actSurface(t, &recordingWork{}))
	if _, answer := act(t, quiet, "founder", "create_work_item", "application/json",
		createBody(), newKey()); !reflect.DeepEqual(answer["caveats"], []any{}) {
		t.Errorf("a write with no caveats answered %#v, want []", answer["caveats"])
	}
}

// THE RECEIPT LOSES ITS MODEL WORDS AND NOTHING ELSE: a receipt that carries
// no warnings is answered exactly as the tool wrote it. A tool answer that is
// not a receipt at all (not a JSON object) is the tool's words to a model, so
// the write's outcome stands and the answer carries no receipt (ADR-0060).
func TestTheActReceiptLosesOnlyTheModelsWarnings(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ output, want string }{
		"warnings and fields":  {`{"key":"ENG-1","warnings":["Use ask: ana"]}`, `{"key":"ENG-1"}`},
		"no warnings":          {`{"key": "ENG-1"}`, `{"key": "ENG-1"}`},
		"not an object":        {`["a","b"]`, `null`},
		"a sentence, not JSON": {`Filed ENG-1; call get_work_item to read it.`, `null`},
	} {
		got := operator.ReceiptOf("t", tools.Result{Output: c.output})
		if string(got.Receipt) != c.want || got.Outcome != statelog.OutcomeApplied {
			t.Errorf("%s: receipt %s (%s), want %s applied", name, got.Receipt,
				got.Outcome, c.want)
		}
	}
}

// oneTaskReader is a work reader that holds one item, ENG-1.
type oneTaskReader struct{ stubWorkReader }

func (oneTaskReader) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {

	return worklet.TaskDetail{Task: worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG",
		Title: "Rotate the signing key"}}, nil
}

// THE ACT ROUTE TELLS THE MODEL THE NOTE AND NEVER THE PERSON (ADR-0059,
// ADR-0060), through the handler itself: a call that succeeds while one of its
// arguments was not read as sent (a key `checklist` does not take) is answered
// to the person with the receipt and nothing of the note, and the note is told
// to the model that made the request, in process.
//
// Mutation: drop TellNote from the act handler and the model hears nothing;
// carry the note beside the receipt and the body names it.
func TestTheActRouteTellsTheModelTheNoteAndNeverThePerson(t *testing.T) {
	t.Parallel()
	h := guarded(newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: oneTaskReader{}, Writer: (&recordingWork{}).writer},
			Pages: builtin.PageDeps{
				Reader: stubPageReader{}, Writer: stubPageWriter{},
			},
		}),
		Org: company,
	}))
	body, _ := json.Marshal(map[string]any{"args": map[string]any{
		"item": "ENG-1", "checklist": map[string]any{
			"op": "remove_list", "list": "l-1", "bogus": "x"}}})
	ctx, heard := modelnote.Expect(t.Context())
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+builtin.UpdateWorkItemTool,
		strings.NewReader(string(body))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer founder")
	r.Header.Set(opkey.Header, newKey())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("the update answered %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(heard.Said(), "checklist.bogus") {
		t.Errorf("the model heard %q, want the note on `checklist.bogus`", heard.Said())
	}
	if strings.Contains(rec.Body.String(), "bogus") {
		t.Errorf("the person's answer carries the model's note: %s", rec.Body)
	}
}

// AN OPERATOR CALL'S NUMBERS ARRIVE EXACTLY, on both of this surface's
// transports, as they do from a seat.
//
// Every other path into these tools reads a call's arguments through
// json.Number — a seat's from its provider — and the tools are written for
// that. The act and MCP transports decoded with the default decoder, which
// turns every JSON number into a float64 first, so an integer past 2^53 was
// rounded before any tool saw it: a whole-number custom-field value a person
// typed was filed as one they never typed, and nothing said so.
//
// Mutation: decode the act envelope's `args` as a map with its own decoder, or
// the MCP handler's with json.Unmarshal, and that transport's case goes red.
func TestAnOperatorCallsNumbersArriveExactly(t *testing.T) {
	t.Parallel()
	const past = "9007199254740993" // 2^53 + 1, the first integer a float64 rounds
	args := map[string]any{"title": "Rotate the signing key", "project": "ENG",
		"fields": map[string]any{"points": json.Number(past)}}
	filedExactly := func(t *testing.T, work *recordingWork) {
		t.Helper()
		filed := work.fieldsFiled()
		if len(filed) != 1 {
			t.Fatalf("the create reached the writer %d times", len(filed))
		}
		if got := string(filed[0]["points"]); got != past {
			t.Errorf("points was filed as %s, want %s as it was sent", got, past)
		}
	}

	t.Run("act", func(t *testing.T) {
		t.Parallel()
		work := &recordingWork{}
		raw, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		status, answer := act(t, guarded(actSurface(t, work)), "founder",
			worklet.CreateWorkItemTool, "application/json", string(raw), newKey())
		if status != http.StatusOK {
			t.Fatalf("the act answered %d %v", status, answer)
		}
		filedExactly(t, work)
	})

	t.Run("mcp", func(t *testing.T) {
		t.Parallel()
		work := &recordingWork{}
		dir := &directory{people: map[string]iam.Principal{"founder": founder}}
		server := httptest.NewServer(dir.middleware(actSurface(t, work).MCPHandler()))
		t.Cleanup(server.Close)
		sess := connectAs(t, server.URL+operator.MCPPath, "founder")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: worklet.CreateWorkItemTool, Arguments: args,
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("the create was refused: %+v", res.Content)
		}
		filedExactly(t, work)
	})
}
