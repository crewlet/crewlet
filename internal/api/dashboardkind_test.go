package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE EXIT TESTS OF THE DASHBOARD KIND (adr/0043), through the whole API: the
// guard resolving each caller's own credential, the artifacts gate admitting
// the board on its share, the question registry deciding each engine dataset
// as that caller, and the datastore answering a store dataset fresh.

// board is one dashboard every fixture credential may open: the company holds
// it as viewers, and its author is somebody none of them are.
var (
	board     = artifacts.IDFor("the delivery board")
	boardSpec = []byte(`{
  "v": 1, "title": "Delivery",
  "datasets": [
    {"id": "work", "title": "My open work", "source": "engine",
     "engine": {"name": "tracker.items", "params": {"scope": "mine"}}},
    {"id": "deals", "title": "Pipeline", "source": "store", "store": {"collection": "deals"},
     "columns": [{"key": "amount", "type": "number"}]},
    {"id": "total", "title": "Pipeline total", "source": "calc",
     "calc": {"from": "deals", "steps": [{"op": "group", "aggregates": [{"column": "amount", "agg": "sum", "of": "amount"}]}]}},
    {"id": "both", "title": "Work beside the pipeline", "source": "calc",
     "calc": {"from": "work", "steps": [{"op": "group", "aggregates": [{"column": "n", "agg": "count"}]}]}}
  ],
  "widgets": [
    {"id": "mine", "type": "table", "title": "My work", "dataset": "work", "columns": ["key", "title"], "w": 12},
    {"id": "pipeline", "type": "stat", "title": "Pipeline", "dataset": "total", "value": "amount", "w": 4}
  ]
}`)
	boardEntry = artifacts.Entry{Path: "dashboard.json", ContentType: "application/json",
		Object: objstore.Object{Key: objstore.KeyAt(time.Unix(1_700_000_000, 0)),
			Hash: objstore.HashOf(boardSpec), Size: int64(len(boardSpec))}}
)

// boardArtifacts is the estate holding the board, embedding the page fakes'
// writes, marks and roster.
type boardArtifacts struct{ apiArtifacts }

func (boardArtifacts) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if id != board {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Head{ID: id, Kind: artifacts.KindDashboard, Title: "Delivery", Owner: "author",
		OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
		Shares: []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
			Role: artifacts.RoleViewer}}}, statelog.Position{}, nil
}

func (boardArtifacts) Version(_ context.Context, id artifacts.ID, n int) (artifactsdomain.Version, error) {
	if n != 1 {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: 1,
		Entry: boardEntry.Path}, Manifest: artifacts.Manifest{Entry: boardEntry.Path,
		Files: []artifacts.Entry{boardEntry}}}, nil
}

func (boardArtifacts) Open(_ context.Context, o objstore.Object) (io.ReadCloser, error) {
	if o.Hash != boardEntry.Object.Hash {
		return nil, objstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(boardSpec)), nil
}

// companyWork is the tracker: two people's tasks, answered for whoever the
// listing names as assignee, and counted per read.
type companyWork struct {
	stubWorkReader
	mu    *sync.Mutex
	reads *int
}

var companyTasks = []worklet.TaskRow{
	{Key: "ENG-1", Title: "Alice's migration", Assignee: "token:alice", Status: "in_progress"},
	{Key: "ENG-2", Title: "Alice's review", Assignee: "token:alice", Status: "todo"},
	{Key: "OPS-7", Title: "Bob's rotation", Assignee: "token:bob", Status: "todo"},
}

func (companyWork) ExpandedQuery(_ context.Context, params map[string]any, _ worklet.Viewer, _ time.Time,
	_ *time.Location) (worklet.Query, error) {
	var q worklet.Query
	if who, ok := params["assignee"]; ok {
		q.Assignee = []string{fmt.Sprint(who)}
	}
	return q, nil
}

func (w companyWork) Tasks(_ context.Context, q worklet.Query, _ time.Time) (worklet.Answer, error) {
	w.mu.Lock()
	*w.reads++
	w.mu.Unlock()
	var rows []worklet.TaskRow
	for _, row := range companyTasks {
		if len(q.Assignee) == 0 || slices.Contains(q.Assignee, row.Assignee) {
			rows = append(rows, row)
		}
	}
	return worklet.Answer{Rows: rows, TotalHint: len(rows), Complete: true}, nil
}

// boardData is the board's datastore: a pipeline collection an agent writes.
type boardData struct {
	mu    *sync.Mutex
	deals *[]float64
}

func (d boardData) Docs(_ context.Context, id artifacts.ID, _ artifactdata.Reading, collection string,
	_ int64, _ string, _ int) (artifactdata.DocPage, error) {
	if id != board || collection != "deals" {
		return artifactdata.DocPage{}, artifactdata.ErrNotFound
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	page := artifactdata.DocPage{Docs: []artifactdata.Doc{}}
	for i, amount := range *d.deals {
		page.Docs = append(page.Docs, artifactdata.Doc{Collection: collection, ID: fmt.Sprintf("d%d", i),
			Fields: json.RawMessage(fmt.Sprintf(`{"amount":%v}`, amount))})
	}
	return page, nil
}

// boardRig is the API serving the board to three people: alice and bob hold
// the tracker, carol does not, and all three hold the artifacts app.
type boardRig struct {
	app   *api.App
	work  companyWork
	data  boardData
	reads func() int
}

func newBoardRig(t *testing.T) *boardRig {
	t.Helper()
	estate := boardArtifacts{}
	gate := artifactsapi.Gate{Heads: estate, Chart: authz.NoChart{}, Marks: estate, Roster: estate,
		Limits: func() artifacts.Limits { return artifacts.Limits{CompanyBytes: 1, Artifacts: 1} }}
	ready, err := objectives.NewBeacon(objectives.NewMeter(nil, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	surface, err := artifactsapi.New(artifactsapi.Options{Gate: gate, Reads: estate, Writes: estate,
		Objects: estate, Ready: ready, Uploads: func() (artifactsapi.Pipeline, error) {
			return nil, artifactupload.ErrInvalid
		},
		WorkItems: func() (artifactsapi.WorkItems, error) { return nil, artifactsapi.ErrWorkletNotUp }})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	mu := &sync.Mutex{}
	work := companyWork{mu: mu, reads: &reads}
	deals := []float64{100, 250}
	data := boardData{mu: &sync.Mutex{}, deals: &deals}
	b := config.DefaultBootstrap()
	worker := []iam.Grant{iam.GrantWorklet, iam.GrantArtifacts}
	authorize(&b,
		config.APIToken{ID: "alice", Token: "alice-token-long-enough-to-pass", Grants: worker},
		config.APIToken{ID: "bob", Token: "bob-token-long-enough-to-pass", Grants: worker},
		config.APIToken{ID: "carol", Token: "carol-token-long-enough-to-pass", Grants: []iam.Grant{iam.GrantArtifacts}},
	)
	app := newApp(t, api.Options{
		Bootstrap: &b,
		Artifacts: surface,
		Datasets:  &api.DatasetsSource{Boards: surface, Data: data},
		Sources:   queries.Sources{Work: work},
	})
	return &boardRig{app: app, work: work, data: data, reads: func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}}
}

// dataset reads one dataset of the board as who.
func (r *boardRig) dataset(t *testing.T, who, id string) (int, string, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/artifacts/"+board.String()+"/versions/1/sources/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+who+"-token-long-enough-to-pass")
	rec := httptest.NewRecorder()
	r.app.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, rec.Body.String(), body
}

// keys are the `key` column of a ready answer's rows.
func keys(t *testing.T, body map[string]any) []string {
	t.Helper()
	table, ok := body["table"].(map[string]any)
	if !ok {
		t.Fatalf("the answer holds no table: %v", body)
	}
	var out []string
	for _, row := range table["rows"].([]any) {
		out = append(out, fmt.Sprint(row.(map[string]any)["key"]))
	}
	return out
}

// TWO VIEWERS WITH DIFFERENT TRACKER ACCESS OPEN ONE DASHBOARD AND EACH SEES
// ONLY THEIR OWN ROWS: the board is one spec, `scope: mine` is resolved for
// whoever reads it, a cached answer never reaches a second principal, and a
// viewer without Worklet is told the source is closed, with no row of
// anybody's.
func TestTwoViewersOfOneBoardEachSeeOnlyTheirOwnRows(t *testing.T) {
	t.Parallel()
	rig := newBoardRig(t)

	code, raw, alice := rig.dataset(t, "alice", "work")
	if code != http.StatusOK || alice["status"] != "ready" {
		t.Fatalf("alice reads her work: %d %s", code, raw)
	}
	if got := keys(t, alice); !slices.Equal(got, []string{"ENG-1", "ENG-2"}) {
		t.Errorf("alice sees %v, want her own two tasks", got)
	}

	code, raw, bob := rig.dataset(t, "bob", "work")
	if code != http.StatusOK || bob["status"] != "ready" {
		t.Fatalf("bob reads his work: %d %s", code, raw)
	}
	if got := keys(t, bob); !slices.Equal(got, []string{"OPS-7"}) {
		t.Errorf("bob sees %v, want his own task and none of alice's (a cached answer reached him?)", got)
	}

	code, raw, carol := rig.dataset(t, "carol", "work")
	if code != http.StatusOK || carol["status"] != "refused" {
		t.Fatalf("carol, without Worklet, is answered a refusal of the dataset: %d %s", code, raw)
	}
	refusal, _ := carol["refusal"].(map[string]any)
	if refusal["dataset"] != "work" || !strings.Contains(fmt.Sprint(refusal["grants"]), "worklet") {
		t.Errorf("the refusal names the dataset and the grant that would open it: %v", refusal)
	}
	for _, task := range companyTasks {
		if strings.Contains(raw, task.Key) || strings.Contains(raw, task.Title) {
			t.Errorf("carol's answer carries %s, a row she could not read directly: %s", task.Key, raw)
		}
	}

	// A CALCULATION OVER A CLOSED SOURCE IS CLOSED, never a partial answer.
	if _, raw, both := rig.dataset(t, "carol", "both"); both["status"] != "refused" ||
		strings.Contains(raw, `"rows"`) {
		t.Errorf("carol's calculation over the tracker is refused with no rows: %s", raw)
	}

	// AND THE CACHE IS PER PRINCIPAL: alice again is answered from it, and
	// the tracker was asked once for each of the two who may read it.
	before := rig.reads()
	if _, _, again := rig.dataset(t, "alice", "work"); !slices.Equal(keys(t, again), []string{"ENG-1", "ENG-2"}) {
		t.Errorf("alice's second read is still hers: %v", again)
	}
	if rig.reads() != before {
		t.Errorf("alice's repeat within the cache's life asked the tracker again")
	}
	if before != 2 {
		t.Errorf("the tracker answered %d reads for two readers who may read it, want 2 (carol's was "+
			"refused before it was asked)", before)
	}
}

// AN AGENT'S STORE DATASET UPDATES LIVE: a store dataset is never cached, so a
// board reading it again after an agent's write (which the datastore's room
// pushes to every open board) is answered the new rows, and a calculation over
// it the new total.
func TestAStoreDatasetIsReadFreshAfterAWrite(t *testing.T) {
	t.Parallel()
	rig := newBoardRig(t)
	total := func() any {
		_, raw, body := rig.dataset(t, "carol", "total")
		if body["status"] != "ready" {
			t.Fatalf("carol reads the pipeline total: %s", raw)
		}
		return body["table"].(map[string]any)["rows"].([]any)[0].(map[string]any)["amount"]
	}
	if got := total(); got != 350.0 {
		t.Fatalf("the pipeline totals %v, want 350", got)
	}
	rig.data.mu.Lock()
	*rig.data.deals = append(*rig.data.deals, 650)
	rig.data.mu.Unlock()
	if got := total(); got != 1000.0 {
		t.Errorf("after the agent's write the pipeline totals %v, want 1000", got)
	}
}

// THE BOARD AND ITS OUTLINE CARRY NO ROW: every reader is handed the same
// spec, so nothing in it may be anybody's data.
func TestTheBoardAndItsOutlineCarryNoRow(t *testing.T) {
	t.Parallel()
	rig := newBoardRig(t)
	for _, path := range []string{"dashboard", "outline"} {
		req := httptest.NewRequest(http.MethodGet, "/artifacts/"+board.String()+"/versions/1/"+path, nil)
		req.Header.Set("Authorization", "Bearer alice-token-long-enough-to-pass")
		rec := httptest.NewRecorder()
		rig.app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
		for _, task := range companyTasks {
			if strings.Contains(rec.Body.String(), task.Key) {
				t.Errorf("%s carries %s", path, task.Key)
			}
		}
	}
	if rig.reads() != 0 {
		t.Errorf("reading the board asked the tracker %d times; only a dataset read may", rig.reads())
	}
}

// A FILTER THE BOARD DOES NOT OFFER IS REFUSED, so nothing a viewer types
// reaches a domain read the author did not write.
func TestADatasetReadTakesOnlyTheBoardsFilters(t *testing.T) {
	t.Parallel()
	rig := newBoardRig(t)
	req := httptest.NewRequest(http.MethodGet,
		"/artifacts/"+board.String()+"/versions/1/sources/work?assignee=token:bob", nil)
	req.Header.Set("Authorization", "Bearer alice-token-long-enough-to-pass")
	rec := httptest.NewRecorder()
	rig.app.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "OPS-7") {
		t.Errorf("a filter the board has none of = %d %s, want 400 and no row", rec.Code, rec.Body.String())
	}
	if rig.reads() != 0 {
		t.Error("a refused filter still reached the tracker")
	}
}
