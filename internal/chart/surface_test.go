package chart_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE `/chart` SURFACE OVER THIS RIG'S REAL WRITER.
//
// The surface's own suite drives a fake writer, which is right for what it
// asks — which verb a route reaches, what each outcome renders as — and can
// say nothing about what a request DOES to the rows: whether a body that left
// the runtime half out kept the one the object has, or whether a refusal the
// domain made reaches the caller as the refusal it is. Those are properties
// of the pair, so they are asked here, where the pair is real.

// surfaceRig is the chart surface mounted over a writer rig.
type surfaceRig struct {
	*writeRig
	mux *http.ServeMux
}

// serveChart mounts the surface over r, as who, with a lead relation that
// answers leads for every pair in led — a seat's handle or a unit's key.
func serveChart(r *writeRig, who iam.Principal, led ...string) surfaceRig {
	r.t.Helper()
	svc, err := chartapi.New(chartapi.Options{
		Reader: r.reader(),
		Authority: func(actor string, kind chart.AuthorKind, grants []iam.Grant,
			p chart.Provenance) chartapi.Writer {
			return r.writer.As(actor, kind, grants, p)
		},
		Principal: func(*http.Request) (iam.Principal, iam.Resolution) {
			return who, iam.Resolved
		},
		Chart: leadRelation(led),
	})
	if err != nil {
		r.t.Fatalf("chartapi.New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		r.t.Fatalf("Routes: %v", err)
	}
	return surfaceRig{writeRig: r, mux: mux}
}

// send serves one request while a consumer applies the log, so a write this
// node publishes is one it applies inside the resolve budget.
func (s surfaceRig) send(method, path, body string) *httptest.ResponseRecorder {
	s.t.Helper()
	rec := httptest.NewRecorder()
	s.whileDraining(func() {
		s.mux.ServeHTTP(rec, httptest.NewRequestWithContext(s.t.Context(),
			method, path, strings.NewReader(body)))
	})
	return rec
}

// leadRelation answers that its actor leads every object it names, and
// nothing else, whoever asks.
type leadRelation []string

func (l leadRelation) leads(subject string) bool {
	for _, led := range l {
		if led == subject {
			return true
		}
	}
	return false
}

func (l leadRelation) Leads(_ context.Context, _, subject string) (bool, error) {
	return l.leads(subject), nil
}

func (l leadRelation) LeadsUnit(_ context.Context, _, unit string) (bool, error) {
	return l.leads(unit), nil
}

func (l leadRelation) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (l leadRelation) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

func (l leadRelation) LeadsAnyone(context.Context, string) (bool, error) {
	return len(l) > 0, nil
}

// signedIn is a person bound to the seat `mira`, holding grants, who proved
// who they are a minute ago — inside both step-up windows, because every
// chart write asks for a recent proof and no case here is about its age.
func signedIn(grants ...iam.Grant) iam.Principal {
	at := time.Now().Add(-time.Minute)
	return iam.Principal{ID: uuid.New(), Login: "mira.lead", Seat: "mira",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: grants,
		ReauthAt: at.Add(time.Hour)}
}

// A LEAD'S PATCH THAT LEAVES THE RUNTIME OUT KEEPS IT, THROUGH THE SURFACE.
//
// A body is full post-state for every field but the runtime half, which the
// person editing a goal neither may change nor is shown. Before, the route
// admitted the lead and the domain read the missing half as a clear, so every
// lead's edit of a seat with a model chain was refused. And a lead's body that
// DOES state a runtime is refused at the route, naming the grant, before the
// domain is reached.
func TestALeadsPatchThroughTheSurfaceKeepsTheRuntimeItLeftOut(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))
	seeded := json.RawMessage(`{"llm":["zulu"],"mcp_env":{"gl":{"T":"${T}"}}}`)
	if _, err := r.writer.WriteSeat(t.Context(), "op-seed", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen", Runtime: seeded,
	}); err != nil {
		t.Fatalf("seed the runtime half: %v", err)
	}
	r.drain()
	s := serveChart(r, signedIn(), "sarah-chen")

	rec := s.send(http.MethodPatch, "/chart/seats/sarah-chen",
		`{"name":"Sarah Chen","goal":"ship the thing"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the lead's goal edit answered %d: %s", rec.Code, rec.Body)
	}
	seat := r.mustSeat("sarah-chen")
	if seat.Goal != "ship the thing" || string(seat.Runtime) != string(seeded) {
		t.Errorf("the seat reads goal %q and runtime %s, want the lead's goal "+
			"and its runtime %s", seat.Goal, seat.Runtime, seeded)
	}

	rec = s.send(http.MethodPatch, "/chart/seats/sarah-chen",
		`{"name":"Sarah Chen","runtime":{"llm":["haiku"]}}`)
	if rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), string(iam.GrantConfigWrite)) {
		t.Errorf("the lead's runtime write answered %d %s, want 403 naming %s",
			rec.Code, rec.Body, iam.GrantConfigWrite)
	}
}

// A LEAD'S PATCH OF A FIELD AUTHORITY IS DERIVED FROM IS REFUSED AS THE TABLE
// REFUSES, AND THE COMPANY'S GRANT MAKES IT.
//
// The route admits a lead to the seat they lead and cannot see which fields
// the body changes; the domain can, and refuses the one that would hand the
// lead another team's tracker project. The caller reads the refusal the
// authority table would have written — `403`, `no_grant`, `config:write` —
// with the field that asked, and a holder of the company's grant, who leads
// nothing, makes the same change.
func TestALeadsPatchOfAFieldAuthorityComesFromIsRefusedAsTheTableRefuses(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-tree",
		op(chart.OpCreateSeat, chart.KindSeat, "report", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "founder", ""),
	)
	if _, err := r.writer.WriteSeat(t.Context(), "op-seed", chart.SeatContent{
		Handle: "report", Name: "Report",
	}); err != nil {
		t.Fatalf("seed the seat: %v", err)
	}
	r.drain()
	body := `{"name":"Report","project":"FOUNDERS"}`

	rec := serveChart(r, signedIn(), "report").send(http.MethodPatch,
		"/chart/seats/report", body)
	var refusal struct {
		Error  string   `json:"error"`
		Reason string   `json:"reason"`
		Grants []string `json:"grants"`
		Fields []string `json:"fields"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	if rec.Code != http.StatusForbidden || refusal.Error != "unauthorized" ||
		refusal.Reason != "no_grant" ||
		strings.Join(refusal.Grants, ",") != string(iam.GrantConfigWrite) ||
		strings.Join(refusal.Fields, ",") != "project" {
		t.Fatalf("the lead's PATCH answered %d %s, want 403 unauthorized, "+
			"no_grant, [config:write] and the field [project]", rec.Code, rec.Body)
	}
	if got := r.mustSeat("report").Project; got != "" {
		t.Fatalf("the refused project reached the rows: %q", got)
	}

	rec = serveChart(r, signedIn(iam.GrantConfigWrite)).send(http.MethodPatch,
		"/chart/seats/report", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("the company's grant, leading nothing, answered %d: %s",
			rec.Code, rec.Body)
	}
	if got := r.mustSeat("report").Project; got != "FOUNDERS" {
		t.Errorf("after the company's grant the seat's project is %q, want FOUNDERS", got)
	}
}

// A SEAT'S `manages` IS STRUCTURE ON THE SURFACE TOO: A PATCH NAMING IT IS
// REFUSED, AND A LEAD'S SET_MANAGES STOPS AT THE DOOR.
//
// A PATCH carrying `manages` is refused `400 invalid_body` naming the field —
// the route does not read it, and a dropped field would answer 200 for a list
// that never landed — before anything is published. The list is a batch's
// `set_manages`, at the company's grant: a lead who leads the seat is refused
// `403` by the route's own verb, and the grant's holder lands it.
func TestASeatsManagesIsABatchNeverAPatch(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-tree",
		op(chart.OpCreateSeat, chart.KindSeat, "report", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "founder", ""),
	)
	if _, err := r.writer.WriteSeat(t.Context(), "op-seed", chart.SeatContent{
		Handle: "report", Name: "Report",
	}); err != nil {
		t.Fatalf("seed the seat: %v", err)
	}
	r.drain()
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	admin := serveChart(r, signedIn(iam.GrantConfigWrite))
	rec := admin.send(http.MethodPatch, "/chart/seats/report",
		`{"name":"Report","manages":["founder"]}`)
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "manages") {
		t.Errorf("a PATCH naming manages answered %d %s, want 400 naming the field",
			rec.Code, rec.Body)
	}
	if after, _ := r.log.End(t.Context()); after != before {
		t.Errorf("the log moved from %d to %d — a refused PATCH published a record",
			before, after)
	}

	batch := `{"operations":[{"kind":"set_manages",` +
		`"object":{"kind":"seat","id":"report"},"manages":["founder"]}]}`
	rec = serveChart(r, signedIn(), "report").send(http.MethodPost, "/chart/batch", batch)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a lead's set_manages answered %d %s, want 403", rec.Code, rec.Body)
	}
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = ?`,
		"report"); len(got) != 0 {
		t.Fatalf("the refused list reached the rows: %v", got)
	}

	rec = admin.send(http.MethodPost, "/chart/batch", batch)
	if rec.Code != http.StatusOK {
		t.Fatalf("the company's grant answered %d: %s", rec.Code, rec.Body)
	}
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = ?`,
		"report"); strings.Join(got, ",") != "founder" {
		t.Errorf("after the company's grant the seat manages %v, want [founder]", got)
	}
}

// A PATCH ON AN OBJECT THE CHART DOES NOT HOLD IS THE DOMAIN'S REFUSAL, `422
// refused`, NAMING THE ROUTE THAT CREATES ONE — AND NOTHING IS PUBLISHED.
//
// A content write never creates: a creation takes an address, which is exact
// only on the tree's one subject, and a content write contends on its object's
// own. Through the surface that refusal used to read `400 invalid_body`, which
// told the caller their body was malformed when it was the request's object
// that did not exist. And on a REMOVED seat the answer names the removal and
// its reason rather than sending somebody to create a seat dissolved on
// purpose. Mutation: answer the domain's refusal `400 invalid_body` again and
// both cases fail on the status; let the decide fall through to an upsert and
// the first fails on the log.
func TestAPatchOnAnObjectNobodyCreatedIsRefusedNamingTheBatch(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "omar", ""))
	if _, err := r.writer.WriteSeat(t.Context(), "op-seed", chart.SeatContent{
		Handle: "omar", Name: "Omar",
	}); err != nil {
		t.Fatalf("seed the seat: %v", err)
	}
	if _, err := r.writer.WithHolders(noHolders{}).WriteRemoval(t.Context(),
		"op-remove", chart.Batch{Reason: "left the company",
			Operations: []chart.Operation{
				op(chart.OpRemoveObject, chart.KindSeat, "omar", ""),
			}}); err != nil {
		t.Fatalf("remove omar: %v", err)
	}
	r.drain()
	s := serveChart(r, signedIn(iam.GrantConfigWrite), "sarah-chen", "omar")

	refused := func(rec *httptest.ResponseRecorder) (code, detail string) {
		var body struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Error, body.Detail
	}

	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	rec := s.send(http.MethodPatch, "/chart/seats/sarah-chen",
		`{"name":"Sarah Chen","goal":"ship the thing"}`)
	code, detail := refused(rec)
	if rec.Code != http.StatusUnprocessableEntity || code != "refused" ||
		!strings.Contains(detail, "POST /chart/batch") {
		t.Errorf("a PATCH on a seat nobody created answered %d %s, want 422 "+
			"refused naming POST /chart/batch", rec.Code, rec.Body)
	}
	after, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after != before {
		t.Errorf("the log moved from %d to %d — a refused PATCH published a record",
			before, after)
	}
	if got := r.column(`SELECT handle FROM chart_seats WHERE handle = 'sarah-chen'`); len(got) != 0 {
		t.Errorf("the chart holds %v, want no seat created by a content write", got)
	}

	rec = s.send(http.MethodPatch, "/chart/seats/omar", `{"name":"Omar"}`)
	code, detail = refused(rec)
	if rec.Code != http.StatusUnprocessableEntity || code != "refused" ||
		!strings.Contains(detail, "left the company") {
		t.Errorf("a PATCH on a removed seat answered %d %s, want 422 refused "+
			"naming the removal's reason", rec.Code, rec.Body)
	}
}
