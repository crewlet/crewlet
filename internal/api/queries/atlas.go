// The company's atlas, read: its entries as a list, as a map, one entry, a
// trace of what depends on one, and the counts a filter menu offers. Every
// question takes the atlas app (ADR-0127) and reads the atlas as its asker:
// Gitlet's repositories only for a holder of Gitlet, and an integration's
// standing or a connection test only for an administrator, so the atlas is
// never a way round another screen (ADR-0126).

package queries

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AtlasReads is the atlas as this node reads it for one asker: the settings
// it runs, the engine's records, and Gitlet as asker where reads is set or
// why it is not read for them (unread); false where it runs no company yet.
// builtin.AtlasDeps serves it, as it serves the atlas tools.
type AtlasReads interface {
	AtlasRead(ctx context.Context, reads gather.GitletReads, unread string, asker gitlet.Principal,
		fresh statelog.Freshness, now time.Time) (gather.Answer, bool)
}

// atlasFresh is the read level every atlas question reads at: the
// dashboard's, which carries its lag rather than waiting on a leader.
var atlasFresh = statelog.Freshness{Level: statelog.ReadStale}

// AtlasRead is what every atlas answer carries beside its own: the settings
// revision it read ("" where this node cannot tell), which sources it read
// in full and why not, and how many values it left out for being shaped like
// a credential, counted and never shown.
type AtlasRead struct {
	Revision string            `json:"revision"`
	Coverage []gather.Coverage `json:"coverage"`
	Withheld int               `json:"withheld"`
}

// AtlasEntriesAnswer is one page of the atlas as a list.
type AtlasEntriesAnswer struct {
	AtlasRead
	Rows       []atlas.Summary `json:"rows"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Total      int             `json:"total"`
}

// AtlasMapAnswer is the atlas drawn as its lanes.
type AtlasMapAnswer struct {
	AtlasRead
	atlas.Map
}

// AtlasEntryAnswer is one entry in full, what its open signals add up to as
// the reader may read them, and what relies on it.
type AtlasEntryAnswer struct {
	AtlasRead
	Entry  atlas.Entry  `json:"entry"`
	Health atlas.Health `json:"health"`
	Vendor atlas.Vendor `json:"vendor,omitempty"`
	UsedBy []atlas.Link `json:"used_by"`
}

// AtlasTraceAnswer is what depends on one entry, or what it depends on.
type AtlasTraceAnswer struct {
	AtlasRead
	atlas.Trace
}

// AtlasFacetsAnswer is how many entries each filter value leaves.
type AtlasFacetsAnswer struct {
	AtlasRead
	atlas.Facets
	Total int `json:"total"`
}

// registerAtlas registers the atlas's questions, each on the atlas app.
func registerAtlas(r *Registry, s Sources) {
	app := Requires(iam.GrantAtlas)
	r.Register("atlas_entries", app, s.atlasEntries)
	r.Register("atlas_map", app, s.atlasMap)
	r.Register("atlas_entry", app, s.atlasEntry)
	r.Register("atlas_trace", app, s.atlasTrace)
	r.Register("atlas_facets", app, s.atlasFacets)
}

// atlasView is the atlas as the question's asker reads it, and what the read
// says of itself.
func (s Sources) atlasView(ctx context.Context, what string) (atlas.View, AtlasRead, error) {
	principal, err := caller(ctx, what)
	if err != nil {
		return atlas.View{}, AtlasRead{}, err
	}
	reads, why := s.atlasGitlet(principal)
	answer, running := s.Atlas.AtlasRead(ctx, reads, why, codeAsker(principal), atlasFresh, s.clock())
	if !running {
		return atlas.View{}, AtlasRead{}, fmt.Errorf("%w: this node runs no company yet, so there is no "+
			"atlas to read", ErrUnavailable)
	}
	read := AtlasRead{Revision: answer.Revision, Coverage: answer.Coverage, Withheld: len(answer.Withheld)}
	return atlas.For(answer.View, atlas.Reader{Admin: principal.Can(iam.GrantAdmin)}), read, nil
}

// atlasGitlet is Gitlet as the asker reads it, or none and why: the atlas
// lists a repository only to a holder of Gitlet (ADR-0110), and only where
// this node serves code.
func (s Sources) atlasGitlet(p iam.Principal) (gather.GitletReads, string) {
	if !p.Can(iam.GrantGitlet) {
		return nil, "you do not use Gitlet, so no repository was read"
	}
	if s.Code == nil {
		return nil, ""
	}
	reads, err := s.Code.CodeReads()
	var absent *NotFound
	switch {
	case errors.As(err, &absent):
		return nil, ""
	case err != nil:
		return nil, "Gitlet could not be read on this node right now: " + err.Error()
	}
	return reads, ""
}

// atlasQuery is the filters a question names.
func atlasQuery(p Params) (atlas.Query, error) {
	unconfirmed, err := atlasFlag(p, "unconfirmed")
	if err != nil {
		return atlas.Query{}, err
	}
	q := atlas.Query{
		Text: p.String("q"), Kind: atlas.Kind(p.String("kind")), Lane: atlas.Lane(p.String("lane")),
		Vendor: atlas.Vendor(p.String("vendor")), Word: atlas.Word(p.String("confidence")),
		Health: atlas.Health(p.String("health")), Show: atlas.Show(p.String("show")),
		Unconfirmed: unconfirmed,
	}
	if err := q.Validate(); err != nil {
		return atlas.Query{}, badAtlas(err)
	}
	return q, nil
}

// atlasFlag is a yes or no parameter, false where it is absent. Anything but
// true or false is refused rather than read as no, so a misspelt flag never
// hides what it asked to show.
func atlasFlag(p Params, key string) (bool, error) {
	if !p.Has(key) {
		return false, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(p.String(key)))
	if err != nil {
		return false, fmt.Errorf("%w: `%s` is true or false", ErrBadParams, key)
	}
	return b, nil
}

// badAtlas is an atlas read's refusal of its parameters, in this surface's
// vocabulary.
func badAtlas(err error) error {
	return fmt.Errorf("%w: %s", ErrBadParams, strings.TrimPrefix(err.Error(), "atlas: bad read: "))
}

// atlasID is the entry a question names by id.
func atlasID(p Params) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(p.String("id")))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: `id` is an atlas entry's id", ErrBadParams)
	}
	return id, nil
}

// noAtlasEntry is an id no entry of the atlas answers to.
func noAtlasEntry() error {
	return &NotFound{Reason: "no_atlas_entry", Detail: "no entry of the atlas has this id: it was never in it, " +
		"or what named it stopped naming it"}
}

func (s Sources) atlasEntries(ctx context.Context, p Params) (any, error) {
	q, err := atlasQuery(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_entries")
	if err != nil {
		return nil, err
	}
	limit := Clamp(p.Int("limit", atlas.PageDefault), atlas.PageDefault, atlas.PageMax)
	page, err := atlas.List(view.Select(q), view.Vendors(), atlas.Sort(p.String("sort")), p.String("cursor"), limit)
	if err != nil {
		return nil, badAtlas(err)
	}
	return AtlasEntriesAnswer{AtlasRead: read, Rows: page.Rows, NextCursor: page.Next, Total: page.Total}, nil
}

func (s Sources) atlasMap(ctx context.Context, p Params) (any, error) {
	q, err := atlasQuery(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_map")
	if err != nil {
		return nil, err
	}
	var closed []string
	for _, key := range strings.Split(p.String("closed"), ",") {
		if key = strings.TrimSpace(key); key != "" {
			closed = append(closed, key)
		}
	}
	return AtlasMapAnswer{AtlasRead: read, Map: view.Map(q, closed)}, nil
}

func (s Sources) atlasEntry(ctx context.Context, p Params) (any, error) {
	id, err := atlasID(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_entry")
	if err != nil {
		return nil, err
	}
	e, found := view.Find(id)
	if !found {
		return nil, noAtlasEntry()
	}
	return AtlasEntryAnswer{AtlasRead: read, Entry: e, Health: e.Health(), Vendor: view.Vendors()[e.ID],
		UsedBy: view.UsedBy(e.ID)}, nil
}

func (s Sources) atlasTrace(ctx context.Context, p Params) (any, error) {
	id, err := atlasID(p)
	if err != nil {
		return nil, err
	}
	direction := atlas.Direction(p.String("direction"))
	if direction == "" {
		direction = atlas.DirectionUp
	}
	all := false
	switch p.String("relations") {
	case "", "structural":
	case "all":
		all = true
	default:
		return nil, fmt.Errorf("%w: `relations` is \"structural\" or \"all\"", ErrBadParams)
	}
	view, read, err := s.atlasView(ctx, "atlas_trace")
	if err != nil {
		return nil, err
	}
	e, found := view.Find(id)
	if !found {
		return nil, noAtlasEntry()
	}
	trace, _, err := view.Trace(e.ID, direction, p.Int("depth", atlas.TraceDepthMax), all)
	if err != nil {
		return nil, badAtlas(err)
	}
	return AtlasTraceAnswer{AtlasRead: read, Trace: trace}, nil
}

func (s Sources) atlasFacets(ctx context.Context, p Params) (any, error) {
	q, err := atlasQuery(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_facets")
	if err != nil {
		return nil, err
	}
	return AtlasFacetsAnswer{AtlasRead: read, Facets: view.Facets(q), Total: len(view.Select(q))}, nil
}
