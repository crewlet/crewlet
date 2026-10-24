package queries_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ARTIFACTS' QUESTIONS answer through the artifacts' one admission: a
// caller with no relation is told the artifact is not found, a reader is shown
// what their role shows, the listing is everybody's and filtered rather than
// refused, and the usage is admin's.

var (
	qaPage    = artifacts.IDFor("the queried page")
	qaPrivate = artifacts.IDFor("somebody's private page")
)

// artifactsEstate is the artifacts estate the questions read.
type artifactsEstate struct {
	elevated []int
	trails   []artifactsdomain.TrailQuery
}

func (e *artifactsEstate) heads() map[artifacts.ID]artifactsdomain.Head {
	return map[artifacts.ID]artifactsdomain.Head{
		qaPage: {ID: qaPage, Kind: artifacts.KindHTML, Title: "Page", Owner: "jane", OwnerKind: iam.ActorHuman,
			HeadNumber: 3, Status: artifactsdomain.StatusLive, Shares: []artifacts.Share{
				{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleViewer},
			}},
		qaPrivate: {ID: qaPrivate, Kind: artifacts.KindHTML, Title: "Private", Owner: "kim",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive},
	}
}

func (e *artifactsEstate) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if h, ok := e.heads()[id]; ok {
		return h, statelog.Position{}, nil
	}
	return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
}

func (e *artifactsEstate) List(context.Context, artifactsdomain.Viewer, artifactsdomain.ListQuery) (
	artifactsdomain.ListPage, error) {
	h := e.heads()
	return artifactsdomain.ListPage{Contents: []artifactsdomain.ListItem{{Head: h[qaPage]}, {Head: h[qaPrivate]}},
		Total: 2}, nil
}

func (e *artifactsEstate) SeenBy(context.Context, artifacts.ID) ([]artifactsdomain.Read, error) {
	return nil, nil
}

func (e *artifactsEstate) Version(_ context.Context, id artifacts.ID, n int) (artifactsdomain.Version, error) {
	if n > e.heads()[id].HeadNumber {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	return artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: n}}, nil
}

func (e *artifactsEstate) Versions(context.Context, artifacts.ID, string, int) (artifactsdomain.VersionPage, error) {
	return artifactsdomain.VersionPage{}, nil
}

func (e *artifactsEstate) History(context.Context, artifacts.ID, string, int) (artifactsdomain.HistoryPage, error) {
	return artifactsdomain.HistoryPage{Entries: []artifactsdomain.HistoryEntry{
		{Op: string(artifactsdomain.OpPublish)}, {Op: string(artifactsdomain.OpReadElevated)}}}, nil
}

func (e *artifactsEstate) Assets(context.Context, artifacts.ID) ([]artifactsdomain.Asset, error) {
	return nil, nil
}

func (e *artifactsEstate) Usage(context.Context) (artifactsdomain.Usage, error) {
	return artifactsdomain.Usage{Artifacts: 2}, nil
}

// Trail answers kim's private page's elevated read, as the domain would, and
// refuses an op the log does not record.
func (e *artifactsEstate) Trail(_ context.Context, q artifactsdomain.TrailQuery) (artifactsdomain.TrailPage, error) {
	if q.Op != "" && !q.Op.Valid() {
		return artifactsdomain.TrailPage{}, artifactsdomain.ErrInvalid
	}
	e.trails = append(e.trails, q)
	return artifactsdomain.TrailPage{Entries: []artifactsdomain.TrailEntry{{Artifact: qaPrivate,
		Title: "Private", Owner: "kim", HistoryEntry: artifactsdomain.HistoryEntry{
			Op: string(artifactsdomain.OpReadElevated), Elevated: true,
			Actor: iam.Actor{Name: "ada", Kind: iam.ActorHuman}}}}}, nil
}

func (e *artifactsEstate) ReadElevated(_ context.Context, _ artifactsdomain.Call, _ artifacts.ID, version int) (
	artifactsdomain.Written, error) {
	e.elevated = append(e.elevated, version)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (e *artifactsEstate) Opened(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return artifactsdomain.Written{}, nil
}

// noUnits is a company with no units and no agents.
type noUnits struct{}

func (noUnits) Units(context.Context) ([]string, error)                  { return nil, nil }
func (noUnits) Seats(context.Context) ([]artifactsapi.RosterSeat, error) { return nil, nil }

// artifactsSeam is the artifacts' source over e.
func artifactsSeam(e *artifactsEstate) *queries.ArtifactsSource {
	return &queries.ArtifactsSource{
		Gate: artifactsapi.Gate{Heads: e, Chart: authz.NoChart{}, Marks: e, Roster: noUnits{},
			Limits: func() artifacts.Limits { return artifacts.Limits{CompanyBytes: 1, Artifacts: 1} }},
		Reads: e,
	}
}

// seated is a signed-in person bound to seat.
func seated(t *testing.T, seat string, grants ...iam.Grant) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.NewSHA1(uuid.Nil, []byte(seat)),
		Login: seat + ".person", Kind: iam.KindPerson, Seat: seat, Grants: grants, Stage: iam.StageActive,
		ReauthAt: time.Now().Add(time.Hour)})
}

// EVERY ARTIFACTS QUESTION DECLARES WHO ASKS IT: the listing everybody, every
// question about one artifact `get_artifact`, the usage admin's verb.
//
// HELD HERE RATHER THAN IN TestEveryQuestionDeclaresAPosture's sweep, whose
// seams feed TestEveryQueryThisServerAnswersHasAReader too: these questions
// gain their room readers with the dashboard's artifacts workspace, and that
// change adds the artifacts seam to the sweep beside them.
func TestEveryArtifactsQuestionDeclaresWhoAsksIt(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"artifacts":         "everybody",
		"artifact":          "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_versions": "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_version":  "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_history":  "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_assets":   "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_seen_by":  "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_access":   "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_usage":    "decided_by:" + string(authz.ActionArtifactUsage),
		"artifact_trail":    "decided_by:" + string(authz.ActionAuditRead),
	}
	bare, r := queries.NewRegistry(), queries.NewRegistry()
	queries.Register(bare, queries.Sources{})
	queries.Register(r, queries.Sources{Artifacts: artifactsSeam(&artifactsEstate{})})
	added := 0
	for _, what := range r.Names() {
		if slices.Contains(bare.Names(), what) {
			continue
		}
		added++
		if got, ok := want[what]; !ok {
			t.Errorf("the artifacts seam answers %s and this table does not name it", what)
		} else if posture := r.PostureOf(what).String(); posture != got {
			t.Errorf("%s is %s, want %s", what, posture, got)
		}
	}
	if added != len(want) {
		t.Errorf("the artifacts seam registers %d questions, want %d", added, len(want))
	}
}

func TestTheArtifactsQuestionsAnswerThroughTheOneAdmission(t *testing.T) {
	t.Parallel()
	e := &artifactsEstate{}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Artifacts: artifactsSeam(e)})

	// A CALLER WITH NO RELATION IS TOLD IT IS NOT FOUND, as an unknown id.
	for _, id := range []artifacts.ID{qaPrivate, artifacts.IDFor("nothing")} {
		if _, err := r.Answer(seated(t, "bob"), "artifact", map[string]any{"id": id.String()}); !errors.Is(err,
			queries.ErrNotFound) {
			t.Errorf("bob asking for %s: %v, want not found", id, err)
		}
	}

	// A VIEWER IS SHOWN THE OWNER AND A COUNT.
	got, err := r.Answer(seated(t, "bob"), "artifact", map[string]any{"id": qaPage.String()})
	if err != nil {
		t.Fatal(err)
	}
	if d := got.(artifactsapi.Detail); d.Shares != nil || d.ShareCount != 1 || d.Role != artifacts.RoleViewer {
		t.Errorf("a viewer is shown %+v", d)
	}
	history, err := r.Answer(seated(t, "bob"), "artifact_history", map[string]any{"id": qaPage.String()})
	if err != nil || len(history.(artifactsdomain.HistoryPage).Entries) != 1 {
		t.Errorf("a viewer's history: %+v (%v), want the elevated read left out", history, err)
	}
	if _, err := r.Answer(seated(t, "bob"), "artifact_version", map[string]any{"id": qaPage.String(),
		"n": "9"}); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a version the artifact does not keep: %v", err)
	}

	// THE LISTING IS EVERYBODY'S, FILTERED: kim's private page is left out.
	list, err := r.Answer(seated(t, "bob"), "artifacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if items := list.(artifactsapi.ListPage).Contents; len(items) != 1 || items[0].ID != qaPage {
		t.Errorf("bob's listing: %+v", items)
	}

	// AN ADMINISTRATOR'S READ IS RECORDED, OF THE VERSION IT READS.
	if _, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "artifact_version",
		map[string]any{"id": qaPrivate.String(), "n": "1"}); err != nil {
		t.Fatal(err)
	}
	if len(e.elevated) != 1 || e.elevated[0] != 1 {
		t.Errorf("the elevated reads recorded: %v", e.elevated)
	}

	// THE USAGE IS ADMIN'S.
	if _, err := r.Answer(seated(t, "bob"), "artifact_usage", nil); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a person asking the usage: %v", err)
	}
	if _, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "artifact_usage", nil); err != nil {
		t.Errorf("an administrator asking the usage: %v", err)
	}
}

// THE COMPANY TRAIL LISTS EVERY ARTIFACT'S RECORDS TO AN ADMINISTRATOR (D09): an
// elevated read of kim's private page, which nobody but kim's editors would
// otherwise find, is answered to whoever reads the company-wide trail, narrowed
// by op; a person holding the artifacts app and a share is refused, however
// many artifacts they hold, and an op the log does not record is a bad
// parameter. Mutation: decide the question by a relation and bob reads the
// overrides of artifacts never shared with him.
func TestTheCompanyTrailListsEveryArtifactsRecordsToAnAdministrator(t *testing.T) {
	t.Parallel()
	e := &artifactsEstate{}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Artifacts: artifactsSeam(e)})
	if _, err := r.Answer(seated(t, "bob", iam.GrantArtifacts), "artifact_trail", nil); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("a person asking the company trail: %v, want refused", err)
	}
	got, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "artifact_trail",
		map[string]any{"op": "read_elevated", "limit": "10"})
	if err != nil {
		t.Fatalf("an administrator asking the company trail: %v", err)
	}
	page := got.(artifactsdomain.TrailPage)
	if len(page.Entries) != 1 || page.Entries[0].Artifact != qaPrivate || !page.Entries[0].Elevated {
		t.Errorf("the company trail answered %+v", page)
	}
	if len(e.trails) != 1 || e.trails[0].Op != artifactsdomain.OpReadElevated || e.trails[0].Limit != 10 {
		t.Errorf("the trail was asked %+v", e.trails)
	}
	if _, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "artifact_trail",
		map[string]any{"op": "glance"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("an op the log does not record: %v, want bad params", err)
	}
}
