package worklet

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
// [Reader.Views] answers one container's strip, the tabs over one board. The
// inventory of somebody's views and the sidebar's pinned group are about a
// PERSON, so a view saved on a project board (container `project:ENG`) must
// reach both; widening the strip read would change what a board's tabs are.
//
// The rules are a strip's: a personal view is its owner's alone, filtered in
// SQL as [savedViews] does; a pin is the viewer's own; a pinned view's count is
// [countView]'s, run in the view's own container as opening it is, so a
// sidebar number agrees with the list it opens. Builtins are a container's own
// tabs with no id, so there is nothing to pin and they are left out.

// EveryViewQuery asks for every saved view [EveryViewQuery.Viewer] can see.
//
// The freshness fields are [ViewQuery]'s, carried through unchanged so this
// read makes no freshness decision of its own.
type EveryViewQuery struct {
	// Viewer is whose personal views join the shared ones and whose pins
	// come first. Empty reads the shared views alone.
	Viewer string
	// Units resolves a unit's name or key to the key a save stores when a
	// pinned unit view is counted — see [ViewQuery.Units].
	Units Units `json:"-"`

	Level     statelog.ReadLevel
	Session   statelog.Position
	Floors    []statelog.Position
	MaxLag    time.Duration
	MaxLagSeq uint64

	// Counts asks for [ViewRow.Count] on every row pinned for Viewer, as
	// [ViewQuery.Counts] does: at most [MaxPinnedViews] counts, since a
	// person's pins are bounded there whichever containers they are in.
	Counts bool
	Now    time.Time
	Zone   *time.Location `json:"-"`
}

// EveryView answers every saved view the viewer can see, pinned-for-them
// first, then by container (the workspace, projects, units, people) and within
// a container by the strip's own rank.
//
// ONE READ TRANSACTION for the rows, pins and counts, like a strip; its
// closure is the whole domain, since the rows span every container and a
// count's own closure is known only once its row is read.
func (r *Reader) EveryView(ctx context.Context, q EveryViewQuery) (ViewListing, error) {
	if q.Level == "" {
		return ViewListing{}, fmt.Errorf("worklet: this view read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if q.Counts {
		switch {
		case q.Viewer == "":
			return ViewListing{}, fmt.Errorf("worklet: a view read asks for " +
				"its pinned views' counts and names no viewer — pins are a " +
				"person's, so a read nobody is looking at has none to count")
		case q.Now.IsZero() || q.Zone == nil:
			return ViewListing{}, fmt.Errorf("worklet: a view read asks for " +
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
		Level:     q.Level,
		Scope:     statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
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
// A DELETED VIEW IS LEFT OUT, as [savedViews] leaves it out.
//
// THE ORDER IS THE CONTAINERS' AND THEN THE STRIP'S: workspace, projects,
// units, people, each by id, and by rank within one container, as its own
// strip draws them. Pinned and unpinned are TWO ORDERS CONCATENATED, as
// [savedViews] does, so a pin re-ranks nothing.
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
		WHERE (owner = ''`+own+`) AND deleted_at IS NULL
		ORDER BY CASE container_kind
		           WHEN '`+ContainerWorkspace+`' THEN 0
		           WHEN '`+ContainerProject+`' THEN 1
		           WHEN '`+ContainerUnit+`' THEN 2
		           ELSE 3 END,
		         container_id, rank, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read every saved view: %w", err)
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
			return nil, fmt.Errorf("worklet: scan a saved view: %w", err)
		}
		row.Key = row.ID
		row.Protected, row.Default = protected == 1, isDefault == 1
		if len(params) > 0 {
			if err := json.Unmarshal(params, &row.Params); err != nil {
				return nil, fmt.Errorf("worklet: decode view %s's params: %w",
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
		return nil, fmt.Errorf("worklet: read every saved view: %w", err)
	}
	return append(first, shared...), nil
}
