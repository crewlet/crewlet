package chartapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// --- the fakes -------------------------------------------------------- //

// reader is a chart that answers one fixed company.
type reader struct {
	chart chart.Chart
	unit  chart.UnitDetail
	seat  chart.SeatDetail
	err   error
	// levels records what level each read was asked at, so a case can
	// assert the surface resolved one rather than passing the caller's
	// silence through.
	levels []statelog.ReadLevel
}

func (r *reader) Read(_ context.Context, f statelog.Freshness) (chart.Chart, error) {
	r.levels = append(r.levels, f.Level)
	return r.chart, r.err
}

func (r *reader) Unit(_ context.Context, _ string, f statelog.Freshness) (
	chart.UnitDetail, error) {

	r.levels = append(r.levels, f.Level)
	return r.unit, r.err
}

func (r *reader) Seat(_ context.Context, _ string, f statelog.Freshness) (
	chart.SeatDetail, error) {

	r.levels = append(r.levels, f.Level)
	return r.seat, r.err
}

func (r *reader) History(_ context.Context, _ int, f statelog.Freshness) (
	[]chart.Change, chart.Answer, error) {

	r.levels = append(r.levels, f.Level)
	return nil, chart.Answer{Level: f.Level}, r.err
}

func (r *reader) Imports(_ context.Context, _ int, f statelog.Freshness) (
	[]chart.Import, chart.Answer, error) {

	r.levels = append(r.levels, f.Level)
	return nil, chart.Answer{Level: f.Level}, r.err
}

func (r *reader) Import(_ context.Context, revision string, f statelog.Freshness) (
	chart.Import, bool, chart.Answer, error) {

	r.levels = append(r.levels, f.Level)
	if r.err != nil {
		return chart.Import{}, false, chart.Answer{}, r.err
	}
	if revision != "rev-1" {
		return chart.Import{}, false, chart.Answer{Level: f.Level}, nil
	}
	return chart.Import{Revision: revision, Objects: 2}, true,
		chart.Answer{Level: f.Level}, nil
}

// writer records which verb a route reached for, and answers as told.
//
// THE VERB IS WHAT THE CASES TURN ON. internal/chart publishes a placement
// and a removal as different records, each refusing a batch stating the
// other, so a route that called the wrong one would refuse exactly the
// gesture it was written to serve.
type writer struct {
	calls   []string
	outcome statelog.Outcome
	err     error
	opIDs   []string
}

func (w *writer) result(verb, opID string) (chart.WriteResult, error) {
	w.calls = append(w.calls, verb)
	w.opIDs = append(w.opIDs, opID)
	if w.err != nil {
		return chart.WriteResult{}, w.err
	}
	outcome := w.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	return chart.WriteResult{
		Result: statelog.Result{Outcome: outcome, OpID: opID},
	}, nil
}

func (w *writer) WriteUnit(_ context.Context, opID string, _ chart.UnitContent) (
	chart.WriteResult, error) {
	return w.result("unit", opID)
}

func (w *writer) WriteSeat(_ context.Context, opID string, _ chart.SeatContent) (
	chart.WriteResult, error) {
	return w.result("seat", opID)
}

func (w *writer) WriteBatch(_ context.Context, opID string, _ chart.Batch) (
	chart.WriteResult, error) {
	return w.result("batch", opID)
}

func (w *writer) WriteRemoval(_ context.Context, opID string, _ chart.Batch) (
	chart.WriteResult, error) {
	return w.result("removal", opID)
}

func (w *writer) WriteRekey(_ context.Context, opID string, _ chart.ObjectRef,
	_ string) (chart.WriteResult, error) {
	return w.result("rekey", opID)
}

func (w *writer) WriteImport(_ context.Context, opID, _ string, _ []chart.Edge) (
	chart.WriteResult, error) {
	return w.result("import", opID)
}

// rel is the lead relation, and it is DELIBERATELY THREE MAPS: a fake that
// answered one question with another would agree with a rule asking either.
type rel struct {
	units      map[[2]string]bool
	projects   map[[2]string]bool
	seats      map[[2]string]bool
	containers map[[2]string]bool
	err        error
}

func (c rel) Leads(_ context.Context, actor, subject string) (bool, error) {
	return c.answer(c.seats, actor, subject)
}

func (c rel) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	return c.answer(c.projects, actor, project)
}

func (c rel) LeadsUnit(_ context.Context, actor, unit string) (bool, error) {
	return c.answer(c.units, actor, unit)
}

func (c rel) LeadsContainer(_ context.Context, actor, container string) (bool, error) {
	return c.answer(c.containers, actor, container)
}

func (c rel) answer(in map[[2]string]bool, actor, subject string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return in[[2]string{actor, subject}], nil
}

// nimbus is the fixture: one team, one seat, and a runtime half on each.
func nimbus() chart.Chart {
	return chart.Chart{
		Units: []chart.Unit{{
			Key: "engineering", Name: "Engineering", Purpose: "ships the product",
			Runtime: json.RawMessage(`{"mcp_env":{"gitlab":{"GITLAB_TOKEN":"${GL}"}}}`),
		}},
		Seats: []chart.Seat{{
			Handle: "sre", Name: "SRE", UnitKey: "engineering",
			Runtime: json.RawMessage(`{"llm":"anthropic","mcp_env":{"datadog":{"DD_APP_KEY":"${DD}"}}}`),
		}},
	}
}

// leadOf is a person acting as the seat that leads `engineering`, holding NO
// capability at all — which is the ordinary state of somebody who runs a team
// and does not hold the deployment.
//
// PROVED A MINUTE AGO, inside both step-up windows: every write here asks for
// a recent proof, and a case that is about the lead relation or a grant must
// not be refused on the age of one; the step-up has its own case.
func leadOf(grants ...iam.Grant) iam.Principal {
	return proved(iam.Principal{ID: uuid.New(), Login: "jane.doe", Seat: "cto",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: grants})
}

// proved gives p a proof of identity a minute old, the way the guard composes
// a signed-in person's deadlines from their session.
func proved(p iam.Principal) iam.Principal {
	at := time.Now().Add(-time.Minute)
	p.ReauthAt = at.Add(time.Hour)
	p.SensitiveReauthAt = at.Add(15 * time.Minute)
	return p
}

// nobody is what an unauthenticated request resolves to.
func nobody() iam.Principal { return iam.Principal{} }

// rig is one stood-up surface and the parts a case asserts against.
type rig struct {
	mux    *http.ServeMux
	reader *reader
	writer *writer
}

// serve stands the surface up over a mux.
func serve(t *testing.T, r *reader, who iam.Principal, relation rel) rig {
	t.Helper()
	if r == nil {
		r = &reader{}
	}
	w := &writer{}
	svc, err := chartapi.New(chartapi.Options{
		Reader:    r,
		Authority: func(string, chart.AuthorKind, []iam.Grant) chartapi.Writer { return w },
		Principal: resolved(func() iam.Principal { return who }),
		Chart:     relation,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return rig{mux: mux, reader: r, writer: w}
}

// leads is the relation every write case below decides against.
func leads() rel {
	return rel{units: map[[2]string]bool{{"cto", "engineering"}: true}}
}

// --- the reads -------------------------------------------------------- //

// THE STRIPPED POSTURE IS WHAT A READER GETS BY SAYING NOTHING.
//
// The runtime half of every chart object is a seat's model chain, its
// credentials, its sandbox cell and its mcp_env — the NAMES of every
// credential the company holds, which is a map of what to attack. A default
// that served it is a default nobody notices for as long as it works, so the
// opt-in is the whole guard: strip-when-asked would leak to every client that
// never heard of the parameter.
func TestTheStrippedPostureIsServedByDefault(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()}, leadOf(iam.GrantStateRead), leads())
	body := getJSON(t, r.mux, "/chart", http.StatusOK)

	if raw := string(mustMarshal(t, body)); strings.Contains(raw, "GITLAB_TOKEN") ||
		strings.Contains(raw, "DD_APP_KEY") || strings.Contains(raw, "mcp_env") {

		t.Fatalf("a default read served the runtime half:\n%s", raw)
	}
	// AND IT SAYS SO. Without the flag a company whose seats declare no
	// runtime renders exactly like a caller silently stripped, and a
	// client cannot tell whether to go and ask somebody for a grant.
	if body["runtime"] != false {
		t.Errorf("runtime = %v, want the answer to say it is stripped", body["runtime"])
	}
	// THE PUBLIC HALF IS STILL THERE, which is the control: a case that
	// passed over an empty answer would certify nothing.
	if !strings.Contains(string(mustMarshal(t, body)), "Engineering") {
		t.Error("the stripped answer carries no public half either")
	}
}

// AND ASKING FOR IT WITHOUT THE GRANT IS STILL STRIPPED, NOT REFUSED.
//
// The rows a caller asked for are rows they may read; a 403 over a field they
// will not miss would fail a request that can be answered. What they must not
// get is silence about it — see the flag asserted above.
func TestAskingForTheRuntimeHalfWithoutTheGrantIsStrippedNotRefused(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()}, leadOf(iam.GrantStateRead), leads())
	body := getJSON(t, r.mux, "/chart?runtime=true", http.StatusOK)

	if body["runtime"] != false {
		t.Errorf("runtime = %v, want false", body["runtime"])
	}
	if strings.Contains(string(mustMarshal(t, body)), "mcp_env") {
		t.Error("the runtime half was served to a caller without the grant")
	}
}

// AND WITH THE GRANT IT ARRIVES.
//
// The control for both cases above: without it they would pass over a surface
// that had simply lost the ability to serve the runtime half at all.
func TestTheRuntimeHalfIsServedToACallerThatMayReadIt(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()},
		leadOf(iam.GrantStateRead, iam.GrantConfigRead), leads())
	body := getJSON(t, r.mux, "/chart?runtime=true", http.StatusOK)

	if body["runtime"] != true {
		t.Fatalf("runtime = %v, want true", body["runtime"])
	}
	if !strings.Contains(string(mustMarshal(t, body)), "mcp_env") {
		t.Error("the runtime half was withheld from a caller that may read it")
	}
}

// EVERY READ RESOLVES A LEVEL, and never passes the caller's silence through.
//
// The framework refuses a read that names none, so a surface that forwarded
// an empty level would fail every request; what this holds is the stronger
// claim that it resolves the OPERATOR default, which is what stops somebody
// reading a structure they just changed from a node that is behind.
func TestAReadResolvesTheOperatorDefaultRatherThanPassingSilenceThrough(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()}, leadOf(iam.GrantStateRead), leads())
	getJSON(t, r.mux, "/chart", http.StatusOK)
	if len(r.reader.levels) != 1 || r.reader.levels[0] == "" {
		t.Fatalf("levels = %v, want one resolved level", r.reader.levels)
	}
	if want := statelog.LevelFor(statelog.SurfaceOperator, ""); r.reader.levels[0] != want {
		t.Errorf("level = %q, want the operator default %q", r.reader.levels[0], want)
	}
}

// A PARAMETER NOBODY READS IS REFUSED, over the union of this surface's own
// keys and the freshness grammar's.
//
// The grammar refuses an unknown key for its own good reason, and narrowing
// the query string to what it parses is what lets `runtime` through it. Doing
// that without extending the refusal would silently serve the stripped answer
// to somebody who wrote `runtimes=true` — indistinguishable, from the client,
// from a company with no runtime at all.
func TestAMisspelledParameterIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()},
		leadOf(iam.GrantStateRead, iam.GrantConfigRead), leads())
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"http://x/chart?runtimes=true", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400: %s", rec.Code, rec.Body)
	}
}

// --- the writes ------------------------------------------------------- //

// A CONTENT WRITE IS DECIDED BY THE HALF ITS BODY CARRIES.
//
// The two are different authorities on purpose: gate everything on the
// company's grant and a lead cannot correct their own seat's goal; gate
// everything on the lead relation and anybody who leads a unit can hand
// themselves a credential. The pattern cannot tell them apart — only the body
// can — so the route asks again once it has read one.
func TestAWriteCarryingTheRuntimeHalfIsDecidedAsAnOperatorWrite(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		who   iam.Principal
		body  string
		want  int
		wrote bool
	}{
		{"a lead edits the public half", leadOf(),
			`{"name":"Engineering"}`, http.StatusOK, true},
		{"the same lead may not write the runtime half", leadOf(),
			`{"name":"Engineering","runtime":{"mcp_env":{"gitlab":{"T":"${X}"}}}}`,
			http.StatusForbidden, false},
		{"the company's own grant may", leadOf(iam.GrantConfigWrite),
			`{"name":"Engineering","runtime":{"mcp_env":{"gitlab":{"T":"${X}"}}}}`,
			http.StatusOK, true},
		{"somebody who leads nothing may not edit even the public half",
			proved(iam.Principal{ID: uuid.New(), Login: "sre", Seat: "sre",
				Kind: iam.KindPerson, Stage: iam.StageActive}),
			`{"name":"Engineering"}`, http.StatusForbidden, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, c.who, leads())
			rec := patch(r.mux, "/chart/units/engineering", c.body)
			if rec.Code != c.want {
				t.Fatalf("answered %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
			if wrote := len(r.writer.calls) > 0; wrote != c.wrote {
				t.Errorf("wrote = %v, want %v (calls %v)",
					wrote, c.wrote, r.writer.calls)
			}
		})
	}
}

// AN UNAUTHENTICATED REQUEST IS NOBODY, and nobody decides nothing.
func TestAnUnauthenticatedRequestIsRefused(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()}, nobody(), leads())
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/chart", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("answered %d, want the table to refuse a principal with no "+
			"stage at all", rec.Code)
	}
}

// AN UNDECIDABLE WRITE IS 503, NOT 403.
//
// A node that cannot reach the chart cannot say who leads a unit. Answering
// 403 sends a lead to ask for an authority they already hold; 503 tells them
// to try again, and the next attempt works.
func TestAWriteThisNodeCannotDecideIsNotForbidden(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(), rel{err: authz.ErrNoChart})
	rec := patch(r.mux, "/chart/units/engineering", `{"name":"E"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("answered %d, want 503 — a node that cannot tell has not refused",
			rec.Code)
	}
	// WITH A Retry-After, which is what a client waits on: a 503 without
	// one says "try again" and not when.
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the undecidable write carries no Retry-After")
	}
}

// A WRITE REFUSED ON ITS BODY NAMES THE GRANT IT NEEDS, exactly as one
// refused at its pattern does.
//
// The body's decision wrote a refusal of its own that answered the reason
// alone, so a lead refused the runtime half was told `no_grant` and not WHICH
// grant — the one fact that says whom to ask.
func TestAWriteRefusedOnItsBodyNamesTheGrant(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(), leads())
	rec := patch(r.mux, "/chart/units/engineering",
		`{"name":"Engineering","runtime":{"mcp_env":{"gitlab":{"T":"${X}"}}}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("answered %d, want 403: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error   string   `json:"error"`
		Message string   `json:"message"`
		Reason  string   `json:"reason"`
		Grants  []string `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (%s)", err, rec.Body)
	}
	if body.Error != string(httpjson.CodeUnauthorized) || body.Message == "" ||
		body.Reason != string(authz.ReasonNoGrant) ||
		!slices.Equal(body.Grants, []string{string(iam.GrantConfigWrite)}) {
		t.Errorf("refusal = %+v, want unauthorized, its sentence, no_grant and "+
			"[config:write]", body)
	}
}

// A BATCH GOES TO THE VERB ITS OPERATIONS NAME.
//
// internal/chart publishes a placement and a removal as different records and
// each refuses a batch stating the other, because a removal installs a GATE
// and a record that installed one for some of its objects and not others
// would make "does this install a gate" a question about a payload. A route
// that sent everything to one verb would refuse every removal.
func TestABatchReachesThePlacementOrTheRemovalVerb(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		body string
		want string
	}{
		{"a hire", `{"operations":[{"kind":"create_seat","object":{"kind":"seat","id":"ana"}}]}`,
			"batch"},
		{"a departure", `{"operations":[{"kind":"remove","object":{"kind":"seat","id":"ana"}}],` +
			`"reason":"left"}`, "removal"},
		// A MIXED BATCH GOES TO THE PLACEMENT VERB and is refused
		// THERE, in the domain's own words: this surface restating that
		// rule would be a second copy of it.
		{"both at once", `{"operations":[` +
			`{"kind":"create_seat","object":{"kind":"seat","id":"ana"}},` +
			`{"kind":"remove","object":{"kind":"seat","id":"bo"}}]}`, "batch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
			rec := post(r.mux, "/chart/batch", c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("answered %d: %s", rec.Code, rec.Body)
			}
			if len(r.writer.calls) != 1 || r.writer.calls[0] != c.want {
				t.Errorf("reached %v, want %q", r.writer.calls, c.want)
			}
		})
	}
}

// A BATCH THAT CHANGES NOTHING IS REFUSED rather than published.
func TestAnEmptyBatchIsRefused(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	if rec := post(r.mux, "/chart/batch", `{"operations":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("answered %d, want 400", rec.Code)
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("published %v for a batch that changes nothing", r.writer.calls)
	}
}

// A 200 MEANS THE NEXT READ HERE SEES IT, and the other two outcomes say so
// rather than claiming it.
//
// The framework reports applied, pending and unknown, and only APPLIED means
// the rows the caller is about to read are the rows this write produced. A
// surface that answered 200 to all three would make `200` an
// acknowledgement, which is exactly the promise a caller polling for their
// own change relies on.
func TestATwoHundredMeansTheNextReadSeesIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		outcome statelog.Outcome
		want    int
	}{
		{statelog.OutcomeApplied, http.StatusOK},
		{statelog.OutcomePending, http.StatusAccepted},
		{statelog.OutcomeUnknown, http.StatusServiceUnavailable},
	} {
		t.Run(string(c.outcome), func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, leadOf(), leads())
			r.writer.outcome = c.outcome
			rec := patch(r.mux, "/chart/units/engineering", `{"name":"E"}`)
			if rec.Code != c.want {
				t.Fatalf("%s answered %d, want %d: %s",
					c.outcome, rec.Code, c.want, rec.Body)
			}
		})
	}
}

// AN UNKNOWN OUTCOME CARRIES THE ID A RETRY MUST REUSE, and the route reads
// it back.
//
// Retrying under a FRESH id would write the change twice if the first had in
// fact landed, which is the one thing the operation ledger exists to prevent
// — so an answer that did not carry the id would make the safe retry
// impossible to perform.
func TestAnUnknownOutcomeHandsBackTheIdThatMakesARetrySafe(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(), leads())
	r.writer.outcome = statelog.OutcomeUnknown
	rec := patch(r.mux, "/chart/units/engineering", `{"name":"E"}`)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	got, _ := body["op_id"].(string)
	if got == "" || got != r.writer.opIDs[0] {
		t.Fatalf("op_id = %q, want the id the write was published under (%v)",
			got, r.writer.opIDs)
	}
	// AND SENDING IT BACK REUSES IT. A route that minted a fresh one
	// anyway would carry the id for decoration.
	r.writer.outcome = statelog.OutcomeApplied
	rec = patchWith(r.mux, "/chart/units/engineering", `{"name":"E"}`,
		map[string]string{chartapi.IdempotencyHeader: got})
	if rec.Code != http.StatusOK {
		t.Fatalf("the retry answered %d: %s", rec.Code, rec.Body)
	}
	if r.writer.opIDs[1] != got {
		t.Errorf("the retry published under %q, want %q", r.writer.opIDs[1], got)
	}
}

// A WRITE THAT DID NOT LAND SAYS WHAT TO DO ABOUT IT: re-read, or wait and
// for how long.
//
// A lost race was `409 bad_params` — a sentence about a query parameter,
// telling the caller to change a request that only needed re-reading — and
// every one of these 503s went out with no Retry-After, so a client was told
// to come back and not when. The node that is behind its log knows roughly
// how long it will take (its backlog over its measured drain), and a hint it
// derived must survive the trip to the header rather than being flattened.
func TestAWriteThatDidNotLandSaysWhatToDoAboutIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		err        error
		outcome    statelog.Outcome
		status     int
		code       httpjson.Code
		retryAfter string
	}{
		{
			name:   "a race another writer won is stale",
			err:    fmt.Errorf("chart: unit engineering moved: %w", statelog.ErrConflict),
			status: http.StatusConflict, code: httpjson.CodeStale,
		},
		{
			name: "a node behind its log says how long, from its own backlog",
			err: &statelog.Refused{Code: statelog.RefuseBehind,
				Level: statelog.ReadSession, RetryAfter: 7 * time.Second},
			status: http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
			retryAfter: "7",
		},
		{
			name:   "an unavailable log with no hint of its own gets the undecidable scale",
			err:    fmt.Errorf("chart: publish: %w", statelog.ErrUnavailable),
			status: http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
			retryAfter: strconv.Itoa(authz.RetryUndecidedSeconds),
		},
		{
			// A NODE BEHIND THE CALLER'S OWN WRITE catches up, and a
			// write refusal carries no hint of its own.
			name: "a write refused because this node is behind says the undecidable scale",
			err: fmt.Errorf("chart: publish: %w",
				&statelog.Unavailable{Reason: statelog.ReasonBehind}),
			status: http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
			retryAfter: strconv.Itoa(authz.RetryUndecidedSeconds),
		},
		{
			// AN EVICTED NODE publishes nothing anybody applies, however
			// often it is asked: "come back in two seconds" would have a
			// client poll it until somebody readmits it.
			name: "a write refused for good says nothing about coming back",
			err: fmt.Errorf("chart: publish: %w",
				&statelog.Unavailable{Reason: statelog.ReasonEvicted}),
			status: http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
		},
		{
			name: "a read refusal waiting cannot clear says nothing about coming back",
			err: &statelog.Refused{Code: statelog.RefuseDeferred,
				Level: statelog.ReadSession},
			status: http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
		},
		{
			name:    "an unknown outcome is retried with the same id, and says when",
			outcome: statelog.OutcomeUnknown,
			status:  http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
			retryAfter: strconv.Itoa(authz.RetryUndecidedSeconds),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, leadOf(), leads())
			r.writer.err, r.writer.outcome = c.err, c.outcome
			rec := patch(r.mux, "/chart/units/engineering", `{"name":"E"}`)
			if rec.Code != c.status {
				t.Fatalf("answered %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v: %s", err, rec.Body)
			}
			if body["error"] != string(c.code) {
				t.Errorf("error = %v, want %s", body["error"], c.code)
			}
			if body["message"] != c.code.Message() {
				t.Errorf("message = %v, want the code's own sentence", body["message"])
			}
			if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
		})
	}
}

// A READ THIS NODE WILL NEVER SERVE SAYS NOTHING ABOUT COMING BACK, and one it
// will serve soon says when — rounded UP, so a client is never sent back
// before the node could have caught up.
//
// The read path fell back to the undecidable two seconds for every refusal
// without a derived hint, and a refusal waiting cannot clear carries none by
// design: a node holding a record it cannot decode told every client to poll
// it every two seconds until somebody upgraded it.
func TestAReadRefusalSaysWhetherAndWhenToComeBack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		err        error
		retryAfter string
	}{
		{"a node holding a record it cannot decode",
			&statelog.Refused{Code: statelog.RefuseDeferred, Level: statelog.ReadStale}, ""},
		{"an evicted node",
			&statelog.Refused{Code: statelog.RefuseEvicted, Level: statelog.ReadStale}, ""},
		{"a node behind its log, from its own backlog",
			&statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadStale,
				RetryAfter: 1200 * time.Millisecond}, "2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, &reader{err: c.err}, leadOf(iam.GrantStateRead), leads())
			rec := httptest.NewRecorder()
			r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/chart", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("answered %d, want 503: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
		})
	}
}

// A RENAME NAMES THE ADDRESS IT IS ADDRESSED BY, and refuses the one that
// changes nothing.
func TestARenameGoesToTheRekeyVerb(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	if rec := post(r.mux, "/chart/units/engineering/rename",
		`{"to":"platform"}`); rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body)
	}
	if len(r.writer.calls) != 1 || r.writer.calls[0] != "rekey" {
		t.Fatalf("reached %v, want the rekey verb", r.writer.calls)
	}
	// THE SAME ADDRESS IS THE FORM THAT DID NOTHING, and the message a
	// caller can act on is here rather than in the domain's.
	rec := post(r.mux, "/chart/units/engineering/rename", `{"to":"engineering"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("renaming to the current address answered %d, want 400", rec.Code)
	}
}

// --- the surface itself ----------------------------------------------- //

// EVERY ROUTE THIS SURFACE MOUNTS IS DECIDED, and the walk is what says so.
//
// A route mounted straight on the mux carries no policy and is invisible to
// the router, which is precisely the shape an ungated chart route takes: it
// works, it is never refused, and nothing anywhere reports it.
func TestEveryChartRouteIsMountedThroughTheAuthorityTable(t *testing.T) {
	t.Parallel()
	seen := &recordingMux{}
	svc, err := chartapi.New(chartapi.Options{
		Reader:    &reader{},
		Authority: func(string, chart.AuthorKind, []iam.Grant) chartapi.Writer { return &writer{} },
		Principal: resolved(nobody),
		Chart:     rel{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Routes(seen); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(seen.patterns) == 0 {
		t.Fatal("this surface mounted nothing")
	}
	// EVERY MOUNTED HANDLER REFUSES A PRINCIPAL THAT IS NOBODY. A route
	// registered outside the router would serve one.
	for _, pattern := range seen.patterns {
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			t.Fatalf("%q is not a method-and-path pattern", pattern)
		}
		rec := httptest.NewRecorder()
		seen.handlers[pattern].ServeHTTP(rec, httptest.NewRequest(method,
			"http://x"+strings.NewReplacer("{key}", "engineering",
				"{handle}", "sre", "{revision}", "rev-1").Replace(path),
			strings.NewReader("{}")))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s answered %d to a principal that is nobody, want 403",
				pattern, rec.Code)
		}
	}
}

// A SURFACE BUILT WITHOUT ITS PARTS IS REFUSED WHERE IT IS BUILT.
//
// An org chart mounted with no authority is the whole company writable by
// anybody who can reach the port, and a nil is exactly the shape a wiring
// mistake takes.
func TestASurfaceBuiltWithoutItsPartsIsRefused(t *testing.T) {
	t.Parallel()
	full := chartapi.Options{
		Reader:    &reader{},
		Authority: func(string, chart.AuthorKind, []iam.Grant) chartapi.Writer { return &writer{} },
		Principal: resolved(nobody),
		Chart:     rel{},
	}
	for _, c := range []struct {
		name string
		drop func(*chartapi.Options)
		want string
	}{
		{"no reader", func(o *chartapi.Options) { o.Reader = nil }, "Reader is required"},
		{"no authority", func(o *chartapi.Options) { o.Authority = nil }, "Authority is required"},
		{"no principal", func(o *chartapi.Options) { o.Principal = nil }, "Principal is required"},
		{"no chart", func(o *chartapi.Options) { o.Chart = nil }, "Chart is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			opts := full
			c.drop(&opts)
			if _, err := chartapi.New(opts); err == nil ||
				!strings.Contains(err.Error(), c.want) {

				t.Errorf("err = %v, want it to name %q", err, c.want)
			}
		})
	}
	// AND THE FULL SET IS ACCEPTED, which is the control: a constructor
	// that refused everything would pass every case above.
	if _, err := chartapi.New(full); err != nil {
		t.Errorf("a fully wired surface was refused: %v", err)
	}
}

// --- helpers ---------------------------------------------------------- //

// recordingMux is a mux that remembers what was mounted on it.
type recordingMux struct {
	patterns []string
	handlers map[string]http.Handler
}

func (m *recordingMux) Handle(pattern string, h http.Handler) {
	if m.handlers == nil {
		m.handlers = map[string]http.Handler{}
	}
	m.patterns = append(m.patterns, pattern)
	m.handlers[pattern] = h
}

func getJSON(t *testing.T, mux *http.ServeMux, path string, want int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x"+path, nil))
	if rec.Code != want {
		t.Fatalf("%s answered %d, want %d: %s", path, rec.Code, want, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v: %s", path, err, rec.Body)
	}
	return out
}

func patch(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	return patchWith(mux, path, body, nil)
}

func patchWith(mux *http.ServeMux, path, body string,
	headers map[string]string) *httptest.ResponseRecorder {

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "http://x"+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

func post(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://x"+path,
		strings.NewReader(body)))
	return rec
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// --- the fleet gate ---------------------------------------------------- //

// fleetSays is a fleet that answers as told.
type fleetSays struct {
	reason string
	err    error
}

func (f fleetSays) ImportReady(context.Context) (string, error) {
	return f.reason, f.err
}

// TestAnImportIsRefusedWhileTheFleetIsMixedVersion, naming the node.
//
// An import rewrites EVERY placement in the chart, in one record on the
// structure's own subject, and every node applies it — including one running
// an older build, under its own reading of what a placement means. The two
// are each individually correct and jointly wrong, which is the whole reason
// the protocol is versioned.
func TestAnImportIsRefusedWhileTheFleetIsMixedVersion(t *testing.T) {
	t.Parallel()
	const body = `{"revision":"rev-1","edges":[` +
		`{"object":{"kind":"unit","id":"engineering"}}]}`

	for _, c := range []struct {
		name  string
		fleet chartapi.Fleet
		want  int
		wrote bool
	}{
		{"a node is still on the older protocol",
			fleetSays{reason: "node-2 is still running protocol 3"},
			http.StatusConflict, false},
		// CANNOT TELL IS NOT A REFUSAL. The fleet was not read, so
		// nothing was established — and the remedy is a retry rather
		// than finishing an upgrade that may not be happening.
		{"this node cannot read the fleet",
			fleetSays{err: errors.New("the coordination store is unreachable")},
			http.StatusServiceUnavailable, false},
		// AND THE CONTROL: a uniform fleet lands.
		{"the fleet is uniform", fleetSays{}, http.StatusOK, true},
		// A SURFACE WITH NO FLEET BEHIND IT applies no gate, which is
		// what a single-node harness has.
		{"no fleet at all", nil, http.StatusOK, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := &writer{}
			svc, err := chartapi.New(chartapi.Options{
				Reader:    &reader{},
				Authority: func(string, chart.AuthorKind, []iam.Grant) chartapi.Writer { return w },
				Principal: resolved(func() iam.Principal {
					return leadOf(iam.GrantConfigWrite)
				}),
				Chart: leads(), Fleet: c.fleet,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			mux := http.NewServeMux()
			if err := svc.Routes(mux); err != nil {
				t.Fatalf("Routes: %v", err)
			}
			rec := post(mux, "/chart/import", body)
			if rec.Code != c.want {
				t.Fatalf("answered %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
			if wrote := len(w.calls) > 0; wrote != c.wrote {
				t.Errorf("published = %v, want %v", wrote, c.wrote)
			}
			if c.want == http.StatusConflict &&
				!strings.Contains(rec.Body.String(), "node-2") {

				t.Errorf("the refusal does not name the lagging node: %s", rec.Body)
			}
			// ITS OWN CODE, which the CLI and the fleet guide name: it
			// was written into the detail beside `bad_params`, where the
			// envelope's reserved key overwrote it.
			var answer struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &answer)
			if c.want == http.StatusConflict &&
				answer.Error != string(httpjson.CodeFleetMixedVersion) {

				t.Errorf("error = %q, want %q", answer.Error, httpjson.CodeFleetMixedVersion)
			}
		})
	}
}

// resolved adapts a principal source to the three-valued seam.
//
// EVERY CASE HERE IS ABOUT THE AUTHORITY TABLE rather than about the
// resolution, so they all say [iam.Resolved] and this says it once. The
// unknown arm has its own case, which is the only place a test should be
// spelling a resolution out.
func resolved(of func() iam.Principal) chartapi.Principal {
	return func(*http.Request) (iam.Principal, iam.Resolution) {
		return of(), iam.Resolved
	}
}

// A BODY OVER THE CAP IS ANSWERED 413, not abandoned.
//
// The body reader refused it and the handler returned without writing a
// status, so the caller was answered an empty 200 — which reads, to every
// client, as the write having landed.
func TestAnOversizedWriteIsAnsweredRatherThanDropped(t *testing.T) {
	t.Parallel()
	r := serve(t, &reader{chart: nimbus()}, leadOf(), leads())
	body := `{"name":"` + strings.Repeat("x", chartapi.MaxBodyBytes) + `"}`
	if rec := patch(r.mux, "/chart/units/engineering", body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized write answered %d, want 413", rec.Code)
	}
}
