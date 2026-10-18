package api

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
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
	// Gate is the datastore's own admission, which a join asks with the
	// read verb: ONE ADMISSION for the routes, the read questions and the
	// rooms, so the three cannot disagree about who may see an artifact.
	Gate artifactdataapi.Gate

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
// appliers' committed batches, and its copies leaving or returning to service
// ([engine.Engine] satisfies it).
type RoomFeed interface {
	OnArtifactsMoved(fn func([]artifactsdomain.Moved)) (stop func())
	OnArtifactDataMoved(fn func([]artifactdata.Moved)) (stop func())
	SetOnArtifactsServing(fn func(serving bool))
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

// roomAccess decides a join by the datastore's own admission with the read
// verb: the role the artifact's shares and the chart give the reader, empty
// for one the artifact is answered as absent to (no relation, purged, never
// created) and for one in the trash, whose data is not served.
type roomAccess struct{ gate artifactdataapi.Gate }

func (a roomAccess) Standing(ctx context.Context, p iam.Principal, id artifacts.ID) (artifacts.Role, error) {
	admitted, err := a.gate.Admit(ctx, p, authz.ActionArtifactRead, id)
	var refused *artifactdataapi.Refused
	switch {
	case errors.Is(err, artifactdataapi.ErrNoSuchArtifact), errors.As(err, &refused):
		return "", nil
	case err != nil:
		return "", err
	case admitted.Head.Status == artifactsdomain.StatusTrashed:
		return "", nil
	}
	return admitted.Role, nil
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
