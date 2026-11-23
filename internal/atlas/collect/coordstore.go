package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Statuses is the coordination store's bucket of source standings
// (coord.AtlasSources), as this package reads it.
type Statuses interface {
	AtlasSourceStatuses(ctx context.Context) (map[string][]byte, error)
	PutAtlasSourceStatus(ctx context.Context, kind string, value []byte) error
	AtlasSourceMisses(ctx context.Context, kind string) ([]byte, bool, error)
	PutAtlasSourceMisses(ctx context.Context, kind string, value []byte) error
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
//
// TWO RECORDS A SOURCE: its standing, which every tick and report lists, and
// its misses, which only a visit of the source reads, by its key. A listing
// of every source's misses at their widest would wait minutes to cross a
// link at the floor, every tick and every report (ADR-0175, "The atlas
// buckets declare their classes").
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

// Load reads every recorded standing, without its misses ([CoordStore.Misses]).
// One this build cannot decode is skipped, as an integration status is: the
// honest reading of a value no longer readable is "this source has no
// standing I can use", and the next read overwrites it.
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

// misses is a source's misses as recorded: [Status.Missing] and
// [Status.MissingOverflow].
type misses struct {
	Missing  []Missing `json:"missing,omitempty"`
	Overflow int       `json:"overflow,omitempty"`
}

// Misses is st with the misses recorded for its source. None recorded, or
// none this build can decode, reads as nothing missed: each entry is missed
// afresh by the next complete read, a day later to retire rather than never.
func (s *CoordStore) Misses(ctx context.Context, st Status) (Status, error) {
	value, found, err := s.statuses.AtlasSourceMisses(ctx, st.Kind.String())
	if err != nil {
		return st, fmt.Errorf("collect: read the %s source's misses: %w", st.Kind, err)
	}
	var m misses
	if found {
		if err := json.Unmarshal(value, &m); err != nil {
			log.WarnContext(ctx, "atlas_source_misses_undecodable", "source", st.Kind.String(), "error", err,
				"detail", "every entry this source stopped seeing is missed afresh by its next read")
			m = misses{}
		}
	}
	st.Missing, st.MissingOverflow = m.Missing, m.Overflow
	return st, nil
}

// Save records one source's standing, its misses apart ([CoordStore.SaveMisses]).
func (s *CoordStore) Save(ctx context.Context, st Status) error {
	if st.Kind == "" {
		return errors.New("collect: cannot record a standing for no source")
	}
	value, err := encodeStanding(st)
	if err != nil {
		return fmt.Errorf("collect: encode the %s standing: %w", st.Kind, err)
	}
	if err := s.statuses.PutAtlasSourceStatus(ctx, st.Kind.String(), value); err != nil {
		return fmt.Errorf("collect: record the %s standing: %w", st.Kind, err)
	}
	return nil
}

// SaveMisses records one source's misses: [Status.Missing], at most
// [MaxMissing] entries, which fit coord.MaxAtlasSourceMissesBytes at their
// widest (TestTheWidestAtlasMissesFitTheirBound).
func (s *CoordStore) SaveMisses(ctx context.Context, st Status) error {
	if st.Kind == "" {
		return errors.New("collect: cannot record the misses of no source")
	}
	value, err := json.Marshal(misses{Missing: st.Missing, Overflow: st.MissingOverflow})
	if err != nil {
		return fmt.Errorf("collect: encode the %s source's misses: %w", st.Kind, err)
	}
	if err := s.statuses.PutAtlasSourceMisses(ctx, st.Kind.String(), value); err != nil {
		return fmt.Errorf("collect: record the %s source's misses: %w", st.Kind, err)
	}
	return nil
}

// encodeStanding is st as its standing record, within
// coord.MaxAtlasSourceStandingBytes. CUT, NEVER REFUSED: what grows it is
// text a vendor wrote, escaped up to six bytes a byte, so each refused part's
// detail is halved first, then the last error, then each part and its scope,
// until it fits. Every count and scope stays: they are what a reader judges
// the source by. With every text cut to its marker the widest is about 27 KB.
func encodeStanding(st Status) ([]byte, error) {
	st.Unread = append([]Finding(nil), st.Unread...)
	stages := []func(*Status) bool{
		func(st *Status) bool {
			return halveEach(st.Unread, func(f *Finding) []*string { return []*string{&f.Detail} })
		},
		func(st *Status) bool { return halve(&st.LastError) },
		func(st *Status) bool {
			return halveEach(st.Unread, func(f *Finding) []*string { return []*string{&f.Part, &f.Scope} })
		},
	}
	for {
		value, err := json.Marshal(st)
		if err != nil || len(value) <= coord.MaxAtlasSourceStandingBytes {
			return value, err
		}
		for len(stages) > 0 && !stages[0](&st) {
			stages = stages[1:]
		}
		if len(stages) == 0 {
			return nil, coord.HoldValue("the "+st.Kind.String()+" standing", len(value),
				coord.MaxAtlasSourceStandingBytes)
		}
	}
}

// halveEach halves every text fields names of each finding, reporting
// whether any was cut.
func halveEach(findings []Finding, fields func(*Finding) []*string) bool {
	cut := false
	for i := range findings {
		for _, text := range fields(&findings[i]) {
			cut = halve(text) || cut
		}
	}
	return cut
}

// halve cuts text to half its bytes, marked, reporting false where it is
// already no longer than the marker alone.
func halve(text *string) bool {
	cut := textcut.Within(*text, len(*text)/2)
	if len(cut) >= len(*text) {
		return false
	}
	*text = cut
	return true
}

// Forget drops a source's standing and its misses.
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

// Ask records a person's ask that kind be read now. Its one text is cut to
// [MaxRequestByLength], so an ask is at most about 800 bytes as stored, well
// inside the coord.SmallValueBytes it is held to.
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
