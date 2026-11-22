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
		rows, _, err := everySavedView(ctx, tx, q.Viewer, pinned, "", nil)
		if err != nil {
			return err
		}
		rows = PinnedFirst(rows)
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

// EveryViewPage is [Reader.EveryView] a page at a time, as a node holding no
// data reads it: the views after `after` (empty for the first) in the
// containers' and strips' order, held to the bytes the read carries
// ([statelog.RowsOf]), and the cursor of the page after, empty on the last.
// Pins are NOT put first: [PinnedFirst] over every page's rows does that.
func (r *Reader) EveryViewPage(ctx context.Context, q EveryViewQuery,
	after string) (ViewListing, string, error) {

	if q.Level == "" {
		return ViewListing{}, "", fmt.Errorf("worklet: this view read names no level")
	}
	counting := ViewQuery{Viewer: q.Viewer, Units: q.Units, Counts: q.Counts,
		Now: q.Now, Zone: q.Zone}
	var listing ViewListing
	var next string
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
		rows, cursor, err := everySavedView(ctx, tx, q.Viewer, pinned, after, statelog.RowsOf(ctx))
		if err != nil {
			return err
		}
		if q.Counts {
			if err = countPinned(ctx, tx, counting, rows); err != nil {
				return err
			}
		}
		listing.Views, next = rows, cursor
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		listing.LogSeq, listing.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return ViewListing{}, "", err
	}
	listing.Level, listing.Complete, listing.LogLag = served.Level, served.Complete, served.Lag
	if served.Incomplete != nil {
		listing.Incomplete = incompleteFrom(served.Incomplete)
	}
	return listing, next, nil
}

// PinnedFirst is rows with those pinned for the viewer first, each half in
// its own order: [Reader.EveryView]'s order over rows in the containers'.
func PinnedFirst(rows []ViewRow) []ViewRow {
	out := make([]ViewRow, 0, len(rows))
	for _, pinned := range []bool{true, false} {
		for _, row := range rows {
			if row.Pinned == pinned {
				out = append(out, row)
			}
		}
	}
	return out
}

// viewCursor is where a page of every saved view ends: its last row's place
// in the containers' and strips' order.
type viewCursor struct {
	Order     int    `json:"o"`
	Container string `json:"c"`
	Rank      Rank   `json:"r"`
	Name      string `json:"n"`
	ID        string `json:"i"`
}

// everySavedView reads every view shared with the company or owned by the
// viewer, in every container, in the containers' and strips' order, after
// the cursor `after` and up to the bytes held keeps (every row for nil), and
// the cursor of the rest where held refused one.
//
// A DELETED VIEW IS LEFT OUT, as [savedViews] leaves it out.
//
// THE ORDER IS THE CONTAINERS' AND THEN THE STRIP'S: workspace, projects,
// units, people, each by id, and by rank within one container, as its own
// strip draws them. Pinned and unpinned are TWO ORDERS CONCATENATED, as
// [savedViews] does, so a pin re-ranks nothing.
func everySavedView(ctx context.Context, tx *sql.Tx, viewer string,
	pinned map[string]bool, after string, held *statelog.RowBytes) ([]ViewRow, string, error) {

	// THE SHARED HALF IS `owner = ''`, and the personal half the viewer's
	// own name — [savedViews]' rule, over every container.
	own := ""
	var args []any
	if viewer != "" {
		own = " OR owner = ?"
		args = append(args, viewer)
	}
	resume := ""
	if after != "" {
		var c viewCursor
		if err := json.Unmarshal([]byte(after), &c); err != nil {
			return nil, "", fmt.Errorf("%w: the cursor %q is not one a page answered", ErrInvalid, after)
		}
		resume = ` AND (` + viewOrder + `, container_id, rank, name, id) > (?, ?, ?, ?, ?)`
		args = append(args, c.Order, c.Container, string(c.Rank), c.Name, c.ID)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, type, owner, protected, is_default, rank, icon,
		       params_json, container_kind, container_id, `+viewOrder+`
		FROM tracker_views
		WHERE (owner = ''`+own+`) AND deleted_at IS NULL`+resume+`
		ORDER BY `+viewOrder+`, container_id, rank, name, id`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("worklet: read every saved view: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ViewRow
	var next string
	last := viewCursor{}
	for rows.Next() {
		var row ViewRow
		var protected, isDefault, order int
		var params []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.Type, &row.Owner,
			&protected, &isDefault, &row.Rank, &row.Icon, &params,
			&row.Container.Kind, &row.Container.ID, &order); err != nil {
			return nil, "", fmt.Errorf("worklet: scan a saved view: %w", err)
		}
		row.Key = row.ID
		row.Protected, row.Default = protected == 1, isDefault == 1
		if len(params) > 0 {
			if err := json.Unmarshal(params, &row.Params); err != nil {
				return nil, "", fmt.Errorf("worklet: decode view %s's params: %w",
					row.ID, err)
			}
		}
		row.Pinned = pinned[row.ID]
		if held != nil && !held.TakeEncoded(row) {
			cursor, err := json.Marshal(last)
			if err != nil {
				return nil, "", err
			}
			next = string(cursor)
			break
		}
		out = append(out, row)
		last = viewCursor{Order: order, Container: row.Container.ID, Rank: row.Rank, Name: row.Name, ID: row.ID}
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("worklet: read every saved view: %w", err)
	}
	return out, next, nil
}

// viewOrder is the containers' order every saved view is read in: the
// workspace, projects, units, then people.
const viewOrder = `CASE container_kind
		           WHEN '` + ContainerWorkspace + `' THEN 0
		           WHEN '` + ContainerProject + `' THEN 1
		           WHEN '` + ContainerUnit + `' THEN 2
		           ELSE 3 END`
