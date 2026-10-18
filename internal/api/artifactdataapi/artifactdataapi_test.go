package artifactdataapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// sheet is an artifact jane owns, shared with bob as contributor and with sam
// as viewer; nobody else has a relation to it.
var (
	sheet   = artifacts.IDFor("the review sheet")
	trashed = artifacts.IDFor("a trashed sheet")
)

type heads struct{}

func (heads) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	shares := []artifacts.Share{
		{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleContributor},
		{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "sam"}, Role: artifacts.RoleViewer},
	}
	switch id {
	case sheet:
		return artifactsdomain.Head{ID: id, Owner: "jane", OwnerKind: iam.ActorHuman, Shares: shares,
			Status: artifactsdomain.StatusLive}, statelog.Position{}, nil
	case trashed:
		return artifactsdomain.Head{ID: id, Owner: "jane", OwnerKind: iam.ActorHuman, Shares: shares,
			Status: artifactsdomain.StatusTrashed}, statelog.Position{}, nil
	}
	return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
}

func (heads) Version(_ context.Context, _ artifacts.ID, number int) (artifactsdomain.Version, error) {
	caps := map[int][]artifacts.Capability{
		1: nil,
		2: {artifacts.CapabilityDB},
		3: {artifacts.CapabilityState},
	}[number]
	return artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Number: number,
		Capabilities: caps}}, nil
}

// store records every write's call and answers what a case set.
type store struct {
	mu     sync.Mutex
	calls  []artifactdata.Call
	writes []artifacts.DataWrite
	answer artifactdata.WriteResult
	err    error
}

func (s *store) Write(_ context.Context, c artifactdata.Call, _ artifacts.ID, in artifacts.DataWrite) (
	artifactdata.WriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	s.writes = append(s.writes, in)
	return s.answer, s.err
}

func (s *store) Undo(_ context.Context, c artifactdata.Call, _ artifacts.ID, _ int64) (artifactdata.WriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	return s.answer, s.err
}

func (s *store) Configure(_ context.Context, c artifactdata.Call, _ artifacts.ID, _ []artifacts.Collection) (
	artifactdata.WriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	return s.answer, s.err
}

func (s *store) Schema(context.Context, artifacts.ID) ([]artifacts.Collection, error) {
	return []artifacts.Collection{{Name: "review", Read: artifacts.ReadAll, Columns: []artifacts.Column{
		{Key: "title", Type: artifacts.ColumnText}}}}, nil
}

func (s *store) Docs(_ context.Context, _ artifacts.ID, _ artifactdata.Reading, collection string, _ int64,
	_ string, _ int) (artifactdata.DocPage, error) {
	return artifactdata.DocPage{Docs: []artifactdata.Doc{{Collection: collection, ID: "r1",
		Fields: json.RawMessage(`{"title":"=1+1"}`), CreatedBy: iam.Actor{Name: "jane"}}}}, nil
}

func (s *store) last() artifactdata.Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

// rig is the surface over the fakes, mounted.
func rig(t *testing.T) (*http.ServeMux, *store) {
	t.Helper()
	data := &store{answer: artifactdata.WriteResult{Verdict: artifacts.DataApplied, Seq: 9,
		Result: statelog.Result{Outcome: statelog.OutcomeApplied}}}
	svc, err := artifactdataapi.New(artifactdataapi.Options{
		Gate: artifactsapi.Gate{Heads: heads{}, Chart: authz.NoChart{},
			Limits: func() artifacts.Limits {
				return artifacts.Limits{DataBytes: 1 << 30, DataStoreBytes: 32 << 20, CompanyBytes: 1, Artifacts: 1}
			}},
		Data: data, Versions: heads{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	return mux, data
}

// person is a signed-in person bound to seat, holding the artifacts app.
func person(seat string) iam.Principal {
	return iam.Principal{ID: uuid.NewSHA1(uuid.Nil, []byte(seat)), Login: seat + ".person",
		Kind: iam.KindPerson, Seat: seat, Grants: []iam.Grant{iam.GrantArtifacts},
		Stage: iam.StageActive, Via: iam.SessionName("lineage-" + seat),
		ReauthAt: time.Now().Add(time.Hour)}
}

// do sends one request as p and answers the recorder.
func do(t *testing.T, mux *http.ServeMux, p iam.Principal, method, path, body string, keyed bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if keyed {
		r.Header.Set("Idempotency-Key", statelog.NewOpID(time.Now(), ""))
	}
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(iam.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// errorOf is a refusal's code.
func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("a refusal that is not JSON: %s", w.Body.String())
	}
	code, _ := body["error"].(string)
	return code
}

const tick = `{"ops":[{"op":"merge","collection":"review","doc":"r1","fields":{"done":true}}]}`

// A CALLER WITH NO RELATION IS ANSWERED EXACTLY AS AN UNKNOWN ARTIFACT: the
// same status and the same code, so a write route tells nobody an artifact
// they were never shared exists.
func TestACallerWithNoRelationIsAnsweredAsAnUnknownArtifact(t *testing.T) {
	t.Parallel()
	mux, data := rig(t)
	stranger := do(t, mux, person("dana"), "POST", "/artifacts/"+sheet.String()+"/data/writes", tick, true)
	unknown := do(t, mux, person("dana"), "POST", "/artifacts/"+artifacts.IDFor("nothing").String()+
		"/data/writes", tick, true)
	if stranger.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound ||
		errorOf(t, stranger) != errorOf(t, unknown) {
		t.Fatalf("a stranger is answered %d %s and an unknown artifact %d %s", stranger.Code,
			errorOf(t, stranger), unknown.Code, errorOf(t, unknown))
	}
	if len(data.calls) != 0 {
		t.Fatal("a refused write reached the datastore")
	}
}

// THE WRITE IS STAMPED WITH WHAT THE TABLE DECIDED: the caller's effective role
// from the artifact's shares, their seat as the actor, the surface they came
// through and the plan quotas in force, and nothing the body says changes the
// role.
func TestAWriteIsStampedWithTheDecidedRole(t *testing.T) {
	t.Parallel()
	mux, data := rig(t)
	w := do(t, mux, person("bob"), "POST", "/artifacts/"+sheet.String()+"/data/writes", tick, true)
	if w.Code != http.StatusOK {
		t.Fatalf("bob's write: %d %s", w.Code, w.Body.String())
	}
	got := data.last().Authority
	if got.Role != artifacts.RoleContributor || got.Actor.Name != "bob" || got.Actor.Kind != iam.ActorHuman ||
		got.Origin != artifacts.OriginDashboard || got.Limits.DataStoreBytes != 32<<20 {
		t.Fatalf("bob's write is stamped %+v", got)
	}
	if data.last().OpKey == "" {
		t.Fatal("the write carries no operation key")
	}
	// A BEARER CREDENTIAL is the API, whatever its body says.
	bearer := person("bob")
	bearer.Via = ""
	do(t, mux, bearer, "POST", "/artifacts/"+sheet.String()+"/data/writes",
		`{"ops":[{"op":"merge","collection":"review","doc":"r1","fields":{"done":true}}],"via":"bridge"}`, true)
	if got := data.last().Authority.Origin; got != artifacts.OriginAPI {
		t.Fatalf("a bearer's write is stamped origin %s", got)
	}
}

// A PAGE SCRIPT'S WRITE IS ADMITTED ONLY AS ITS VERSION DECLARES: a version
// without `db` writes no data, and the per-viewer state needs `state`.
func TestAPageScriptsWriteNeedsItsVersionsCapability(t *testing.T) {
	t.Parallel()
	mux, data := rig(t)
	bridge := func(version int) string {
		return `{"ops":[{"op":"merge","collection":"review","doc":"r1","fields":{"done":true}}],` +
			`"via":"bridge","version":` + string(rune('0'+version)) + `}`
	}
	if w := do(t, mux, person("bob"), "POST", "/artifacts/"+sheet.String()+"/data/writes", bridge(1), true); w.Code != http.StatusForbidden {
		t.Fatalf("a page whose version declares no db: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, mux, person("bob"), "POST", "/artifacts/"+sheet.String()+"/data/writes", bridge(2), true); w.Code != http.StatusOK {
		t.Fatalf("a page whose version declares db: %d %s", w.Code, w.Body.String())
	}
	if got := data.last().Authority.Origin; got != artifacts.OriginBridge {
		t.Fatalf("a page script's write is stamped origin %s", got)
	}
	state := `{"key":"theme","value":"dark","via":"bridge","version":3}`
	if w := do(t, mux, person("sam"), "POST", "/artifacts/"+sheet.String()+"/data/state", state, true); w.Code != http.StatusOK {
		t.Fatalf("a viewer's state write: %d %s", w.Code, w.Body.String())
	}
	op := data.writes[len(data.writes)-1].Ops[0]
	want, _ := artifactdata.StateDocID("sam", "theme")
	if op.Collection != artifactdata.StateCollection || op.Doc != want || string(op.Fields[artifactdata.StateField]) != `"dark"` {
		t.Fatalf("the state write is %+v", op)
	}
}

// A WRITE NAMES ITS OPERATION, AND ITS VERDICT IS ANSWERED IN THE ENGINE'S
// VOCABULARY: no key is a 400, a moved guard a 409 carrying the documents, a
// spent budget a 429 with Retry-After, and a trashed artifact a 410.
func TestAWritesOutcomeIsAnsweredInTheEnginesVocabulary(t *testing.T) {
	t.Parallel()
	mux, data := rig(t)
	path := "/artifacts/" + sheet.String() + "/data/writes"
	if w := do(t, mux, person("bob"), "POST", path, tick, false); w.Code != http.StatusBadRequest {
		t.Fatalf("a write naming no key: %d", w.Code)
	}
	data.answer = artifactdata.WriteResult{Verdict: artifacts.DataConflict, Result: statelog.Result{
		Outcome: statelog.OutcomeApplied}, Conflict: []artifactdata.Doc{{Collection: "review", ID: "r1", Version: 4}}}
	w := do(t, mux, person("bob"), "POST", path, tick, true)
	if w.Code != http.StatusConflict || errorOf(t, w) != "stale_version" || !strings.Contains(w.Body.String(), `"version":4`) {
		t.Fatalf("a conflict: %d %s", w.Code, w.Body.String())
	}
	data.err = &artifactdata.RateLimited{Scope: "principal", Cost: 3, RetryAfter: 2500 * time.Millisecond}
	w = do(t, mux, person("bob"), "POST", path, tick, true)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3" || errorOf(t, w) != "write_budget" {
		t.Fatalf("a spent budget: %d %s retry %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	data.err = nil
	w = do(t, mux, person("bob"), "POST", "/artifacts/"+trashed.String()+"/data/writes", tick, true)
	if w.Code != http.StatusGone || errorOf(t, w) != "data_gone" {
		t.Fatalf("a trashed artifact: %d %s", w.Code, w.Body.String())
	}
}

// THE SCHEMA IS AN EDITOR'S AND THE EXPORT TOO: a contributor is refused both,
// naming the role; the owner's export is a CSV attachment.
func TestTheSchemaAndTheExportNeedAnEditor(t *testing.T) {
	t.Parallel()
	mux, _ := rig(t)
	schema := `{"collections":[{"name":"review","preset":"shared"}]}`
	if w := do(t, mux, person("bob"), "PUT", "/artifacts/"+sheet.String()+"/data/schema", schema, true); w.Code != http.StatusForbidden {
		t.Fatalf("a contributor's schema change: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, mux, person("jane"), "PUT", "/artifacts/"+sheet.String()+"/data/schema", schema, true); w.Code != http.StatusOK {
		t.Fatalf("the owner's schema change: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, mux, person("bob"), "GET", "/artifacts/"+sheet.String()+"/data/export.csv?collection=review", "", false); w.Code != http.StatusForbidden {
		t.Fatalf("a contributor's export: %d", w.Code)
	}
	w := do(t, mux, person("jane"), "GET", "/artifacts/"+sheet.String()+"/data/export.csv?collection=review", "", false)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("the owner's export: %d %v", w.Code, w.Header())
	}
	if !strings.Contains(w.Body.String(), "'=1+1") {
		t.Fatalf("the export runs a formula cell: %s", w.Body.String())
	}
}

// A BODY FIELD THE ROUTE DOES NOT TAKE IS REFUSED, never dropped: a typo would
// read as a request for less.
func TestAnUnknownBodyFieldIsRefused(t *testing.T) {
	t.Parallel()
	mux, data := rig(t)
	w := do(t, mux, person("bob"), "POST", "/artifacts/"+sheet.String()+"/data/writes",
		`{"ops":[],"role":"owner"}`, true)
	if w.Code != http.StatusBadRequest || len(data.calls) != 0 {
		t.Fatalf("a body naming a role: %d %s", w.Code, w.Body.String())
	}
}
