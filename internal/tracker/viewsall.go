package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading every saved view a person can see, across every container.
//
// # Why a strip read is not enough
//
// [Reader.Views] answers ONE container's strip — the tabs over one board — and
// that is the right shape for a board. It is the wrong shape for the two
// surfaces that are about a PERSON's views rather than a container's: the
// inventory of everything somebody saved, and the sidebar's pinned group. Both
// asked the workspace strip, so a view saved with a project board's "+ View"
// (container `project:ENG`) was in no inventory and no sidebar — it could not
// be pinned from the screen that offers pins, and a pin set another way never
// reached the sidebar that draws them. The strip read cannot be widened into
// this one without changing what a board's tabs are, so it is a sibling.
//
// # The same rules as a strip
//
// A personal view is its owner's alone, filtered in SQL exactly as
// [savedViews] filters it; a pin is the viewer's own; a pinned view's count is
// [countView]'s — the view run in ITS OWN container, which is what opening it
// runs — so a sidebar number agrees with the list it opens whichever
// container the view lives in. The builtins are not here: they are a
// container's own tabs, carry no id, and there is nothing to pin.

// EveryViewQuery asks for every saved view [EveryViewQuery.Viewer] can see.
//
// The freshness fields are [ViewQuery]'s, carried through unchanged so this
// read makes no freshness decision of its own.
type EveryViewQuery struct {
	// Viewer is whose personal views join the shared ones and whose pins
	// come first. Empty reads the shared views alone.
	Viewer string
	// Units resolves a unit's two spellings when a pinned unit view is
	// counted — see [ViewQuery.Units].
	Units Units

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64

	// Counts asks for [ViewRow.Count] on every row pinned for Viewer, as
	// [ViewQuery.Counts] does: at most [MaxPinnedViews] counts, since a
	// person's pins are bounded there whichever containers they are in.
	Counts bool
	Now    time.Time
	Zone   *time.Location
}

// EveryView answers every saved view the viewer can see, pinned-for-them
// first, then by container (the workspace, projects, units, people) and within
// a container by the strip's own rank.
//
// ONE READ TRANSACTION for the rows, the pins and the counts, like a strip.
// Its closure is the whole domain: the rows span every container and a
// count's own closure is not known until its row is read.
func (r *Reader) EveryView(ctx context.Context, q EveryViewQuery) (ViewListing, error) {
	if q.Level == "" {
		return ViewListing{}, fmt.Errorf("tracker: this view read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if q.Counts {
		switch {
		case q.Viewer == "":
			return ViewListing{}, fmt.Errorf("tracker: a view read asks for " +
				"its pinned views' counts and names no viewer — pins are a " +
				"person's, so a read nobody is looking at has none to count")
		case q.Now.IsZero() || q.Zone == nil:
			return ViewListing{}, fmt.Errorf("tracker: a view read asks for " +
				"counts with no instant or no clock — a surface passes the " +
				"company's own, which every relative date in a view is cut on")
		}
	}
	// THE COUNT'S OWN QUERY, which [countView] reads the viewer, the units
	// and the clock from. The container is each row's, never this.
	counting := ViewQuery{Viewer: q.Viewer, Units: q.Units, Counts: q.Counts,
		Now: q.Now, Zone: q.Zone}

	var listing ViewListing
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		pinned, err := pinnedViews(ctx, tx, q.Viewer)
		if err != nil {
			return err
		}
		rows, err := everySavedView(ctx, tx, q.Viewer, pinned)
		if err != nil {
			return err
		}
		if q.Counts {
			if err = countPinned(ctx, tx, counting, rows); err != nil {
				return err
			}
		}
		listing.Views = rows
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		listing.LogSeq, listing.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return ViewListing{}, err
	}
	listing.Level = served.Level
	listing.Complete = served.Complete
	listing.LogLag = served.Lag
	if served.Incomplete != nil {
		listing.Incomplete = incompleteFrom(served.Incomplete)
	}
	return listing, nil
}

// everySavedView reads every view shared with the company or owned by the
// viewer, in every container, pinned-for-the-viewer first.
//
// THE ORDER IS THE CONTAINERS' AND THEN THE STRIP'S: the workspace first, then
// projects, units and people, each by its id, and inside one container by
// rank — so a container's views read in the order its own strip draws them.
// TWO ORDERS CONCATENATED, as [savedViews] does, so a pin re-ranks nothing.
func everySavedView(ctx context.Context, tx *sql.Tx, viewer string,
	pinned map[string]bool) ([]ViewRow, error) {

	// THE SHARED HALF IS `owner = ''`, and the personal half the viewer's
	// own name — [savedViews]' rule, over every container.
	own := ""
	var args []any
	if viewer != "" {
		own = " OR owner = ?"
		args = append(args, viewer)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, type, owner, protected, is_default, rank, icon,
		       params_json, container_kind, container_id
		FROM tracker_views
		WHERE (owner = ''`+own+`)
		ORDER BY CASE container_kind
		           WHEN '`+ContainerWorkspace+`' THEN 0
		           WHEN '`+ContainerProject+`' THEN 1
		           WHEN '`+ContainerUnit+`' THEN 2
		           ELSE 3 END,
		         container_id, rank, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("tracker: read every saved view: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var shared, first []ViewRow
	for rows.Next() {
		var row ViewRow
		var protected, isDefault int
		var params []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.Type, &row.Owner,
			&protected, &isDefault, &row.Rank, &row.Icon, &params,
			&row.Container.Kind, &row.Container.ID); err != nil {
			return nil, fmt.Errorf("tracker: scan a saved view: %w", err)
		}
		row.Key = row.ID
		row.Protected, row.Default = protected == 1, isDefault == 1
		if len(params) > 0 {
			if err := json.Unmarshal(params, &row.Params); err != nil {
				return nil, fmt.Errorf("tracker: decode view %s's params: %w",
					row.ID, err)
			}
		}
		if row.Pinned = pinned[row.ID]; row.Pinned {
			first = append(first, row)
			continue
		}
		shared = append(shared, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read every saved view: %w", err)
	}
	return append(first, shared...), nil
}
