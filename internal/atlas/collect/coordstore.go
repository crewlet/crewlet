package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Statuses is the coordination store's bucket of source standings
// (coord.AtlasSources), as this package reads it.
type Statuses interface {
	AtlasSourceStatuses(ctx context.Context) (map[string][]byte, error)
	PutAtlasSourceStatus(ctx context.Context, kind string, value []byte) error
	DeleteAtlasSourceStatus(ctx context.Context, kind string) error

	// The asks that a source be read now (coord.AtlasSourceRefreshes).
	AtlasSourceRefreshes(ctx context.Context) (map[string][]byte, error)
	PutAtlasSourceRefresh(ctx context.Context, kind string, value []byte) error
	DeleteAtlasSourceRefresh(ctx context.Context, kind string) error
}

// CoordStore is each source's standing on the fleet's coordination store:
// the whole company needs the current value, and only the current value, so
// a node taking the duty over reads when each source was last read whole
// and what it has stopped seeing, and the dashboard reads the same row on
// whichever node serves it.
type CoordStore struct{ statuses Statuses }

// NewCoordStore wraps the fleet's standings, refusing a nil backend for the
// integration store's reason: a nil wrapped is a crash on the first tick.
func NewCoordStore(statuses Statuses) (*CoordStore, error) {
	if statuses == nil {
		return nil, errors.New("collect: NewCoordStore needs the fleet's coordination backend " +
			"(statuses is nil), which is where the duty records each source's standing")
	}
	return &CoordStore{statuses: statuses}, nil
}

// Load reads every recorded standing. One this build cannot decode is
// skipped, as an integration status is: the honest reading of a value no
// longer readable is "this source has no standing I can use", and the next
// read overwrites it.
func (s *CoordStore) Load(ctx context.Context) (map[integration.Kind]Status, error) {
	raw, err := s.statuses.AtlasSourceStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect: read the source standings: %w", err)
	}
	out := make(map[integration.Kind]Status, len(raw))
	for kind, value := range raw {
		var st Status
		if err := json.Unmarshal(value, &st); err != nil {
			log.WarnContext(ctx, "atlas_source_standing_undecodable", "source", kind, "error", err,
				"detail", "this source is read again and its standing rewritten")
			continue
		}
		// THE KEY IS AUTHORITATIVE, not the field inside the document, for
		// the integration store's reason.
		st.Kind = integration.Kind(kind)
		out[st.Kind] = st
	}
	return out, nil
}

// Save records one source's standing.
func (s *CoordStore) Save(ctx context.Context, st Status) error {
	if st.Kind == "" {
		return errors.New("collect: cannot record a standing for no source")
	}
	value, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("collect: encode the %s standing: %w", st.Kind, err)
	}
	if err := s.statuses.PutAtlasSourceStatus(ctx, st.Kind.String(), value); err != nil {
		return fmt.Errorf("collect: record the %s standing: %w", st.Kind, err)
	}
	return nil
}

// Forget drops a source's standing.
func (s *CoordStore) Forget(ctx context.Context, kind integration.Kind) error {
	if err := s.statuses.DeleteAtlasSourceStatus(ctx, kind.String()); err != nil {
		return fmt.Errorf("collect: forget the %s standing: %w", kind, err)
	}
	return nil
}

// Requests reads every waiting ask that a source be read now. One this
// build cannot decode is skipped, as a standing is: the duty drops it with
// the next read of its source.
func (s *CoordStore) Requests(ctx context.Context) (map[integration.Kind]Request, error) {
	raw, err := s.statuses.AtlasSourceRefreshes(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect: read the refresh asks: %w", err)
	}
	out := make(map[integration.Kind]Request, len(raw))
	for kind, value := range raw {
		var req Request
		if err := json.Unmarshal(value, &req); err != nil {
			log.WarnContext(ctx, "atlas_source_refresh_undecodable", "source", kind, "error", err)
			continue
		}
		out[integration.Kind(kind)] = req
	}
	return out, nil
}

// Ask records a person's ask that kind be read now.
func (s *CoordStore) Ask(ctx context.Context, kind integration.Kind, req Request) error {
	if kind == "" || req.RequestedAt.IsZero() {
		return errors.New("collect: a refresh ask names its source and its instant")
	}
	req.RequestedAt = req.RequestedAt.UTC()
	req.By = textcut.Within(req.By, MaxRequestByLength)
	value, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("collect: encode the %s refresh ask: %w", kind, err)
	}
	if err := s.statuses.PutAtlasSourceRefresh(ctx, kind.String(), value); err != nil {
		return fmt.Errorf("collect: record the %s refresh ask: %w", kind, err)
	}
	return nil
}

// Answered drops a source's ask, once it is read or gone.
func (s *CoordStore) Answered(ctx context.Context, kind integration.Kind) error {
	if err := s.statuses.DeleteAtlasSourceRefresh(ctx, kind.String()); err != nil {
		return fmt.Errorf("collect: drop the %s refresh ask: %w", kind, err)
	}
	return nil
}
