package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading a container's view strip.
//
// Every project and the workspace HAS its implicit views, none of them an
// object: a container is never left without a way to look at it, a fresh
// project needs no setup, and nothing guards deleting the last view.
//
// Saved views carry a shared [Rank], the strip's own arrangement. A person's
// PINS are theirs and come first for them alone, which is why the viewer is a
// parameter of the read rather than a property of the row.

// ViewRow is one entry in a container's strip.
type ViewRow struct {
	// ID is empty for an implicit view, which is not an object: nothing to
	// save, protect, rank or pin. A caller renders it from [ViewRow.Key].
	ID   string   `json:"id,omitempty"`
	Key  string   `json:"key"`
	Name string   `json:"name" untrusted:"name"`
	Type ViewType `json:"type"`

	Container Container `json:"container"`

	// Builtin marks the implicit views: never edited or deleted, and the
	// same on every node without anybody writing one.
	Builtin bool `json:"builtin"`

	// Owner empty is a SHARED view; a handle makes it personal. Protected
	// says only its owner may change it.
	Owner     string `json:"owner,omitempty"`
	Protected bool   `json:"protected,omitempty"`

	// Default is the container's landing tab, and at most one row carries
	// it — the applier settles that in the same transaction as the write.
	Default bool `json:"default,omitempty"`

	// Pinned is THIS VIEWER's, from their own person record, never the
	// row's.
	Pinned bool `json:"pinned,omitempty"`

	Rank   Rank              `json:"rank,omitempty"`
	Icon   string            `json:"icon,omitempty"`
	Params map[string]string `json:"params,omitempty" untrusted:"name"`

	// Count is how many tasks the view selects when RUN (the `total_hint`
	// of a `work_items{view, container}` read with the viewer's `me`),
	// carried only on a row pinned for this viewer when [ViewQuery.Counts]
	// asked. A pointer: absent is "not counted", never zero, and a pinned
	// view selecting nothing most needs its count drawn.
	//
	// CountCapped says the count stopped at [TotalHintCeiling], for
	// [Answer.TotalCapped]'s reason.
	Count       *int `json:"count,omitempty"`
	CountCapped bool `json:"count_capped,omitempty"`

	// CountRefused is WHY a pinned row asked for a count carries none: the
	// view no longer compiles (a custom field it filters on was archived,
	// say). Named, because an absent count on a pinned row reads as "not
	// asked".
	CountRefused string `json:"count_refused,omitempty"`
}

// ViewQuery asks for one container's strip.
type ViewQuery struct {
	Container Container

	// Viewer is whose pins order the saved half and whose personal views
	// join it. Empty asks for the shared strip: no pins, no personal views.
	Viewer string

	// Units resolves a UNIT container to its team's KEY, what a save stores
	// ([CanonicalContainer]), so the team's name or key finds the same
	// views. Nil matches the container as asked, honest for a surface
	// holding no chart.
	//
	// NOT SERIALISED: JSON writes an interface as `{}` and cannot decode
	// it back; the far side of a wire attaches its own chart
	// (internal/estate).
	Units Units `json:"-"`

	Level statelog.ReadLevel

	// Session is the caller's high-water mark, Floors the floors it names,
	// and MaxLag what a stale read accepts: the framework's own query
	// inputs, passed through so this read makes no freshness decision.
	Session statelog.Position
	Floors  []statelog.Position
	MaxLag  time.Duration

	// MaxLagSeq is the same bound in RECORDS, what the broker answers;
	// MaxLag derives from it through this node's drain rate. With both
	// set the read refuses at whichever is reached first.
	MaxLagSeq uint64

	// Counts asks for [ViewRow.Count] on every row pinned for [Viewer], for
	// a sidebar. It needs a named viewer, since only a viewer has pins, and
	// costs one capped count per pin: at most [MaxPinnedViews], the bound
	// that makes it safe on a poll.
	//
	// Now and Zone are the instant and the company's clock relative dates
	// are cut on ([ParseQuery]'s pair), so a pinned "due this week" counts
	// the week its board draws.
	Counts bool
	Now    time.Time
	Zone   *time.Location `json:"-"`
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

// The implicit views' keys: STABLE STRINGS, because a caller stores which tab
// it was on and a positional key would move it whenever the set changed.
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
// ONE READ TRANSACTION for the saved rows and the viewer's pins, so the strip
// describes one instant.
func (r *Reader) Views(ctx context.Context, q ViewQuery) (ViewListing, error) {
	if q.Level == "" {
		return ViewListing{}, fmt.Errorf("worklet: this view read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	// THE READ REFUSES THE SPELLINGS THE WRITE REFUSES: a strip under a
	// container no write could produce comes back empty, indistinguishable
	// from one with no saved views, so the mismatch is named instead.
	switch {
	case !ValidContainerKind(q.Container.Kind):
		return ViewListing{}, fmt.Errorf("worklet: %q is not a container a "+
			"view strip belongs to — the four are %s, %s, %s and %s",
			q.Container.Kind, ContainerWorkspace, ContainerProject,
			ContainerUnit, ContainerPerson)
	case q.Container.Kind == ContainerWorkspace && q.Container.ID != "":
		return ViewListing{}, fmt.Errorf("worklet: a view read names workspace "+
			"container %q — the workspace is the top of the company and "+
			"carries no id", q.Container.ID)
	case q.Container.Kind != ContainerWorkspace && q.Container.ID == "":
		return ViewListing{}, fmt.Errorf("worklet: a view read names a %s "+
			"container with no id — name the project, unit or person whose "+
			"strip this is", q.Container.Kind)
	}

	if q.Counts {
		switch {
		case q.Viewer == "":
			return ViewListing{}, fmt.Errorf("worklet: a view read asks for " +
				"its pinned views' counts and names no viewer — pins are a " +
				"person's, so a strip nobody is looking at has none to count")
		case q.Now.IsZero() || q.Zone == nil:
			return ViewListing{}, fmt.Errorf("worklet: a view read asks for " +
				"counts with no instant or no clock — a surface passes the " +
				"company's own, which every relative date in a view is cut on")
		}
	}

	var listing ViewListing
	served, err := r.log.Read(ctx, statelog.Query{
		Level:     q.Level,
		Scope:     stripScope(q),
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
		implicit := implicitViews(q.Container)
		saved, err := savedViews(ctx, tx, q.Container, q.Units, q.Viewer, pinned)
		if err != nil {
			return err
		}
		// THE IMPLICIT ONES FIRST, then the saved, in a fresh slice:
		// appending onto `implicit` would write through its spare capacity.
		if q.Counts {
			if err = countPinned(ctx, tx, q, saved); err != nil {
				return err
			}
		}
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

// stripScope is the closure of one strip read, WIDER than the container
// whenever the read looks past the container's own rows:
//
//   - A NAMED VIEWER adds the person family: the pins are read from their
//     person record in this transaction, and without it a node holding a
//     deferred pins record would order the strip by stale pins and call it
//     complete.
//   - COUNTS widen it to the domain: a pinned view's count is a task query
//     whose closure is unknown until the view's row is read, after this scope
//     is declared, and the domain holds every such query.
func stripScope(q ViewQuery) statelog.ScopeSet {
	if q.Counts {
		return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
	}
	scope := viewScope(q.Container)
	if q.Viewer != "" {
		scope.Paths = append(scope.Paths, personScope().Paths...)
	}
	return scope.Normalised()
}

// countPinned fills [ViewRow.Count] on every row pinned for the viewer, inside
// the strip's own transaction.
//
// THE COUNT IS THE VIEW'S OWN TOTAL: the rows `work_items` answers when the
// view is run ([countView]), counted by [countHint], the statement behind that
// read's `total_hint`. A second definition of what a view selects would let a
// sidebar number disagree with the list it opens.
func countPinned(ctx context.Context, tx *sql.Tx,
	q ViewQuery, rows []ViewRow) error {

	for i := range rows {
		if !rows[i].Pinned {
			continue
		}
		count, capped, err := countView(ctx, tx, q, rows[i])
		switch {
		case errors.Is(err, errUncountable):
			rows[i].CountRefused = err.Error()
		case err != nil:
			return err
		default:
			rows[i].Count, rows[i].CountCapped = &count, capped
		}
	}
	return nil
}

// errUncountable marks a count that failed because of the VIEW rather than the
// store — see [countView].
var errUncountable = errors.New("this view cannot be counted")

// countView counts one view, or says why it cannot be counted.
//
// A VIEW THAT NO LONGER COMPILES is a refusal on its row, wrapped in
// [errUncountable], never a failed strip: one stale view must not take every
// pin with it. A STORE that cannot be read fails the read unwrapped, since
// nothing else in the answer could be trusted either; [countPinned] tells the
// two apart by that wrapping alone.
func countView(ctx context.Context, tx *sql.Tx,
	q ViewQuery, view ViewRow) (int, bool, error) {

	// THE CONTAINER THE VIEW WAS SAVED IN, as a board runs it:
	// `work_items{view, container}`. A unit's or person's strip has no
	// container in the query grammar, so those run as saved.
	caller := map[string]any{}
	switch view.Container.Kind {
	case ContainerWorkspace:
		caller["container"] = "workspace"
	case ContainerProject:
		caller["container"] = ContainerProject + ":" + view.Container.ID
	}
	// THE BOARD'S OWN MERGE, `me` included — see [mergeExpansion].
	resolved, err := mergeExpansion(caller, nil, savedDefaults(view.Params),
		q.Viewer)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %w", errUncountable, err)
	}
	parsed, err := ParseQuery(resolved, q.Now, q.Zone)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %w", errUncountable, err)
	}
	parsed.Units = q.Units
	// FLAT, as every surface that RUNS a view runs it: the dashboard sends
	// `subtasks=separate` and `list_work_items` overrules to it, so the
	// grammar's default (a root predicate whose subtree rides along
	// UNFILTERED) would count rows the pin's own list never shows.
	// OVERRULED, not defaulted, because a view saved with a tree mode is
	// still run flat.
	parsed.Subtasks = SubtasksSeparate
	fields, err := resolveFields(ctx, tx, parsed)
	switch {
	case errors.Is(err, errUnresolvedField):
		return 0, false, fmt.Errorf("%w: %w", errUncountable, err)
	case err != nil:
		return 0, false, err
	}
	if parsed.PriorityListOf != "" {
		if parsed.PriorityList, err = readPriorityList(ctx, tx,
			parsed.PriorityListOf); err != nil {
			return 0, false, err
		}
	}
	set, err := compile(parsed, q.Now, fields)
	if err != nil {
		// COMPILING IS PURE: a failure is the view's, never the store's.
		return 0, false, fmt.Errorf("%w: %w", errUncountable, err)
	}
	return countHint(ctx, tx, set)
}

// viewScope is the read's closure: the container the strip belongs to.
//
// It is [containerScope]'s own rule, not a copy: a read's closure and a write's
// scope are compared path against path, and a second spelling would hide a
// deferred view record from a read that then reports itself complete.
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
		// THE TIMELINE SORTS BY START, its axis's own arrangement: rows in
		// rank order draw an unreadable staircase, and a client re-sort
		// would make a page's ORDER depend on which rows it held.
		{Key: ViewKeyTimeline, Name: "Timeline", Type: ViewTimeline,
			Container: container, Builtin: true,
			Params: map[string]string{"sort": "start"}},
		{Key: ViewKeyTable, Name: "Table", Type: ViewTable, Container: container, Builtin: true},
		// THE TRASH IS A QUERY, NOT A RENDERING: a builtin table VIEW, not
		// a sixth [ViewType], since `removed=true` makes it the trash and a
		// type-keyed renderer would hold a shape whose meaning needs a
		// parameter it could be saved without. Any view carrying that
		// parameter is a trash listing.
		//
		// `show_closed`, because a removed task is often a finished one and
		// the group predicate is otherwise ANDed ([Query.ShowClosed]),
		// hiding every deletion of done work. NO `sort`: [sortTerms]
		// already orders a removed listing by `removed_at DESC`, and naming
		// it here would be a second copy to drift.
		{Key: ViewKeyTrash, Name: "Trash", Type: ViewTable,
			Container: container, Builtin: true,
			Params: map[string]string{"removed": "true", "show_closed": "true"}},
	}
	return rows
}

// savedViews reads the container's own views, pinned-for-this-viewer first,
// then by rank.
//
// A PERSONAL VIEW IS PRIVATE TO ITS OWNER, enforced HERE: filtered in SQL
// rather than after the fact, so it cannot ride a page boundary into somebody
// else's strip. A DELETED VIEW is in nobody's strip; its row only guards its
// version ([Writer.DeleteView]).
func savedViews(ctx context.Context, tx *sql.Tx, container Container,
	units Units, viewer string, pinned map[string]bool) ([]ViewRow, error) {

	// A UNIT CONTAINER IS READ UNDER ITS TEAM'S KEY, what a save stores
	// ([CanonicalContainer]), so the team's name finds views saved under it.
	ids := []string{container.ID}
	if container.Kind == ContainerUnit {
		if keys := unitKeys(units, ids); len(keys) > 0 {
			ids = keys
		}
	}
	// THE SHARED HALF IS `owner = ''`, all an anonymous strip gets. The
	// personal half is the viewer's own name, the one they save under: a
	// person bound to a seat writes AS it (iam.ActorFor).
	own := ""
	args := []any{container.Kind}
	args = append(args, anyOf(ids)...)
	if viewer != "" {
		own = " OR owner = ?"
		args = append(args, viewer)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, type, owner, protected, is_default, rank, icon,
		       params_json
		FROM tracker_views
		WHERE container_kind = ? AND container_id IN (`+placeholders(len(ids))+`)
		  AND (owner = ''`+own+`) AND deleted_at IS NULL
		ORDER BY rank, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read %s %s's views: %w",
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
			return nil, fmt.Errorf("worklet: scan a view of %s %s: %w",
				container.Kind, container.ID, err)
		}
		row.Container = container
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
		return nil, fmt.Errorf("worklet: read %s %s's views: %w",
			container.Kind, container.ID, err)
	}
	// TWO ORDERS CONCATENATED, not sorted together: within each half the
	// rank is somebody's arrangement, and one sort over a pinned flag would
	// re-rank the shared strip for one reader.
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
