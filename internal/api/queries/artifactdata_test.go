package queries_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// dataSheet is an artifact jane owns, shared with eve as editor and with sam
// as viewer; nobody else has a relation to it.
var dataSheet = artifacts.IDFor("the questions' review sheet")

type dataHeads struct{}

func (dataHeads) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if id != dataSheet {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Head{ID: id, Owner: "jane", OwnerKind: iam.ActorHuman,
		Status: artifactsdomain.StatusLive, Shares: []artifacts.Share{
			{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "eve"}, Role: artifacts.RoleEditor},
			{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "sam"}, Role: artifacts.RoleViewer},
		}}, statelog.Position{}, nil
}

// dataReads keeps every Reading a question handed it, and holds one collection
// every participant reads and one only editors do.
type dataReads struct {
	mu       sync.Mutex
	readings []artifactdata.Reading
}

func (d *dataReads) saw(rd artifactdata.Reading) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readings = append(d.readings, rd)
}

func (d *dataReads) last(t *testing.T) artifactdata.Reading {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.readings) == 0 {
		t.Fatal("no question reached the datastore")
	}
	return d.readings[len(d.readings)-1]
}

func (d *dataReads) Docs(_ context.Context, _ artifacts.ID, rd artifactdata.Reading, _ string, _ int64,
	_ string, _ int) (artifactdata.DocPage, error) {
	d.saw(rd)
	return artifactdata.DocPage{Docs: []artifactdata.Doc{}}, nil
}

func (d *dataReads) Doc(_ context.Context, _ artifacts.ID, rd artifactdata.Reading, collection, doc string) (
	artifactdata.Doc, error) {
	d.saw(rd)
	return artifactdata.Doc{Collection: collection, ID: doc}, nil
}

func (d *dataReads) Query(_ context.Context, _ artifacts.ID, rd artifactdata.Reading, _ string,
	_ query.Query) (artifactdata.DocPage, error) {
	d.saw(rd)
	return artifactdata.DocPage{Docs: []artifactdata.Doc{}}, nil
}

func (d *dataReads) History(_ context.Context, _ artifacts.ID, rd artifactdata.Reading,
	_ artifactdata.HistoryFilter, _ string, _ int) (artifactdata.HistoryPage, error) {
	d.saw(rd)
	return artifactdata.HistoryPage{Entries: []artifactdata.HistoryEntry{}}, nil
}

func (d *dataReads) Schema(context.Context, artifacts.ID) ([]artifacts.Collection, error) {
	return []artifacts.Collection{
		{Name: "review", Read: artifacts.ReadAll},
		{Name: "notes", Read: artifacts.ReadEditors},
	}, nil
}

func (d *dataReads) Store(_ context.Context, id artifacts.ID) (artifactdata.StoreInfo, error) {
	return artifactdata.StoreInfo{Artifact: id}, nil
}

func (d *dataReads) Usage(context.Context) (artifactdata.Usage, error) {
	return artifactdata.Usage{}, nil
}

func (d *dataReads) State(_ context.Context, _ artifacts.ID, rd artifactdata.Reading) (
	[]artifactdata.StateEntry, error) {
	d.saw(rd)
	return []artifactdata.StateEntry{}, nil
}

// dataRegistry is the registry over the datastore's questions alone.
func dataRegistry(t *testing.T) (*queries.Registry, *dataReads) {
	t.Helper()
	reads := &dataReads{}
	gate := artifactdataapi.Gate{Heads: dataHeads{}, Chart: authz.NoChart{},
		Limits: func() artifacts.Limits { return artifacts.Limits{DataBytes: 1 << 30, DataStoreBytes: 32 << 20} }}
	return registryOver(t, queries.Sources{ArtifactData: &queries.ArtifactDataSource{Gate: gate, Reads: reads}}), reads
}

// dataPerson is a signed-in person on seat holding the artifacts app.
func dataPerson(t *testing.T, seat string) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.NewSHA1(uuid.Nil, []byte(seat)),
		Login: seat + ".person", Kind: iam.KindPerson, Seat: seat, Grants: []iam.Grant{iam.GrantArtifacts},
		Stage: iam.StageActive, Via: iam.SessionName("lineage-" + seat), ReauthAt: time.Now().Add(time.Hour)})
}

// EVERY DATASTORE QUESTION IS SHAPED BY THE READER THE ADMISSION NAMES: the
// role the artifact's shares give them and the record a row's creator is
// compared with, whatever the parameters say.
func TestADatastoreQuestionIsShapedByTheDecidedReader(t *testing.T) {
	t.Parallel()
	r, reads := dataRegistry(t)
	params := map[string]any{"id": dataSheet.String(), "collection": "review", "doc": "r1", "role": "owner"}
	for _, tc := range []struct {
		seat string
		want artifacts.Role
	}{{"sam", artifacts.RoleViewer}, {"eve", artifacts.RoleEditor}, {"jane", artifacts.RoleOwner}} {
		for _, what := range []string{"artifact_data", "artifact_data_doc", "artifact_data_query",
			"artifact_data_history", "artifact_data_state"} {
			if _, err := r.Answer(dataPerson(t, tc.seat), what, params); err != nil {
				t.Fatalf("%s asking %s: %v", tc.seat, what, err)
			}
			if got := reads.last(t); got.Role != tc.want || got.Record != tc.seat {
				t.Errorf("%s asking %s was read as %+v, want role %s and record %s", tc.seat, what,
					got, tc.want, tc.seat)
			}
		}
	}
}

// A COLLECTION THE READER MAY NEVER READ A ROW OF IS NOT IN THE SCHEMA THEY ARE
// SHOWN: its name and its columns are the editors' too.
func TestTheDatastoresSchemaLeavesOutWhatTheReaderMayNotRead(t *testing.T) {
	t.Parallel()
	r, _ := dataRegistry(t)
	for seat, want := range map[string][]string{"sam": {"review"}, "eve": {"review", "notes"}} {
		got, err := r.Answer(dataPerson(t, seat), "artifact_data_schema", map[string]any{"id": dataSheet.String()})
		if err != nil {
			t.Fatalf("%s: %v", seat, err)
		}
		collections, _ := asMap(t, got)["collections"].([]any)
		var names []string
		for _, c := range collections {
			name, _ := c.(map[string]any)["name"].(string)
			names = append(names, name)
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s is shown the collections %v, want %v", seat, names, want)
		}
	}
}

// A STRANGER IS ANSWERED EXACTLY AS AN UNKNOWN ARTIFACT, on every question, so
// a read tells nobody an artifact they were never shared exists, and reads
// nothing for them.
func TestAStrangerAsksAboutAnArtifactThatIsNotThere(t *testing.T) {
	t.Parallel()
	r, reads := dataRegistry(t)
	for _, what := range []string{"artifact_data", "artifact_data_doc", "artifact_data_query",
		"artifact_data_history", "artifact_data_schema", "artifact_data_state"} {
		for _, id := range []string{dataSheet.String(), artifacts.IDFor("nothing").String(), "not-an-id"} {
			_, err := r.Answer(dataPerson(t, "dana"), what, map[string]any{"id": id, "collection": "review",
				"doc": "r1"})
			if !errors.Is(err, queries.ErrNotFound) {
				t.Errorf("dana asking %s about %s = %v, want not found", what, id, err)
			}
		}
	}
	if len(reads.readings) != 0 {
		t.Errorf("a stranger's questions reached the datastore %d times", len(reads.readings))
	}
}

// THE COMPANY'S DATASTORE FOOTPRINT IS THE DEPLOYMENT'S MACHINERY, so the
// usage verb's: an admin's, and refused to a person holding the artifacts
// app alone.
func TestTheDatastoresUsageIsAnAdminsQuestion(t *testing.T) {
	t.Parallel()
	r, _ := dataRegistry(t)
	if _, err := r.Answer(dataPerson(t, "jane"), "artifact_data_usage", nil); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a person without admin asking for the footprint = %v, want a refusal", err)
	}
	got, err := r.Answer(everyGrant(t), "artifact_data_usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	if limit, _ := asMap(t, got)["data_bytes"].(float64); limit != 1<<30 {
		t.Errorf("the footprint is answered against %v, want the plan's 1 GiB", limit)
	}
}
