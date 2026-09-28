package chartapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	"github.com/crewlet/crewlet/internal/redact"
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
	batches []chart.Batch
	imports [][]chart.Edge
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

func (w *writer) WriteBatch(_ context.Context, opID string, batch chart.Batch) (
	chart.WriteResult, error) {
	w.batches = append(w.batches, batch)
	return w.result("batch", opID)
}

func (w *writer) WriteRemoval(_ context.Context, opID string, _ chart.Batch) (
	chart.WriteResult, error) {
	return w.result("removal", opID)
}

func (w *writer) WriteImport(_ context.Context, opID, _ string, edges []chart.Edge) (
	chart.WriteResult, error) {
	w.imports = append(w.imports, edges)
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

// LeadsAnyone is [rel.Leads] asked of every subject — read off the seats map
// rather than a map of its own, since it is the same relation.
func (c rel) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	for pair, led := range c.seats {
		if led && pair[0] == actor {
			return true, nil
		}
	}
	return false, nil
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
		Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer { return w },
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

// servedSeats stands the surface up over company with held as its directory
// seam, and answers the handles GET path lists.
//
// AS SOMEBODY WHO MAY ASK THE DIRECTORY'S QUESTION — the board's read and the
// grant that invites people into seats — because every case using this is
// about what the filters return; who may ask for `unheld` at all has its own
// case.
func servedSeats(t *testing.T, company chart.Chart, held chartapi.Held,
	path string, want int) []string {

	t.Helper()
	svc, err := chartapi.New(chartapi.Options{
		Reader: &reader{chart: company},
		Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer {
			return &writer{}
		},
		Principal: resolved(func() iam.Principal {
			return leadOf(iam.GrantStateRead, iam.GrantPeopleManage)
		}),
		Chart: leads(),
		Held:  held,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	body := getJSON(t, mux, path, want)
	seats, _ := body["seats"].([]any)
	listed := []string{}
	for _, s := range seats {
		seat, _ := s.(map[string]any)
		handle, _ := seat["handle"].(string)
		listed = append(listed, handle)
	}
	return listed
}

// THE SEAT LISTING'S FILTERS ARE REACHABLE, and they filter.
//
// The surface refuses a parameter nobody reads BEFORE the handler runs, and
// `kind` and `unheld` were never on the list it refuses against — so the
// documented `GET /chart/seats?kind=human&unheld=true` answered 400 as an
// unknown parameter, and neither filter could be asked at all.
func TestTheSeatListingFiltersByKindAndByWhoHoldsASeat(t *testing.T) {
	t.Parallel()
	company := chart.Chart{Seats: []chart.Seat{
		{Handle: "designer", Kind: chart.SeatHuman},
		{Handle: "sre", Kind: chart.SeatAgent},
		{Handle: "writer", Kind: chart.SeatHuman},
	}}
	held := func(context.Context) (map[string]bool, error) {
		return map[string]bool{"writer": true}, nil
	}

	if got := servedSeats(t, company, held, "/chart/seats?kind=human",
		http.StatusOK); !slices.Equal(got, []string{"designer", "writer"}) {
		t.Errorf("kind=human listed %v, want designer and writer", got)
	}
	if got := servedSeats(t, company, held, "/chart/seats?kind=human&unheld=true",
		http.StatusOK); !slices.Equal(got, []string{"designer"}) {
		t.Errorf("kind=human&unheld=true listed %v, want only designer", got)
	}
	// AND A NODE WITH NO DIRECTORY REFUSES THE FILTER rather than applying
	// it to an empty one, which would list every human seat.
	servedSeats(t, company, nil, "/chart/seats?unheld=true",
		http.StatusServiceUnavailable)
	// AND SO DOES ONE WHOSE DIRECTORY IS THERE AND CANNOT BE READ: read as
	// false, every seat it failed on was listed as one nobody holds.
	unreadable := func(context.Context) (map[string]bool, error) {
		return nil, errors.New("the replicated estate is not open")
	}
	servedSeats(t, company, unreadable, "/chart/seats?unheld=true",
		http.StatusServiceUnavailable)
	// WHILE THE UNFILTERED LISTING NEVER ASKS IT, and is served.
	if got := servedSeats(t, company, unreadable, "/chart/seats?kind=human",
		http.StatusOK); !slices.Equal(got, []string{"designer", "writer"}) {
		t.Errorf("kind=human over an unreadable directory listed %v", got)
	}
}

// WHO HOLDS NOBODY IS THE DIRECTORY'S QUESTION, AND TAKES ITS GRANT.
//
// `unheld=true` lists the human seats no person in the identity directory is
// bound to — a fact about that estate, read through the chart. The board's
// read grant says nothing about the directory, so a reader holding it alone is
// refused naming what the directory's own listing takes: `people:manage`, the
// grant that invites somebody into one of those seats, or `audit:read`. Each of
// those is admitted, and the kind filter alone stays the board's.
//
// Mutation: drop the re-ask and the board's reader lists every vacancy.
func TestTheUnheldFilterIsADirectoryRead(t *testing.T) {
	t.Parallel()
	company := chart.Chart{Seats: []chart.Seat{
		{Handle: "designer", Kind: chart.SeatHuman},
		{Handle: "writer", Kind: chart.SeatHuman},
	}}
	held := func(context.Context) (map[string]bool, error) {
		return map[string]bool{"writer": true}, nil
	}
	listing := func(who iam.Principal, path string) *httptest.ResponseRecorder {
		svc, err := chartapi.New(chartapi.Options{
			Reader: &reader{chart: company},
			Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer {
				return &writer{}
			},
			Principal: resolved(func() iam.Principal { return who }),
			Chart:     leads(),
			Held:      held,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mux := http.NewServeMux()
		if err := svc.Routes(mux); err != nil {
			t.Fatalf("Routes: %v", err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x"+path, nil))
		return rec
	}

	rec := listing(leadOf(iam.GrantStateRead), "/chart/seats?kind=human&unheld=true")
	var refusal struct {
		Error  string   `json:"error"`
		Grants []string `json:"grants"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	if rec.Code != http.StatusForbidden || refusal.Error != string(httpjson.CodeUnauthorized) ||
		!slices.Equal(refusal.Grants, []string{string(iam.GrantPeopleManage),
			string(iam.GrantAuditRead)}) {
		t.Errorf("the board's reader asking for unheld seats answered %d %s, "+
			"want 403 naming [people:manage audit:read]", rec.Code, rec.Body)
	}
	for _, g := range []iam.Grant{iam.GrantPeopleManage, iam.GrantAuditRead} {
		if rec := listing(leadOf(iam.GrantStateRead, g),
			"/chart/seats?kind=human&unheld=true"); rec.Code != http.StatusOK ||
			strings.Contains(rec.Body.String(), `"writer"`) {
			t.Errorf("%s asking for unheld seats answered %d %s, want the "+
				"seat nobody holds", g, rec.Code, rec.Body)
		}
	}
	// THE CONTROL: the kind filter alone is the board's read.
	if rec := listing(leadOf(iam.GrantStateRead), "/chart/seats?kind=human"); rec.Code != http.StatusOK {
		t.Errorf("the board's reader filtering by kind answered %d: %s", rec.Code, rec.Body)
	}
}

// THE UNHELD FILTER ASKS A RENAMED SEAT BY ITS IDENTITY.
//
// A binding names the seat by the handle it was created under (ADR-0020), so
// the directory holds `cto` for a seat now called `chief-tech`. The report
// asked by that identity; the filter asked by the current handle, so a renamed
// seat somebody holds was listed as one nobody does — an operator sent to
// invite a person who is already signed in.
func TestTheUnheldFilterAsksARenamedSeatByItsIdentity(t *testing.T) {
	t.Parallel()
	company := chart.Chart{Seats: []chart.Seat{
		{Handle: "chief-tech", OriginHandle: "cto", Kind: chart.SeatHuman,
			FormerHandles: []string{"cto"}},
		{Handle: "designer", Kind: chart.SeatHuman},
	}}
	held := func(context.Context) (map[string]bool, error) {
		return map[string]bool{"cto": true}, nil
	}

	if got := servedSeats(t, company, held, "/chart/seats?unheld=true",
		http.StatusOK); !slices.Equal(got, []string{"designer"}) {
		t.Errorf("the unheld filter listed %v, want only designer — "+
			"chief-tech is held under its identity cto", got)
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

// EVERY READ OF THE RUNTIME HALF MASKS ITS CREDENTIALS, AND SO DOES THE ADDRESS.
//
// Holding the grant that reads the half is not holding its credentials: the
// half is served the way GET /config serves the settings document — a whole
// `${VAR}` reference shown, because it names a credential and is what an
// operator edits, and anything else masked, a composite of references
// included. The writer seals every literal before a row holds one, so the
// literals here are a row written before that rule; the point is that no read
// of this surface — the chart, one seat, the export — is where a credential
// leaves, and that what it serves instead is the mask a write restores.
func TestEveryReadOfTheRuntimeHalfMasksItsCredentials(t *testing.T) {
	t.Parallel()
	held := chart.Chart{
		Units: []chart.Unit{{Key: "engineering", Name: "Engineering",
			Runtime: json.RawMessage(`{"mcp_env":{"gitlab":{"GITLAB_TOKEN":"glpat-LITERAL"}}}`)}},
		Seats: []chart.Seat{{Handle: "sre", Name: "SRE", UnitKey: "engineering",
			Email: "sre-LITERAL@example.com",
			Runtime: json.RawMessage(`{"mcp_env":{"datadog":{` +
				`"DD_APP_KEY":"dd-LITERAL","DD_SITE":"${DD_SITE}",` +
				`"Authorization":"Bearer ${DD_TOKEN}"}},` +
				`"slack":{"bot_token":"xoxb-LITERAL"},` +
				`"mattermost":{"username":"sre-bot"}}`)}},
	}
	r := serve(t, &reader{chart: held, seat: chart.SeatDetail{Seat: held.Seats[0]}},
		leadOf(iam.GrantStateRead, iam.GrantConfigRead), leads())
	for _, path := range []string{"/chart?runtime=true", "/chart/seats/sre?runtime=true",
		"/chart/seats?runtime=true", "/chart/units?runtime=true", "/company/export"} {
		served := string(mustMarshal(t, getJSON(t, r.mux, path, http.StatusOK)))
		if strings.Contains(served, "LITERAL") {
			t.Errorf("%s served a credential: %s", path, served)
		}
		// THE CONTROL: the read still serves the half — a whole reference
		// as itself, a field that holds no credential as written, and the
		// mask where a credential was — so it is a masking, not a stripping.
		if path != "/chart/units?runtime=true" {
			for _, want := range []string{`"${DD_SITE}"`, `"sre-bot"`,
				`"DD_APP_KEY":"` + redact.FieldMask + `"`,
				`"Authorization":"` + redact.FieldMask + `"`,
				`"email":"` + redact.FieldMask + `"`} {
				if !strings.Contains(served, want) {
					t.Errorf("%s does not serve %s: %s", path, want, served)
				}
			}
		}
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
		// A CLEAR IS THE RUNTIME HALF TOO, although it carries no bytes:
		// it takes away a seat's model chain and its credentials.
		{"nor clear it", leadOf(),
			`{"name":"Engineering","clear_runtime":true}`,
			http.StatusForbidden, false},
		{"which the company's own grant may", leadOf(iam.GrantConfigWrite),
			`{"name":"Engineering","clear_runtime":true}`,
			http.StatusOK, true},
		// NULL IS NEITHER KEEP NOR CLEAR, and it is the body's shape that
		// is wrong — so it is refused as one, before a lead is told they
		// lack a grant for sending nothing.
		{"a null runtime is a shape nobody means", leadOf(),
			`{"name":"Engineering","runtime":null}`,
			http.StatusBadRequest, false},
		{"somebody who leads nothing may not edit even the prose",
			proved(iam.Principal{ID: uuid.New(), Login: "sre", Seat: "sre",
				Kind: iam.KindPerson, Stage: iam.StageActive}),
			`{"name":"Engineering"}`, http.StatusForbidden, false},
		// THE ADMIN PATH IS THE COMPANY'S GRANT: whoever may restructure
		// the chart may correct its prose leading nothing, and the
		// deployment's grant, which decides nothing about a seat's
		// prompt, may not.
		{"the company's grant edits the prose leading nothing",
			proved(iam.Principal{ID: uuid.New(), Login: "ops.admin",
				Kind: iam.KindPerson, Stage: iam.StageActive,
				Grants: []iam.Grant{iam.GrantConfigWrite}}),
			`{"name":"Engineering"}`, http.StatusOK, true},
		{"the deployment's grant alone does not",
			proved(iam.Principal{ID: uuid.New(), Login: "sre.oncall",
				Kind: iam.KindPerson, Stage: iam.StageActive,
				Grants: []iam.Grant{iam.GrantFleetOperate}}),
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

// A CHART WRITE MADE THROUGH A MACHINE TOKEN NAMES THE TOKEN.
//
// The party this surface asks for is the principal's author AND the
// credential it acted through: a lead's own token acts as the lead, so the
// author is their seat, and the provenance is `pat:<id>`. The authority took
// no provenance before, so a chart change made through somebody's token was
// recorded exactly as one they made themselves. Mutation: pass an empty
// provenance from writerFor and the party carries no operator.
func TestAChartWriteThroughATokenNamesTheToken(t *testing.T) {
	t.Parallel()
	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	who := leadOf()
	who.Via = via
	var author string
	var through chart.Provenance
	w := &writer{}
	svc, err := chartapi.New(chartapi.Options{
		Reader: &reader{},
		Authority: func(actor string, _ chart.AuthorKind, _ []iam.Grant,
			p chart.Provenance) chartapi.Writer {
			author, through = actor, p
			return w
		},
		Principal: resolved(func() iam.Principal { return who }),
		Chart:     leads(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if rec := patch(mux, "/chart/units/engineering", `{"name":"E"}`); rec.Code/100 != 2 {
		t.Fatalf("the lead's write answered %d: %s", rec.Code, rec.Body.String())
	}
	if author != "cto" || through.OperatorID != via {
		t.Errorf("the chart party is %q through %q, want the lead's seat "+
			"through %q", author, through.OperatorID, via)
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

// A REFUSAL THE DOMAIN MADE ON A GRANT IS THE TABLE'S REFUSAL.
//
// Whether a lead's body CHANGES a seat's project, its space or its email is a
// comparison against the row, which only the domain's decide can make — so the
// route admits the lead and the domain refuses the fields.
// Rendered as one of the chart's own rules (`422 refused`), that refusal told
// the caller the request would never land and never which grant would have
// admitted it; it is `403 unauthorized`, `no_grant`, the grants, and the fields
// that asked. Mutation: drop the arm and the refusal answers 422.
func TestADomainGrantRefusalIsTheTablesRefusal(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(), rel{seats: map[[2]string]bool{{"cto", "sre"}: true}})
	r.writer.err = fmt.Errorf("chart: publish: %w", &chart.GrantRefusal{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
		Class:  chart.ClassPrivileged, Grants: []iam.Grant{iam.GrantConfigWrite},
		Fields: []string{"project", "space"}, Actor: "cto",
	})
	rec := patch(r.mux, "/chart/seats/sre", `{"name":"SRE","project":"CEO","space":"CEO"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("answered %d, want 403: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error  string   `json:"error"`
		Reason string   `json:"reason"`
		Grants []string `json:"grants"`
		Fields []string `json:"fields"`
		Detail string   `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (%s)", err, rec.Body)
	}
	if body.Error != string(httpjson.CodeUnauthorized) ||
		body.Reason != string(authz.ReasonNoGrant) ||
		!slices.Equal(body.Grants, []string{string(iam.GrantConfigWrite)}) ||
		!slices.Equal(body.Fields, []string{"project", "space"}) ||
		!strings.Contains(body.Detail, "project, space") {
		t.Errorf("refusal = %+v, want unauthorized, no_grant, [config:write], "+
			"the fields and the domain's sentence", body)
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
		{"a hire", `{"operations":[{"kind":"create_seat","object":{"kind":"seat","id":"ana"},"seat_kind":"human"}]}`,
			"batch"},
		{"a departure", `{"operations":[{"kind":"remove","object":{"kind":"seat","id":"ana"}}],` +
			`"reason":"left"}`, "removal"},
		// A MIXED BATCH GOES TO THE PLACEMENT VERB and is refused
		// THERE, in the domain's own words: this surface restating that
		// rule would be a second copy of it.
		{"both at once", `{"operations":[` +
			`{"kind":"create_seat","object":{"kind":"seat","id":"ana"},"seat_kind":"human"},` +
			`{"kind":"remove","object":{"kind":"seat","id":"bo"}}]}`, "batch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// BOTH HATS, because two of these remove: which grants a
			// removal takes is the next case's question, not this one's.
			r := serve(t, nil, leadOf(iam.GrantConfigWrite, iam.GrantFleetOperate), leads())
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

// A BATCH THAT REMOVES ANYTHING TAKES THE DEPLOYMENT'S GRANT AS WELL.
//
// The pattern's verb is the company's grant, and whether a batch removes
// anything is visible only in its body — so the route asks again the moment it
// sees a `remove`, a batch that also places included, and a caller holding the
// company's grant alone is refused naming `fleet:operate` before anything is
// published. A placement on that grant alone is the control, and so is a
// removal holding both.
//
// Mutation: drop the re-ask and the config:write holder's removal reaches the
// writer.
func TestARemovalBatchTakesTheDeploymentsGrant(t *testing.T) {
	t.Parallel()
	removal := `{"operations":[{"kind":"remove","object":{"kind":"seat","id":"ana"}}],"reason":"left"}`
	mixed := `{"operations":[` +
		`{"kind":"rename","object":{"kind":"seat","id":"ana"},"to":"ana-ops"},` +
		`{"kind":"remove","object":{"kind":"seat","id":"ana-ops"}}]}`
	for _, body := range []string{removal, mixed} {
		r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
		rec := post(r.mux, "/chart/batch", body)
		var refusal struct {
			Error  string   `json:"error"`
			Reason string   `json:"reason"`
			Grants []string `json:"grants"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
		if rec.Code != http.StatusForbidden || refusal.Reason != string(authz.ReasonNoGrant) ||
			!slices.Equal(refusal.Grants, []string{string(iam.GrantFleetOperate)}) {
			t.Errorf("a removal on the company's grant alone answered %d %s, "+
				"want 403 no_grant naming [fleet:operate]", rec.Code, rec.Body)
		}
		if len(r.writer.calls) != 0 {
			t.Errorf("a refused removal reached the writer: %v", r.writer.calls)
		}
	}
	placing := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	if rec := post(placing.mux, "/chart/batch", `{"operations":[{"kind":"create_seat",`+
		`"object":{"kind":"seat","id":"ana"},"seat_kind":"human"}]}`); rec.Code != http.StatusOK {
		t.Errorf("a placement on the company's grant answered %d: %s", rec.Code, rec.Body)
	}
	both := serve(t, nil, leadOf(iam.GrantConfigWrite, iam.GrantFleetOperate), leads())
	if rec := post(both.mux, "/chart/batch", removal); rec.Code != http.StatusOK ||
		len(both.writer.calls) != 1 || both.writer.calls[0] != "removal" {
		t.Errorf("a removal holding both grants answered %d and reached %v: %s",
			rec.Code, both.writer.calls, rec.Body)
	}
}

// THE COMPANY-WIDE FEED AND THE CONTINUOUS REPORT ARE AUDIT READS; ONE
// OBJECT'S OWN HISTORY IS NOT.
//
// `GET /chart/history` is who moved whom and who dissolved which team across
// the whole company, and `GET /chart/check` names every seat nobody in the
// identity directory holds — the record of what happened and the directory's
// own question, which are `audit:read`'s. A reader holding the board's grant
// alone is refused both naming it, and still reads a seat with the history it
// carries: that is the context of the one object they asked about.
//
// Mutation: mount either route back on the chart's read and the board's reader
// is served it.
func TestTheFeedAndTheReportAreAuditReads(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/chart/history", "/chart/check"} {
		r := serve(t, &reader{chart: nimbus()}, leadOf(iam.GrantStateRead), leads())
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x"+path, nil))
		var refusal struct {
			Grants []string `json:"grants"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
		if rec.Code != http.StatusForbidden ||
			!slices.Equal(refusal.Grants, []string{string(iam.GrantAuditRead)}) {
			t.Errorf("%s for the board's reader answered %d %s, want 403 naming "+
				"[audit:read]", path, rec.Code, rec.Body)
		}
	}
	auditor := serve(t, &reader{chart: nimbus()}, leadOf(iam.GrantAuditRead), leads())
	getJSON(t, auditor.mux, "/chart/history", http.StatusOK)

	board := serve(t, &reader{chart: nimbus(), seat: chart.SeatDetail{
		Seat: nimbus().Seats[0]}}, leadOf(iam.GrantStateRead), leads())
	if body := getJSON(t, board.mux, "/chart/seats/sre", http.StatusOK); body["seat"] == nil {
		t.Errorf("one seat's read for the board's reader carries no seat: %v", body)
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

// A FLOOR ON ANOTHER LOG IS A BAD REQUEST, NOT A REFUSAL TO COME BACK FROM.
//
// Only the read can tell a `min_position` from the pages log apart from one
// from the chart's, and every node refuses it the same — so the route answers
// `400 bad_params` as it does every other parameter it cannot take. It was the
// reader's `wrong_stream` refusal and a 503, sending a client to another node.
// The control is the refusal a node on a rebuilt stream gives, which stays a
// 503.
func TestAFloorOnAnotherLogIsABadRequest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		err    error
		status int
		code   httpjson.Code
	}{
		{"a min_position on another domain's log",
			fmt.Errorf("chart: read: %w: this read floors at CREWLET_PAGES_LOG@1:5",
				statelog.ErrForeignPosition),
			http.StatusBadRequest, httpjson.CodeBadParams},
		{"a node whose stream was rebuilt under it, the control",
			&statelog.Refused{Code: statelog.RefuseWrongStream, Level: statelog.ReadStale},
			http.StatusServiceUnavailable, httpjson.CodeUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, &reader{err: c.err}, leadOf(iam.GrantStateRead), leads())
			rec := httptest.NewRecorder()
			r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/chart", nil))
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
		})
	}
}

// A FAULT'S OWN WORDS STAY IN THE LOG.
//
// `internal_error` says the reason is in this node's log, and a fault's reason
// is a store's or a driver's words — a database path here. This surface sent
// them as the answer's `detail` and logged nothing, on a read and on a write
// alike. The control is a state-log refusal, whose detail is written for the
// caller and still travels.
func TestAFaultsOwnWordsStayInTheLog(t *testing.T) {
	t.Parallel()
	fault := errors.New("open /var/lib/crewlet/replicated.db: disk I/O error")

	read := serve(t, &reader{err: fault}, leadOf(iam.GrantStateRead), leads())
	rec := httptest.NewRecorder()
	read.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/chart", nil))
	if rec.Code != http.StatusInternalServerError ||
		strings.Contains(rec.Body.String(), "/var/lib") {
		t.Errorf("a read that faulted answered %d: %s — want 500 without the "+
			"store's words", rec.Code, rec.Body)
	}

	write := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	write.writer.err = fault
	if rec := post(write.mux, "/chart/units/engineering/rename",
		`{"to":"platform"}`); rec.Code != http.StatusInternalServerError ||
		strings.Contains(rec.Body.String(), "/var/lib") {
		t.Errorf("a write that faulted answered %d: %s — want 500 without the "+
			"store's words", rec.Code, rec.Body)
	}

	refused := serve(t, &reader{err: &statelog.Refused{Code: statelog.RefuseDeferred,
		Level: statelog.ReadStale, Detail: "a record at version 9 this node cannot decode"}},
		leadOf(iam.GrantStateRead), leads())
	rec = httptest.NewRecorder()
	refused.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/chart", nil))
	if !strings.Contains(rec.Body.String(), "version 9") {
		t.Errorf("a refusal lost its own words: %s", rec.Body)
	}
}

// A RENAME IS A ONE-OPERATION STRUCTURAL BATCH, naming the object by the
// address the route is addressed by and the address the body asks for.
//
// On the tree's one subject, because that is what makes a create of the same
// address see it (internal/chart's rename tests hold that ordering).
func TestARenameIsPublishedAsAStructuralBatch(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	if rec := post(r.mux, "/chart/units/engineering/rename",
		`{"to":"platform"}`); rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body)
	}
	want := chart.Batch{Operations: []chart.Operation{{Kind: chart.OpRename,
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
		To:     "platform"}}}
	if len(r.writer.batches) != 1 || !reflect.DeepEqual(r.writer.batches[0].Operations,
		want.Operations) {
		t.Fatalf("published %+v, want %+v", r.writer.batches, want)
	}
}

// AND A BATCH CARRIES A RENAME'S NEW ADDRESS, so a rename can ride beside the
// moves it goes with. Dropped, the domain would refuse it as naming no address.
func TestABatchCarriesARenamesNewAddress(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	rec := post(r.mux, "/chart/batch", `{"operations":[
		{"kind":"rename","object":{"kind":"unit","id":"engineering"},"to":"platform"},
		{"kind":"move","object":{"kind":"seat","id":"cto"},"parent":"platform"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body)
	}
	if len(r.writer.batches) != 1 || len(r.writer.batches[0].Operations) != 2 ||
		r.writer.batches[0].Operations[0].To != "platform" {
		t.Fatalf("published %+v, want the rename's new address carried", r.writer.batches)
	}
}

// A SEAT'S KIND IS STRUCTURE ON THIS SURFACE TOO: a batch and an import carry
// it, and a seat's content refuses it.
//
// Carried, because a create_seat and an import edge each state what holds a
// seat and the domain refuses one that does not; refused on the content write,
// because the domain no longer reads it there and a dropped field would answer
// 200 for a kind change that never happened.
func TestASeatsKindTravelsAsStructure(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	rec := post(r.mux, "/chart/batch", `{"operations":[
		{"kind":"set_kind","object":{"kind":"seat","id":"cto"},"seat_kind":"human"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a set_kind answered %d: %s", rec.Code, rec.Body)
	}
	if len(r.writer.batches) != 1 ||
		r.writer.batches[0].Operations[0].SeatKind != chart.SeatHuman {
		t.Errorf("published %+v, want the seat kind carried", r.writer.batches)
	}

	rec = post(r.mux, "/chart/import", `{"revision":"r1","edges":[
		{"object":{"kind":"seat","id":"cto"},"seat_kind":"human"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("an import answered %d: %s", rec.Code, rec.Body)
	}
	if len(r.writer.imports) != 1 || r.writer.imports[0][0].Kind != chart.SeatHuman {
		t.Errorf("imported %+v, want the seat kind carried", r.writer.imports)
	}

	lead := serve(t, nil, leadOf(iam.GrantConfigWrite),
		rel{seats: map[[2]string]bool{{"cto", "ana"}: true}})
	if rec := patch(lead.mux, "/chart/seats/ana", `{"name":"Ana","kind":"agent"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a seat's content naming a kind answered %d, want 400 — the "+
			"domain does not read it there: %s", rec.Code, rec.Body)
	}
	// THE CONTROL: the same body without the kind is written.
	if rec := patch(lead.mux, "/chart/seats/ana", `{"name":"Ana"}`); rec.Code != http.StatusOK {
		t.Errorf("a seat's content answered %d: %s", rec.Code, rec.Body)
	}
}

// A SEAT'S `manages` IS STRUCTURE ON THIS SURFACE TOO: a batch's set_manages
// and an import's edge carry the list, and a seat's content refuses it.
//
// Carried, because each states a seat's whole list and a dropped one would
// publish a seat that manages nobody; refused on the content write, because
// the domain no longer reads it there — a lead's goal edit that sent the list
// back as they had read it wrote a renamed entry back over the rename — and a
// dropped field would answer 200 for a list that never changed.
func TestASeatsManagesTravelsAsStructure(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	rec := post(r.mux, "/chart/batch", `{"operations":[
		{"kind":"set_manages","object":{"kind":"seat","id":"cto"},
		 "manages":["platform","sre"]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a set_manages answered %d: %s", rec.Code, rec.Body)
	}
	if len(r.writer.batches) != 1 || !slices.Equal(
		r.writer.batches[0].Operations[0].Manages, []string{"platform", "sre"}) {
		t.Errorf("published %+v, want the list carried", r.writer.batches)
	}

	rec = post(r.mux, "/chart/import", `{"revision":"r1","edges":[
		{"object":{"kind":"seat","id":"cto"},"seat_kind":"human","manages":["sre"]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("an import answered %d: %s", rec.Code, rec.Body)
	}
	if len(r.writer.imports) != 1 ||
		!slices.Equal(r.writer.imports[0][0].Manages, []string{"sre"}) {
		t.Errorf("imported %+v, want the list carried", r.writer.imports)
	}

	lead := serve(t, nil, leadOf(iam.GrantConfigWrite),
		rel{seats: map[[2]string]bool{{"cto", "ana"}: true}})
	if rec := patch(lead.mux, "/chart/seats/ana", `{"name":"Ana","manages":["cto"]}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "manages") {
		t.Errorf("a seat's content naming manages answered %d, want 400 naming "+
			"the field — the domain does not read it there: %s", rec.Code, rec.Body)
	}
	if len(lead.writer.calls) != 0 {
		t.Errorf("a refused body reached the writer: %v", lead.writer.calls)
	}
}

// AND IT ANSWERS THE DOMAIN'S REFUSAL, NOT 200 — and as the domain's refusal,
// `422 refused`, not as a malformed body.
//
// A rename used to be decided on the new address's own subject, so one that
// lost to a create of the same address was accepted and then dropped at the
// apply — and this route answered 200 for a rename that never happened. On the
// tree's subject the decide refuses it, and the refusal is what comes back.
// It came back `400 invalid_body`, which told the caller to reshape a body
// that was never wrong: the address was taken, and no body would change that.
func TestARenameAnswersTheRefusalRatherThan200(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	r.writer.err = &chart.RefusalError{Index: 0, Rule: chart.RuleKeyTaken,
		Operation: chart.Operation{Kind: chart.OpRename,
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
			To:     "platform"},
		Detail: `unit "platform" is already in the chart`}

	rec := post(r.mux, "/chart/units/engineering/rename", `{"to":"platform"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a refused rename answered %d, want 422: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error != string(httpjson.CodeRefused) {
		t.Errorf("error = %q, want %q", body.Error, httpjson.CodeRefused)
	}
	if !strings.Contains(rec.Body.String(), chart.RuleKeyTaken) {
		t.Errorf("the answer does not carry the rule it broke: %s", rec.Body)
	}
}

// A FIELD A ROUTE DOES NOT READ IS REFUSED, NAMING IT, AND NOTHING IS WRITTEN.
//
// Every write here is full post-state or a structural gesture, so a dropped
// field answers 200 for a request that asked for more than landed — a
// misspelled `purpse` left the purpose empty with nothing to say so.
func TestAFieldARouteDoesNotReadIsRefused(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(iam.GrantConfigWrite), leads())
	for _, c := range []struct{ name, method, path, body string }{
		{"a unit's content", http.MethodPatch, "/chart/units/engineering",
			`{"name":"E","purpse":"x"}`},
		{"a batch", http.MethodPost, "/chart/batch",
			`{"operations":[{"kind":"move","object":{"kind":"seat","id":"cto"},"parnet":"eng"}]}`},
		{"a rename", http.MethodPost, "/chart/units/engineering/rename",
			`{"to":"platform","from":"engineering"}`},
		// AND ANYTHING AFTER THE ONE VALUE, which json.Unmarshal refused
		// and a decoder asked only whether More values follow does not:
		// it answers no before a `}` or a `]`.
		{"a body with a trailing brace", http.MethodPost,
			"/chart/units/engineering/rename", `{"to":"platform"}}`},
		{"a body with a trailing bracket", http.MethodPost, "/chart/batch",
			`{"operations":[{"kind":"move","object":{"kind":"seat","id":"cto"},"parent":"eng"}]}]`},
		{"a body with a second value", http.MethodPatch,
			"/chart/units/engineering", `{"name":"E"} {"name":"F"}`},
	} {
		var rec *httptest.ResponseRecorder
		if c.method == http.MethodPatch {
			rec = patch(r.mux, c.path, c.body)
		} else {
			rec = post(r.mux, c.path, c.body)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400 — a body carrying what "+
				"the route does not read is refused: %s", c.name, rec.Code, rec.Body)
		}
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("a refused body reached the writer: %v", r.writer.calls)
	}
	// THE CONTROL: the same unit body without the stray field is written,
	// and whitespace after the value is not a second one.
	if rec := patch(r.mux, "/chart/units/engineering", "{\"name\":\"E\"}\n \t"); rec.Code != http.StatusOK {
		t.Errorf("a well-formed unit body answered %d: %s", rec.Code, rec.Body)
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
		Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer { return &writer{} },
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
		Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer { return &writer{} },
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
				Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer { return w },
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
