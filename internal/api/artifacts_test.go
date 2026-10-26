package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// apiPage is the one artifact the artifacts fakes hold: an HTML page jane owns,
// which the fixture's credential (an administrator relating to nothing) reads
// through the admin path.
var (
	apiPage      = artifacts.IDFor("the api's page")
	apiPageBytes = []byte("<!doctype html><p>hello</p>")
	apiPageEntry = artifacts.Entry{Path: "index.html", ContentType: "text/html; charset=utf-8",
		Object: objstore.Object{Key: objstore.KeyAt(time.Unix(1_700_000_000, 0)),
			Hash: objstore.HashOf(apiPageBytes), Size: int64(len(apiPageBytes))}}
)

// apiArtifacts is the artifacts estate, its writer and its object store, each
// answering with a value naming what it was asked.
type apiArtifacts struct{}

// artifactsGate is the artifacts' one admission over estate, with no chart.
func artifactsGate(estate apiArtifacts) artifactsapi.Gate {
	return artifactsapi.Gate{Heads: estate, Chart: authz.NoChart{}, Marks: estate, Roster: estate,
		Limits: func() artifacts.Limits { return artifacts.Limits{CompanyBytes: 1, Artifacts: 1} }}
}

func (apiArtifacts) Brand(context.Context, statelog.Freshness) (artifactsdomain.Adoption, bool, error) {
	return artifactsdomain.Adoption{}, false, nil
}

func (apiArtifacts) BrandFile(context.Context, objstore.Hash) (artifactsdomain.BrandFile, error) {
	return artifactsdomain.BrandFile{}, artifactsdomain.ErrNotFound
}

func (apiArtifacts) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if id != apiPage {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Head{ID: id, Kind: artifacts.KindHTML, Title: "Hello", Owner: "jane",
		HeadNumber: 1, Status: artifactsdomain.StatusLive}, statelog.Position{}, nil
}

func (apiArtifacts) Version(_ context.Context, id artifacts.ID, n int) (artifactsdomain.Version, error) {
	if n != 1 {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: 1,
		Entry: "index.html"}, Manifest: artifacts.Manifest{Entry: "index.html",
		Files: []artifacts.Entry{apiPageEntry}}}, nil
}

func (apiArtifacts) Versions(context.Context, artifacts.ID, string, int) (artifactsdomain.VersionPage, error) {
	return artifactsdomain.VersionPage{Versions: []artifactsdomain.VersionSummary{{Number: 1}}}, nil
}

func (apiArtifacts) History(context.Context, artifacts.ID, string, int) (artifactsdomain.HistoryPage, error) {
	return artifactsdomain.HistoryPage{Entries: []artifactsdomain.HistoryEntry{{Op: "create"}}}, nil
}

func (apiArtifacts) Assets(context.Context, artifacts.ID) ([]artifactsdomain.Asset, error) {
	return nil, nil
}

func (apiArtifacts) SeenBy(context.Context, artifacts.ID) ([]artifactsdomain.Read, error) {
	return []artifactsdomain.Read{{Seat: "bob"}}, nil
}

func (apiArtifacts) Usage(context.Context) (artifactsdomain.Usage, error) {
	return artifactsdomain.Usage{Artifacts: 7}, nil
}

func (apiArtifacts) List(context.Context, artifactsdomain.Viewer, artifactsdomain.ListQuery) (
	artifactsdomain.ListPage, error) {
	return artifactsdomain.ListPage{}, nil
}

func (apiArtifacts) Trail(context.Context, artifactsdomain.TrailQuery) (artifactsdomain.TrailPage, error) {
	return artifactsdomain.TrailPage{Entries: []artifactsdomain.TrailEntry{{Artifact: apiPage,
		HistoryEntry: artifactsdomain.HistoryEntry{Op: string(artifactsdomain.OpReadElevated)}}}}, nil
}

func (apiArtifacts) ReadElevated(context.Context, artifactsdomain.Call, artifacts.ID, int) (
	artifactsdomain.Written, error) {
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (apiArtifacts) Opened(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (apiArtifacts) applied() (artifactsdomain.Written, error) {
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (a apiArtifacts) Patch(context.Context, artifactsdomain.Call, artifacts.ID, artifactsdomain.Patch) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Share(context.Context, artifactsdomain.Call, artifacts.ID, artifacts.Share) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Unshare(context.Context, artifactsdomain.Call, artifacts.ID, artifacts.Audience) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Transfer(context.Context, artifactsdomain.Call, artifacts.ID, string) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) LinkWorkItem(context.Context, artifactsdomain.Call, artifacts.ID, string) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Trash(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Restore(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Purge(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) ClearBrand(context.Context, artifactsdomain.Call) (artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) RemoveAsset(context.Context, artifactsdomain.Call, artifacts.ID, uuid.UUID) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (apiArtifacts) Folders(context.Context, artifactsdomain.Viewer, statelog.Freshness) (
	artifactsdomain.FolderTree, error) {
	return artifactsdomain.FolderTree{Folders: []artifactsdomain.FolderItem{}}, nil
}
func (apiArtifacts) Folder(context.Context, string, statelog.Freshness) (
	artifactsdomain.Folder, statelog.Position, error) {
	return artifactsdomain.Folder{}, statelog.Position{}, artifactsdomain.ErrFolderNotFound
}
func (a apiArtifacts) CreateFolders(context.Context, artifactsdomain.Call, artifactsdomain.FolderCreateInput) (
	artifactsdomain.FolderCreated, error) {
	w, err := a.applied()
	return artifactsdomain.FolderCreated{Result: w.Result}, err
}
func (a apiArtifacts) RenameFolder(context.Context, artifactsdomain.Call, string, string) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) MoveFolder(context.Context, artifactsdomain.Call, string, string) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) RemoveFolder(context.Context, artifactsdomain.Call, string) (artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Move(context.Context, artifactsdomain.Call, artifacts.ID, string) (
	artifactsdomain.Written, error) {
	return a.applied()
}
func (a apiArtifacts) Pin(context.Context, artifactsdomain.Call, artifacts.ID, bool) (artifactsdomain.Written, error) {
	return a.applied()
}

func (apiArtifacts) Open(_ context.Context, o objstore.Object) (io.ReadCloser, error) {
	if o.Hash != apiPageEntry.Object.Hash {
		return nil, objstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(apiPageBytes)), nil
}

func (apiArtifacts) Units(context.Context) ([]string, error)                  { return nil, nil }
func (apiArtifacts) Seats(context.Context) ([]artifactsapi.RosterSeat, error) { return nil, nil }

// THE ARTIFACTS' ROUTES AND THEIR READ QUESTIONS SHARE ONE MUX: every route of
// the surface and every REST mirror registers beside the rest of the API (two
// overlapping patterns would panic the mux), the literals `/artifacts/usage`
// and `/artifacts/trail` are not read as artifacts, and every route answers through the API's guard.
func TestTheArtifactsRoutesAndQuestionsShareOneMux(t *testing.T) {
	t.Parallel()
	estate := apiArtifacts{}
	gate := artifactsGate(estate)
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
	a := newApp(t, api.Options{
		Runtime:   &fakeRuntime{},
		Artifacts: surface,
		Sources:   queries.Sources{Artifacts: &queries.ArtifactsSource{Gate: gate, Reads: estate}},
	})
	page := "/artifacts/" + apiPage.String()
	for _, tc := range []struct {
		method, path, want string
		status             int
	}{
		{"GET", "/artifacts", `"contents"`, http.StatusOK},
		{"GET", "/artifacts?delimiter=%7C", "bad_params", http.StatusBadRequest},
		{"GET", "/artifact-folders", `"folders"`, http.StatusOK},
		{"DELETE", "/artifact-folders/" + uuid.NewString(), "not_found", http.StatusNotFound},
		{"GET", "/artifacts/usage", `"artifacts":7`, http.StatusOK},
		{"GET", "/artifacts/trail?op=read_elevated", `"read_elevated"`, http.StatusOK},
		{"GET", page, `"Hello"`, http.StatusOK},
		{"GET", page + "/versions", `"versions"`, http.StatusOK},
		{"GET", page + "/versions/1", `"manifest"`, http.StatusOK},
		{"GET", page + "/history", `"create"`, http.StatusOK},
		{"GET", page + "/assets", `"assets"`, http.StatusOK},
		{"GET", page + "/seen-by", `"bob"`, http.StatusOK},
		{"GET", page + "/access", `"jane"`, http.StatusOK},
		{"GET", page + "/versions/1/source", `hello`, http.StatusOK},
		{"GET", page + "/versions/1/files/index.html", `hello`, http.StatusOK},
		{"POST", page + "/tickets", `artifact_origin_unset`, http.StatusConflict},
		{"PATCH", page, `"applied"`, http.StatusOK},
		{"GET", "/artifacts/" + artifacts.IDFor("nothing").String(), "not_found", http.StatusNotFound},
	} {
		body := ""
		if tc.method == "PATCH" {
			body = `{"title":"Renamed"}`
		}
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
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
	// THE DOWNLOAD IS AN ATTACHMENT, never a page on the API origin.
	r := httptest.NewRequest("GET", page+"/versions/1/files/index.html", nil)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, authed(r))
	if ct := w.Result().Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" ||
		!strings.HasPrefix(w.Result().Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("the page's file is served as %q, %q", ct, w.Result().Header.Get("Content-Disposition"))
	}
}
