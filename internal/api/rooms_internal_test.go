package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
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
// a share admits gets the role it grants; one with no relation, an artifact
// nothing holds and one in the trash are each answered as absent; a read this
// node could not make is undecidable, never absent.
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
	access := roomAccess{gate: artifactdataapi.Gate{Heads: heads, Chart: authz.NoChart{}}}
	person := func(seat string) iam.Principal {
		return iam.Principal{Kind: iam.KindPerson, Login: seat + "@example.com", Seat: seat,
			Stage: iam.StageActive, Grants: iam.Apps}
	}
	for name, tc := range map[string]struct {
		who  string
		id   artifacts.ID
		want artifacts.Role
	}{
		"a viewer's share":         {"erin", live, artifacts.RoleViewer},
		"the owner":                {"dana", live, artifacts.RoleOwner},
		"no relation":              {"fred", live, ""},
		"an artifact in the trash": {"erin", trashed, ""},
		"nothing at that id":       {"erin", artifacts.IDFor("rooms-nothing"), ""},
	} {
		got, err := access.Standing(t.Context(), person(tc.who), tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%s: standing %q, %v; want %q", name, got, err, tc.want)
		}
	}
	broken := roomAccess{gate: artifactdataapi.Gate{Heads: roomHeads{err: errors.New("the copy is behind")},
		Chart: authz.NoChart{}}}
	if got, err := broken.Standing(t.Context(), person("erin"), live); err == nil || got != "" {
		t.Fatalf("a head this node could not read answered %q, %v; want undecidable", got, err)
	}
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
