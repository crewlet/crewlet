package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading a container's view strip.
//
// # Why three views exist without anybody saving one
//
// Every project and the workspace HAS a list, a board and a calendar, and none
// of them is an object. Making all three implicit is what makes "required
// views" moot: a container can never be left without a way to look at it, a
// fresh project needs no setup gesture, and nothing has to guard against
// somebody deleting the last view.
//
// # The order is the person's, and it is two orders
//
// Saved views carry a [Rank], which is the strip's own arrangement and is
// shared. A person's PINS are theirs, and a pinned view comes first for THEM
// and nobody else — which is why the viewer is a parameter of the read rather
// than a property of the row.

// ViewRow is one entry in a container's strip.
type ViewRow struct {
	// ID is empty for an implicit view: it is not an object, so there is
	// nothing to save, protect, rank or pin. A caller renders it from
	// [ViewRow.Key] instead.
	ID   string   `json:"id,omitempty"`
	Key  string   `json:"key"`
	Name string   `json:"name"`
	Type ViewType `json:"type"`

	Container Container `json:"container"`

	// Builtin marks the implicit views, which cannot be edited or deleted
	// and are the same on every node without anybody having written one.
	Builtin bool `json:"builtin"`

	// Owner empty is a SHARED view; a handle makes it personal. Protected
	// says only its owner may change it.
	Owner     string `json:"owner,omitempty"`
	Protected bool   `json:"protected,omitempty"`

	// Default is the container's landing tab, and at most one row carries
	// it — the applier settles that in the same transaction as the write.
	Default bool `json:"default,omitempty"`

	// Pinned is THIS VIEWER's, never the row's: it comes from the person's
	// own record, so the same view is pinned for one reader and not for
	// another.
	Pinned bool `json:"pinned,omitempty"`

	Rank   Rank              `json:"rank,omitempty"`
	Icon   string            `json:"icon,omitempty"`
	Params map[string]string `json:"params,omitempty"`
}

// ViewQuery asks for one container's strip.
type ViewQuery struct {
	Container Container

	// Viewer is whose pins order the saved half, and whose personal views
	// join it. An UNNAMED party asks for the shared strip: no pins, and no
	// personal views but the shared ones.
	//
	// A [Party] RATHER THAN A HANDLE for the reason [MyWorkQuery.Who] is
	// one — a saved view and a pin are both written by the person's own
	// credential through the operator tool server, so they are OWNED by
	// the token's name while the strip is asked for under the seat's.
	Viewer Party

	// Units resolves a UNIT container's two spellings, so a strip asked
	// for by a team's id carries the views saved against its name and the
	// other way round — see [Units] and [CanonicalContainer]. Nil matches
	// the container as asked, which is the honest answer for a surface
	// holding no chart.
	Units Units

	Level statelog.ReadLevel

	// Session is the caller's own high-water mark, MinPosition an explicit
	// floor it names, and MaxLag what a stale read says it will accept —
	// the three the framework's own query takes, carried through unchanged
	// so this read makes no freshness decision of its own.
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// ViewListing is the strip, with the framework's own verdict on the read.
type ViewListing struct {
	Views []ViewRow `json:"views"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// The implicit views' keys. STABLE STRINGS rather than positions, because a
// caller stores which tab it was on and a positional key would move the
// person's tab whenever the set changed.
const (
	ViewKeyList     = "list"
	ViewKeyBoard    = "board"
	ViewKeyCalendar = "calendar"
	ViewKeyTimeline = "timeline"
	ViewKeyTable    = "table"
	ViewKeyTrash    = "trash"
)

// Views answers one container's strip.
//
// ONE READ TRANSACTION for the saved rows and the viewer's pins — so the strip
// describes one instant rather than two reads' worth of them.
func (r *Reader) Views(ctx context.Context, q ViewQuery) (ViewListing, error) {
	if q.Level == "" {
		return ViewListing{}, fmt.Errorf("tracker: this view read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	// THE READ REFUSES THE SPELLINGS THE WRITE REFUSES. A strip asked for
	// under a container the write could never have produced comes back
	// empty, which reads exactly like a container nobody has saved a view
	// in — so the mismatch is named here rather than rendered as nothing.
	switch {
	case !ValidContainerKind(q.Container.Kind):
		return ViewListing{}, fmt.Errorf("tracker: %q is not a container a "+
			"view strip belongs to — the four are %s, %s, %s and %s",
			q.Container.Kind, ContainerWorkspace, ContainerProject,
			ContainerUnit, ContainerPerson)
	case q.Container.Kind == ContainerWorkspace && q.Container.ID != "":
		return ViewListing{}, fmt.Errorf("tracker: a view read names workspace "+
			"container %q — the workspace is the top of the company and "+
			"carries no id", q.Container.ID)
	case q.Container.Kind != ContainerWorkspace && q.Container.ID == "":
		return ViewListing{}, fmt.Errorf("tracker: a view read names a %s "+
			"container with no id — name the project, unit or person whose "+
			"strip this is", q.Container.Kind)
	}

	var listing ViewListing
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       viewScope(q.Container),
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
		implicit := implicitViews(q.Container)
		saved, err := savedViews(ctx, tx, q.Container, q.Units, q.Viewer, pinned)
		if err != nil {
			return err
		}
		// THE IMPLICIT ONES FIRST, then what somebody saved. Built into
		// its own slice rather than appended onto `implicit`: appending
		// to one slice and storing the result in another writes through
		// whatever spare capacity the first has, which is a bug the day
		// somebody reads `implicit` after this line.
		listing.Views = make([]ViewRow, 0, len(implicit)+len(saved))
		listing.Views = append(listing.Views, implicit...)
		listing.Views = append(listing.Views, saved...)

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

// viewScope is the read's closure: the container the strip belongs to.
//
// It is [containerScope]'s own rule and not a second copy of it, because a
// read's closure and a write's scope are compared path against path: two
// spellings of one container would make a deferred view record invisible to
// the read that wants it, and such a read reports itself complete while
// missing the write.
func viewScope(container Container) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermContainer, ID: containerScope(container)}.Path(),
	}}.Normalised()
}

// implicitViews is the six every container has.
func implicitViews(container Container) []ViewRow {
	rows := []ViewRow{
		{Key: ViewKeyList, Name: "List", Type: ViewList, Container: container, Builtin: true},
		{Key: ViewKeyBoard, Name: "Board", Type: ViewBoard, Container: container, Builtin: true},
		{Key: ViewKeyCalendar, Name: "Calendar", Type: ViewCalendar, Container: container, Builtin: true},
		// THE TIMELINE SORTS BY START, which is the arrangement its axis
		// already has: a bar chart down a date axis whose rows arrive in
		// rank order draws a staircase nobody can read, and re-sorting in
		// the client would make the ORDER of a page depend on which rows
		// the page happened to contain.
		{Key: ViewKeyTimeline, Name: "Timeline", Type: ViewTimeline,
			Container: container, Builtin: true,
			Params: map[string]string{"sort": "start"}},
		{Key: ViewKeyTable, Name: "Table", Type: ViewTable, Container: container, Builtin: true},
		// THE TRASH IS A QUERY, NOT A RENDERING, which is why it is a
		// builtin VIEW of the table shape rather than a sixth [ViewType]:
		// what makes it the trash is `removed=true`, and a renderer keyed
		// on the TYPE would then have a shape whose meaning depended on a
		// parameter it could be saved without. Any view carrying that
		// parameter is a trash listing and a client may treat it as one.
		//
		// `show_closed` travels with it because a removed task is very
		// often a finished one, and the group predicate is ANDed
		// unconditionally otherwise (see [Query.ShowClosed]): without it
		// the one tab whose whole job is "what did my assistant delete"
		// hides every deletion of anything already done.
		//
		// NO `sort`, deliberately, unlike the timeline: [sortTerms]
		// already orders a removed listing by `removed_at DESC`, which is
		// the one order this tab wants and the only index over these rows.
		// Naming it here would be a second copy of that decision, and the
		// two would drift.
		{Key: ViewKeyTrash, Name: "Trash", Type: ViewTable,
			Container: container, Builtin: true,
			Params: map[string]string{"removed": "true", "show_closed": "true"}},
	}
	return rows
}

// savedViews reads the container's own views, ordered pinned-for-this-viewer
// first and then by rank.
//
// A PERSONAL VIEW IS PRIVATE TO ITS OWNER, which is enforced HERE rather than
// by a scheme: a row whose owner is set and is not the viewer is not in the
// answer at all. Filtering in SQL rather than after the fact is what stops a
// personal view riding a page boundary into somebody else's strip.
func savedViews(ctx context.Context, tx *sql.Tx, container Container,
	units Units, viewer Party, pinned map[string]bool) ([]ViewRow, error) {

	// A UNIT CONTAINER MATCHES BOTH OF ITS TEAM'S SPELLINGS, which is the
	// same rule a `unit=` filter follows and for the same reason: a strip
	// saved before the team had an id is addressed by its name, and one
	// saved after it by the id. Every other kind addresses itself one way
	// and [unitSpellings] hands that one back.
	ids := []string{container.ID}
	if container.Kind == ContainerUnit {
		if spellings := unitSpellings(units, ids); len(spellings) > 0 {
			ids = spellings
		}
	}
	// THE SHARED HALF IS `owner = ''`, and it answers for a party that
	// names nobody — which is what an anonymous strip is. The personal
	// half matches EVERY name this person saves under, because a view
	// saved through their own credential is owned by the token's id while
	// the strip is asked for under their seat's.
	own := ""
	args := []any{container.Kind}
	args = append(args, anyOf(ids)...)
	if owners := viewer.args(); len(owners) > 0 {
		own = " OR owner IN (" + placeholders(len(owners)) + ")"
		args = append(args, owners...)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, type, owner, protected, is_default, rank, icon,
		       params_json
		FROM tracker_views
		WHERE container_kind = ? AND container_id IN (`+placeholders(len(ids))+`)
		  AND (owner = ''`+own+`)
		ORDER BY rank, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("tracker: read %s %s's views: %w",
			container.Kind, container.ID, err)
	}
	defer func() { _ = rows.Close() }()

	var shared, first []ViewRow
	for rows.Next() {
		var row ViewRow
		var protected, isDefault int
		var params []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.Type, &row.Owner,
			&protected, &isDefault, &row.Rank, &row.Icon, &params); err != nil {
			return nil, fmt.Errorf("tracker: scan a view of %s %s: %w",
				container.Kind, container.ID, err)
		}
		row.Container = container
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
		return nil, fmt.Errorf("tracker: read %s %s's views: %w",
			container.Kind, container.ID, err)
	}
	// TWO ORDERS, CONCATENATED rather than sorted together: within each
	// half the rank is the arrangement somebody made, and a single sort
	// over a pinned flag would silently re-rank the shared strip for one
	// reader.
	return append(first, shared...), nil
}

// pinnedViews is the viewer's own pins, and empty for an anonymous read.
//
// GATED ON THE IDENTITY LIST, which is what [savedViews] matches on: the two
// halves of one strip must agree about whether there is a viewer at all, or a
// party naming only a credential would have its views and not its pins.
func pinnedViews(ctx context.Context, tx *sql.Tx, viewer Party) (map[string]bool, error) {
	if len(viewer.Handles()) == 0 {
		return nil, nil
	}
	person, held, err := readPartyRecord(ctx, tx, viewer)
	if err != nil || !held {
		return nil, err
	}
	pinned := make(map[string]bool, len(person.PinnedViews))
	for _, id := range person.PinnedViews {
		pinned[id] = true
	}
	return pinned, nil
}
