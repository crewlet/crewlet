package api

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// Rooms is what the socket's artifact rooms are decided by and fed from, as
// the engine supplies them. See [Options.Rooms].
type Rooms struct {
	// Gate is the artifacts' own admission, which a join takes as a read:
	// ONE ADMISSION for the artifacts' and the datastore's routes, their
	// read questions and the rooms, so none can disagree with another about
	// who may see an artifact.
	Gate artifactsapi.Gate

	// Stores says where an artifact's datastore stands, through the estate
	// router.
	Stores StoreCatalogue

	// Presence is this node's presence hub.
	Presence stream.Presence

	// Feed is where this node hears what its artifact appliers committed
	// and whether its artifact copies serve.
	Feed RoomFeed

	// Metrics records the rooms' push lag; nil records nothing.
	Metrics stream.Metrics
}

// StoreCatalogue is one artifact's datastore catalogue row, read through the
// estate router.
type StoreCatalogue interface {
	Store(ctx context.Context, id artifacts.ID) (artifactdata.StoreInfo, error)
}

// RoomFeed is where this node hears what moved on its artifacts: both
// appliers' committed batches, from whichever source the engine's live relay
// says this node's sockets hear, and a reset at every switch of it
// ([engine.Engine] satisfies it).
type RoomFeed interface {
	OnArtifactsMoved(fn func([]artifactsdomain.Moved)) (stop func())
	OnArtifactDataMoved(fn func([]artifactdata.Moved)) (stop func())
}

// metrics is r's push-lag recorder, nil for no rooms.
func (r *Rooms) metrics() stream.Metrics {
	if r == nil {
		return nil
	}
	return r.Metrics
}

// streamRooms is r as the socket's service takes it.
func (r *Rooms) streamRooms() stream.Rooms {
	if r == nil {
		return stream.Rooms{}
	}
	return stream.Rooms{Access: roomAccess{gate: r.Gate}, Stores: storeThrough{r.Stores},
		Presence: r.Presence}
}

// roomAccess decides a join as the artifacts' own read admission does: the
// role the artifact's shares and the chart give the reader, empty for one the
// artifact is answered as absent to (no relation, purged, never created), and
// [artifactsdomain.ErrTrashed] for one it is disclosed to while it is in the
// trash, whose data is not served — told apart, because the second is a state
// its owner's restore ends.
//
// A READ OF ITS CONTENT, RECORDED: a room carries the artifact's data to the
// socket in it, so the join is the content read every other surface takes
// ([artifactsapi.Gate.Content]): an artifact in the trash is refused before
// anything is recorded, and an administrator admitted only by the admin path
// is recorded as reading it before the room is joined, with a record that
// could not be written refusing the join as undecidable rather than letting
// the override pass unrecorded. Repeats collapse into the day's first record.
type roomAccess struct{ gate artifactsapi.Gate }

func (a roomAccess) Standing(ctx context.Context, p iam.Principal, id artifacts.ID) (artifacts.Role, error) {
	admitted, err := a.gate.Content(ctx, p, authz.ActionArtifactRead, id, 0)
	var refused *artifactsapi.Refused
	switch {
	case errors.Is(err, artifactsapi.ErrNoSuchArtifact), errors.As(err, &refused):
		return "", nil
	case err != nil:
		return "", err
	}
	return admitted.Role(), nil
}

// storeThrough is the catalogue row's version and delete floor.
type storeThrough struct{ stores StoreCatalogue }

func (s storeThrough) Through(ctx context.Context, id artifacts.ID) (int64, int64, error) {
	info, err := s.stores.Store(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	return info.Version, info.TombstonesFloor, nil
}
