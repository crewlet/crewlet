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
// A project running sprints adds two more — the sprint board and the backlog —
// because those are the two questions a sprint creates and neither is
// expressible as a default. They appear only where the project's policy says
// it runs sprints, so a company that does not use them never sees them.
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

	// Viewer is whose pins order the saved half. Empty asks for the
	// shared strip: no pins, and no personal views but the shared ones.
	Viewer string

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
// person's tab when a sprint policy is turned on.
const (
	ViewKeyList     = "list"
	ViewKeyBoard    = "board"
	ViewKeyCalendar = "calendar"
	ViewKeySprint   = "sprint"
	ViewKeyBacklog  = "backlog"
)

// Views answers one container's strip.
//
// ONE READ TRANSACTION for the saved rows, the viewer's pins and the project's
// sprint policy — so the strip describes one instant. Assembled across three
// reads it could show a sprint board for a policy that had just been turned
// off, which is a tab that opens on nothing.
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
		Level:           q.Level,
		Scope:           viewScope(q.Container),
		Session:         q.Session,
		MinPosition:     q.MinPosition,
		MaxLag:          q.MaxLag,
		MaxLagPositions: q.MaxLagSeq,
		Set:             true,
	}, func(tx *sql.Tx) error {
		pinned, err := pinnedViews(ctx, tx, q.Viewer)
		if err != nil {
			return err
		}
		implicit, err := implicitViews(ctx, tx, q.Container)
		if err != nil {
			return err
		}
		saved, err := savedViews(ctx, tx, q.Container, q.Viewer, pinned)
		if err != nil {
			return err
		}
		listing.Views = append(implicit, saved...)

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

// implicitViews is the three every container has, plus the two a sprinting
// project adds.
func implicitViews(ctx context.Context, tx *sql.Tx, container Container) ([]ViewRow, error) {
	rows := []ViewRow{
		{Key: ViewKeyList, Name: "List", Type: ViewList, Container: container, Builtin: true},
		{Key: ViewKeyBoard, Name: "Board", Type: ViewBoard, Container: container, Builtin: true},
		{Key: ViewKeyCalendar, Name: "Calendar", Type: ViewCalendar, Container: container, Builtin: true},
	}
	if container.Kind != ContainerProject || container.ID == "" {
		return rows, nil
	}
	// THE TWO A SPRINT POLICY ADDS, and only where there is one: a company
	// that does not run sprints never meets a tab that opens on nothing.
	project, held, err := readProject(ctx, tx, container.ID)
	if err != nil || !held || project.Sprints == nil {
		return rows, err
	}
	return append(rows,
		ViewRow{Key: ViewKeySprint, Name: "Sprint", Type: ViewBoard,
			Container: container, Builtin: true,
			Params: map[string]string{"sprint": "active"}},
		ViewRow{Key: ViewKeyBacklog, Name: "Backlog", Type: ViewList,
			Container: container, Builtin: true,
			// THE BACKLOG IS UNFINISHED WORK IN NO SPRINT, ordered by
			// the board's own arrangement. `sprint=none` is an absence
			// of an OPEN stay rather than of every stay, which is what
			// keeps a carry-over out of it: a task pulled forward into
			// the next sprint is in that sprint, not back in the pile
			// somebody is planning from.
			Params: map[string]string{
				"status_group": string(GroupNotStarted) + "," + string(GroupActive),
				"sprint":       "none",
				"sort":         "rank",
			}},
	), nil
}

// savedViews reads the container's own views, ordered pinned-for-this-viewer
// first and then by rank.
//
// A PERSONAL VIEW IS PRIVATE TO ITS OWNER, which is enforced HERE rather than
// by a scheme: a row whose owner is set and is not the viewer is not in the
// answer at all. Filtering in SQL rather than after the fact is what stops a
// personal view riding a page boundary into somebody else's strip.
func savedViews(ctx context.Context, tx *sql.Tx, container Container,
	viewer string, pinned map[string]bool) ([]ViewRow, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, type, owner, protected, is_default, rank, icon,
		       params_json
		FROM tracker_views
		WHERE container_kind = ? AND container_id = ?
		  AND (owner = '' OR owner = ?)
		ORDER BY rank, name`,
		container.Kind, container.ID, viewer)
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
func pinnedViews(ctx context.Context, tx *sql.Tx, viewer string) (map[string]bool, error) {
	if viewer == "" {
		return nil, nil
	}
	person, held, err := readPerson(ctx, tx, viewer)
	if err != nil || !held {
		return nil, err
	}
	pinned := make(map[string]bool, len(person.PinnedViews))
	for _, id := range person.PinnedViews {
		pinned[id] = true
	}
	return pinned, nil
}
