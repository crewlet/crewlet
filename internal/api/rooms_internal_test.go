package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// roomHeads holds artifact heads by id, and an error that makes every read
// fail.
type roomHeads struct {
	heads map[artifacts.ID]artifactsdomain.Head
	err   error
}

func (h roomHeads) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {

	if h.err != nil {
		return artifactsdomain.Head{}, statelog.Position{}, h.err
	}
	head, ok := h.heads[id]
	if !ok {
		return artifactsdomain.Head{}, statelog.Position{}, fmt.Errorf("%w: %s",
			artifactsdomain.ErrNotFound, id)
	}
	return head, statelog.Position{}, nil
}

// A ROOM'S JOIN IS THE DATASTORE'S OWN ADMISSION WITH THE READ VERB: a reader
// a share admits gets the role it grants; one with no relation and an artifact
// nothing holds are each answered as absent; an artifact in the trash is the
// trash to a reader it is disclosed to (`artifact_trashed` on the socket) and
// absent to anybody else; a read this node could not make is undecidable,
// never absent.
func TestARoomIsJoinedByTheDatastoresOwnAdmission(t *testing.T) {
	t.Parallel()
	live, trashed := artifacts.IDFor("rooms-live"), artifacts.IDFor("rooms-trashed")
	shares := []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat,
		ID: "erin"}, Role: artifacts.RoleViewer}}
	heads := roomHeads{heads: map[artifacts.ID]artifactsdomain.Head{
		live: {ID: live, Owner: "dana", OwnerKind: iam.ActorHuman, Shares: shares,
			Status: artifactsdomain.StatusLive},
		trashed: {ID: trashed, Owner: "dana", OwnerKind: iam.ActorHuman, Shares: shares,
			Status: artifactsdomain.StatusTrashed},
	}}
	marks := &roomMarks{}
	access := roomAccess{gate: artifactsapi.Gate{Heads: heads, Chart: authz.NoChart{}, Marks: marks,
		Limits: func() artifacts.Limits { return artifacts.Limits{} }}}
	person := func(seat string) iam.Principal {
		return iam.Principal{Kind: iam.KindPerson, Login: seat + "@example.com", Seat: seat,
			Stage: iam.StageActive, Grants: iam.Apps}
	}
	for name, tc := range map[string]struct {
		who  string
		id   artifacts.ID
		want artifacts.Role
	}{
		"a viewer's share":                   {"erin", live, artifacts.RoleViewer},
		"the owner":                          {"dana", live, artifacts.RoleOwner},
		"no relation":                        {"fred", live, ""},
		"an artifact in the trash, unshared": {"fred", trashed, ""},
		"nothing at that id":                 {"erin", artifacts.IDFor("rooms-nothing"), ""},
	} {
		got, err := access.Standing(t.Context(), person(tc.who), tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%s: standing %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := access.Standing(t.Context(), person("erin"), trashed); got != "" ||
		!errors.Is(err, artifactsdomain.ErrTrashed) {
		t.Errorf("a reader of a binned artifact: standing %q, %v; want the trash", got, err)
	}
	broken := roomAccess{gate: artifactsapi.Gate{Heads: roomHeads{err: errors.New("the copy is behind")},
		Chart: authz.NoChart{}}}
	if got, err := broken.Standing(t.Context(), person("erin"), live); err == nil || got != "" {
		t.Fatalf("a head this node could not read answered %q, %v; want undecidable", got, err)
	}
	if len(marks.elevated) != 0 {
		t.Fatalf("joins by readers a share admits recorded %d elevated reads", len(marks.elevated))
	}

	// AN ADMINISTRATOR WITH NO RELATION joins by the admin path, and the
	// join is recorded as an elevated read before it is answered; a record
	// that could not be written refuses the join as undecidable.
	admin := iam.Principal{Kind: iam.KindPerson, Login: "gail@example.com", Seat: "gail",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}}
	got, err := access.Standing(t.Context(), admin, live)
	if err != nil || got != artifacts.RoleOwner || len(marks.elevated) != 1 {
		t.Fatalf("an administrator's join answered %q, %v, recording %d", got, err, len(marks.elevated))
	}
	marks.err = errors.New("the log refused the record")
	if got, err := access.Standing(t.Context(), admin, live); err == nil || got != "" {
		t.Fatalf("an unrecorded elevated join answered %q, %v; want undecidable", got, err)
	}
}

// roomMarks records the elevated reads a join leaves, and refuses them with err.
type roomMarks struct {
	elevated []artifacts.ID
	err      error
}

func (m *roomMarks) ReadElevated(_ context.Context, _ artifactsdomain.Call, id artifacts.ID, _ int) (
	artifactsdomain.Written, error) {

	if m.err != nil {
		return artifactsdomain.Written{}, m.err
	}
	m.elevated = append(m.elevated, id)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (m *roomMarks) Opened(context.Context, artifactsdomain.Call, artifacts.ID) (artifactsdomain.Written, error) {
	return artifactsdomain.Written{}, nil
}

// storeCatalogue answers one store's catalogue row.
type storeCatalogue struct{ info artifactdata.StoreInfo }

func (s storeCatalogue) Store(context.Context, artifacts.ID) (artifactdata.StoreInfo, error) {
	return s.info, nil
}

// A JOIN'S CATCH-UP POINT IS THE STORE'S VERSION AND ITS DELETE FLOOR, the two
// facts the datastore's own catch-up read answers `resync` by.
func TestARoomsCatchUpPointIsTheStoresOwn(t *testing.T) {
	t.Parallel()
	through, floor, err := storeThrough{storeCatalogue{artifactdata.StoreInfo{Version: 88,
		TombstonesFloor: 12}}}.Through(t.Context(), artifacts.IDFor("x"))
	if err != nil || through != 88 || floor != 12 {
		t.Fatalf("the catch-up point is %d over floor %d (%v), want 88 over 12", through, floor, err)
	}
	var none *Rooms
	if r := none.streamRooms(); r.Access != nil || r.Stores != nil || r.Presence != nil {
		t.Fatal("no rooms built a half of them")
	}
}
