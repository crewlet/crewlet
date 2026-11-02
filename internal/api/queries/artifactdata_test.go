package queries_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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

// dataSheet is an artifact jane owns, shared with eve as editor and with sam
// as viewer; nobody else has a relation to it. dataBinned is the same sheet in
// the trash.
var (
	dataSheet  = artifacts.IDFor("the questions' review sheet")
	dataBinned = artifacts.IDFor("the questions' trashed sheet")
)

type dataHeads struct{}

func (dataHeads) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	status := artifactsdomain.StatusLive
	switch id {
	case dataSheet:
	case dataBinned:
		status = artifactsdomain.StatusTrashed
	default:
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Head{ID: id, Owner: "jane", OwnerKind: iam.ActorHuman, HeadNumber: 3,
		Status: status, Shares: []artifacts.Share{
			{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "eve"}, Role: artifacts.RoleEditor},
			{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "sam"}, Role: artifacts.RoleViewer},
		}}, statelog.Position{}, nil
}

// dataMarks records the elevated reads the gate writes, or refuses them all.
type dataMarks struct {
	mu       sync.Mutex
	elevated []artifactsdomain.Call
	err      error
}

func (m *dataMarks) ReadElevated(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, _ int) (
	artifactsdomain.Written, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return artifactsdomain.Written{}, m.err
	}
	m.elevated = append(m.elevated, c)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (m *dataMarks) Opened(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (m *dataMarks) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.elevated)
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
	r, reads, _ := dataRegistryWithMarks(t)
	return r, reads
}

// dataRegistryWithMarks is [dataRegistry] with the reader marks its gate
// records elevated reads into.
func dataRegistryWithMarks(t *testing.T) (*queries.Registry, *dataReads, *dataMarks) {
	t.Helper()
	reads, marks := &dataReads{}, &dataMarks{}
	gate := artifactsapi.Gate{Heads: dataHeads{}, Chart: authz.NoChart{}, Marks: marks,
		Limits: func() artifacts.Limits { return artifacts.Limits{DataBytes: 1 << 30, DataStoreBytes: 32 << 20} }}
	return registryOver(t, queries.Sources{ArtifactData: &queries.ArtifactDataSource{Gate: gate, Reads: reads}}),
		reads, marks
}

// dataPerson is a signed-in person on seat holding the artifacts app, and any
// further grants.
func dataPerson(t *testing.T, seat string, grants ...iam.Grant) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.NewSHA1(uuid.Nil, []byte(seat)),
		Login: seat + ".person", Kind: iam.KindPerson, Seat: seat,
		Grants: append([]iam.Grant{iam.GrantArtifacts}, grants...),
		Stage:  iam.StageActive, Via: iam.SessionName("lineage-" + seat), ReauthAt: time.Now().Add(time.Hour)})
}

// dataQuestions is every datastore question decided per artifact.
var dataQuestions = []string{"artifact_data", "artifact_data_doc", "artifact_data_query",
	"artifact_data_history", "artifact_data_schema", "artifact_data_state"}

// AN ADMINISTRATOR'S DATASTORE READ IS RECORDED BEFORE A ROW IS ANSWERED (D09):
// every question an administrator with no relation asks writes the elevated
// read first, and none is answered when the record cannot be written. An
// administrator who holds a viewer share reads as that viewer, recorded as
// nothing, and is not shown the editors' collection. Mutation: admit the
// questions through Gate.Admit and ada's reads leave no record; stand the
// sharer at owner and sam is shown the notes.
func TestAnAdministratorsDatastoreReadIsRecorded(t *testing.T) {
	t.Parallel()
	r, reads, marks := dataRegistryWithMarks(t)
	params := map[string]any{"id": dataSheet.String(), "collection": "review", "doc": "r1"}
	for i, what := range dataQuestions {
		if _, err := r.Answer(dataPerson(t, "ada", iam.GrantAdmin), what, params); err != nil {
			t.Fatalf("ada asking %s: %v", what, err)
		}
		if got := marks.count(); got != i+1 {
			t.Fatalf("ada asking %s left %d elevated reads, want %d", what, got, i+1)
		}
	}
	before := marks.count()
	for _, what := range dataQuestions {
		if _, err := r.Answer(dataPerson(t, "sam", iam.GrantAdmin), what, params); err != nil {
			t.Fatalf("sam asking %s: %v", what, err)
		}
		if what != "artifact_data_schema" {
			if got := reads.last(t); got.Role != artifacts.RoleViewer {
				t.Errorf("sam, a viewer holding admin, asking %s was read as %+v", what, got)
			}
		}
	}
	if marks.count() != before {
		t.Errorf("an administrator's reads through their own share left %d elevated reads",
			marks.count()-before)
	}
	got, err := r.Answer(dataPerson(t, "sam", iam.GrantAdmin), "artifact_data_schema",
		map[string]any{"id": dataSheet.String()})
	if err != nil {
		t.Fatal(err)
	}
	if collections, _ := asMap(t, got)["collections"].([]any); len(collections) != 1 {
		t.Errorf("sam, a viewer holding admin, is shown %d collections, want the one viewers read",
			len(collections))
	}

	failing, failingReads, failingMarks := dataRegistryWithMarks(t)
	failingMarks.err = errors.New("the artifacts log is not reachable")
	for _, what := range dataQuestions {
		if _, err := failing.Answer(dataPerson(t, "ada", iam.GrantAdmin), what, params); !errors.Is(err,
			queries.ErrUnavailable) {
			t.Errorf("ada asking %s with no record written = %v, want unavailable", what, err)
		}
	}
	if len(failingReads.readings) != 0 {
		t.Errorf("an unrecorded elevated read reached the datastore %d times", len(failingReads.readings))
	}
}

// THE TRASH STOPS SERVING THE DATASTORE AT ONCE: every question about a
// trashed sheet is answered `artifact_trashed`, to its owner, its editor and
// an administrator alike, and reaches nothing — the answer every artifacts
// route gives it, because the trash is a state its owner's restore ends. A
// caller the sheet is not disclosed to is still told it is not there, so the
// trash discloses nothing the stranger case below does not.
//
// Mutation: drop the status check from Gate.Content and the rows are read;
// answer the trash as not found and the disclosed rows go red; decide the
// trash before the admission and the stranger's row does.
func TestATrashedSheetsDataIsNotServed(t *testing.T) {
	t.Parallel()
	r, reads, marks := dataRegistryWithMarks(t)
	for _, ctx := range []context.Context{dataPerson(t, "jane"), dataPerson(t, "eve"),
		dataPerson(t, "ada", iam.GrantAdmin)} {
		for _, what := range dataQuestions {
			_, err := r.Answer(ctx, what, map[string]any{"id": dataBinned.String(),
				"collection": "review", "doc": "r1"})
			if !errors.Is(err, queries.ErrArtifactTrashed) || errors.Is(err, queries.ErrNotFound) {
				t.Errorf("asking %s about a trashed sheet = %v, want artifact_trashed", what, err)
			}
		}
	}
	for _, what := range dataQuestions {
		_, err := r.Answer(dataPerson(t, "dana"), what, map[string]any{"id": dataBinned.String(),
			"collection": "review", "doc": "r1"})
		if !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("a stranger asking %s about a trashed sheet = %v, want not found", what, err)
		}
	}
	if len(reads.readings) != 0 || marks.count() != 0 {
		t.Errorf("a trashed sheet's questions reached the datastore %d times and left %d records",
			len(reads.readings), marks.count())
	}
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
