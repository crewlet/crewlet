package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// dataSheet is the one artifact the datastore fakes hold, owned by a seat the
// fixture's credential has no relation to: it reads and writes through the
// admin path.
var dataSheet = artifacts.IDFor("the api's review sheet")

type dataHeads struct{}

func (dataHeads) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if id != dataSheet {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Head{ID: id, Owner: "jane", OwnerKind: iam.ActorHuman,
		Status: artifactsdomain.StatusLive}, statelog.Position{}, nil
}

func (dataHeads) Version(_ context.Context, _ artifacts.ID, n int) (artifactsdomain.Version, error) {
	return artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Number: n}}, nil
}

// dataStore answers every datastore read and write with a value naming what
// it was asked.
type dataStore struct{}

func (dataStore) Docs(_ context.Context, _ artifacts.ID, rd artifactdata.Reading, collection string,
	_ int64, _ string, _ int) (artifactdata.DocPage, error) {
	return artifactdata.DocPage{Docs: []artifactdata.Doc{{Collection: collection, ID: "r1",
		Fields: json.RawMessage(`{"title":"one"}`), CreatedBy: iam.Actor{Name: rd.Record}}}}, nil
}

func (dataStore) Doc(_ context.Context, _ artifacts.ID, _ artifactdata.Reading, collection, doc string) (
	artifactdata.Doc, error) {
	return artifactdata.Doc{Collection: collection, ID: doc}, nil
}

func (dataStore) Query(_ context.Context, _ artifacts.ID, _ artifactdata.Reading, collection string,
	q query.Query) (artifactdata.DocPage, error) {
	return artifactdata.DocPage{Docs: []artifactdata.Doc{{Collection: collection, ID: q.After}}}, nil
}

func (dataStore) History(context.Context, artifacts.ID, artifactdata.Reading, artifactdata.HistoryFilter,
	string, int) (artifactdata.HistoryPage, error) {
	return artifactdata.HistoryPage{Entries: []artifactdata.HistoryEntry{}}, nil
}

func (dataStore) Schema(context.Context, artifacts.ID) ([]artifacts.Collection, error) {
	return []artifacts.Collection{{Name: "review", Read: artifacts.ReadAll}}, nil
}

func (dataStore) Store(_ context.Context, id artifacts.ID) (artifactdata.StoreInfo, error) {
	return artifactdata.StoreInfo{Artifact: id}, nil
}

func (dataStore) Usage(context.Context) (artifactdata.Usage, error) { return artifactdata.Usage{}, nil }

func (dataStore) State(context.Context, artifacts.ID, artifactdata.Reading) ([]artifactdata.StateEntry, error) {
	return []artifactdata.StateEntry{}, nil
}

func (dataStore) Write(context.Context, artifactdata.Call, artifacts.ID, artifacts.DataWrite) (
	artifactdata.WriteResult, error) {
	return artifactdata.WriteResult{Verdict: artifacts.DataApplied, Result: statelog.Result{
		Outcome: statelog.OutcomeApplied}}, nil
}

func (dataStore) Undo(context.Context, artifactdata.Call, artifacts.ID, int64) (artifactdata.WriteResult, error) {
	return artifactdata.WriteResult{}, artifactdata.ErrNotUndoable
}

func (dataStore) Configure(context.Context, artifactdata.Call, artifacts.ID, []artifacts.Collection) (
	artifactdata.WriteResult, error) {
	return artifactdata.WriteResult{}, artifactdata.ErrForbidden
}

// THE DATASTORE'S ROUTES AND ITS READ QUESTIONS SHARE ONE MUX: each literal
// segment (`schema`, `history`, `state`, `export.csv`, `query`) is served by its
// own route rather than read as a collection or a document of that name, and
// every route answers through the API's own guard.
func TestTheDatastoresRoutesAndQuestionsShareOneMux(t *testing.T) {
	t.Parallel()
	gate := artifactsapi.Gate{Heads: dataHeads{}, Chart: authz.NoChart{}, Marks: apiArtifacts{},
		Limits: func() artifacts.Limits { return artifacts.Limits{DataBytes: 1, DataStoreBytes: 1} }}
	surface, err := artifactdataapi.New(artifactdataapi.Options{Gate: gate, Data: dataStore{}, Versions: dataHeads{}})
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(t, api.Options{
		Runtime:      &fakeRuntime{},
		ArtifactData: surface,
		Sources:      queries.Sources{ArtifactData: &queries.ArtifactDataSource{Gate: gate, Reads: dataStore{}}},
	})
	base := "/artifacts/" + dataSheet.String() + "/data"
	for _, tc := range []struct {
		method, path, body, want string
		status                   int
	}{
		{"GET", base + "/review", "", `"r1"`, http.StatusOK},
		{"GET", base + "/review/r9", "", `"r9"`, http.StatusOK},
		{"GET", base + "/review/query?q=" + `{"after":"x1"}`, "", `"x1"`, http.StatusOK},
		{"GET", base + "/schema", "", `"review"`, http.StatusOK},
		{"GET", base + "/history", "", `"entries"`, http.StatusOK},
		{"GET", base + "/state", "", `"state"`, http.StatusOK},
		{"GET", base + "/export.csv?collection=review", "", "r1", http.StatusOK},
		{"POST", base + "/writes", `{"ops":[{"op":"set","collection":"review","doc":"a","fields":{"t":1}}]}`,
			`"applied"`, http.StatusOK},
		{"POST", base + "/undo", `{"seq":4}`, "not_undoable", http.StatusConflict},
		{"PUT", base + "/schema", `{"collections":[]}`, "forbidden", http.StatusForbidden},
		{"GET", "/artifacts/" + artifacts.IDFor("nothing").String() + "/data/review", "", "not_found", http.StatusNotFound},
	} {
		r := httptest.NewRequest(tc.method, strings.ReplaceAll(tc.path, `"`, "%22"), strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.method != "GET" {
			r.Header.Set("Idempotency-Key", statelog.NewOpID(time.Now(), ""))
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, authed(r))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s %s = %d %s, want %d holding %s", tc.method, tc.path, w.Code, w.Body.String(),
				tc.status, tc.want)
		}
	}
}
