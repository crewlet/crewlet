package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Saved views and presets as DEFAULTS, and why the expansion is not the
// parser's.
//
// `view=<id>` and `preset=<name>` each stand for a set of parameters, and the
// grammar's own doc has always said how they compose: "loaded FIRST and
// explicit keys override them, so a saved view is a set of defaults rather
// than a lock". A person who opens a saved board and then picks a different
// assignee gets the view with that one key changed — never the view, and never
// the key alone.
//
// [ParseQuery] cannot do it. A view is a ROW, so reading one is I/O, and this
// grammar's whole shape rests on a parser that cannot fail on a store — the
// same reason a custom field's ref reaches the compiler unresolved. So the
// expansion is a step BEFORE the parse, taken by whoever has the reader:
// [Reader.Expand] answers the merged parameters and the caller parses those.
//
// # Why a view may not set every key
//
// A saved view carrying `view=` would expand into itself, and one carrying
// `cursor=` would resume a page the caller never asked for. Both are refused
// at the SAVE — see [checkView] — and stripped here too, because a row written
// by an older build is a row this one still has to read.

// The presets, and there are exactly five: each is a question a person asks
// often enough that a screen puts it on a tab.
//
// Three of them name an index: `tracker_tasks_queue_idx` names
// `preset=my_queue`, `tracker_task_deps_open_idx` names `preset=blocked`, and
// the overdue date alias's own comment names "the preset that means the same
// thing". The other two are the two lists a PERSON has that the rows alone
// cannot express — what somebody put at the top of their queue, and what
// nobody has picked up.
const (
	// PresetMyQueue is what the viewer is expected to do next: their open
	// work, most important first. It is the one preset that needs to know
	// WHO is asking, which is why [Reader.Expand] takes a viewer.
	PresetMyQueue = "my_queue"

	// PresetBlocked is open work that cannot move, which is the list a
	// lead reads before a stand-up.
	PresetBlocked = "blocked"

	// PresetOverdue is open work past its due date — the same predicate
	// the `due=overdue` alias carries, rather than a second one that
	// drifts from it.
	PresetOverdue = "overdue"

	// PresetPriorities is the caller's own ordered list, open tasks only.
	//
	// It is the one preset whose ORDER is the answer: the rows come back
	// in the sequence somebody arranged them in rather than in any sort,
	// which is why it expands to a key the query grammar has rather than
	// to a `sort=`.
	PresetPriorities = "priorities"

	// PresetTriage is the open work nobody has picked up.
	//
	// With ONE FIXED STATUS SET there is no intake status to filter on —
	// a company cannot declare a `triage` column — so the honest
	// definition of "needs somebody to decide" is the unassigned open
	// work, which is what a lead actually reads it for.
	PresetTriage = "triage"
)

// Presets are the five, for a refusal that can name them.
var Presets = []string{
	PresetMyQueue, PresetPriorities, PresetTriage, PresetBlocked, PresetOverdue,
}

// expansionRefused are the keys a view or a preset may not carry.
//
// `view` and `preset` because an expansion that expanded again would be a
// loop; `cursor` because a page boundary belongs to the caller that was handed
// it and to nobody else; `read_level`, `max_lag_seconds` and `max_lag_seq`
// because how fresh an answer must be is a property of the SURFACE asking, and
// a saved row that carried one would let a board silently downgrade a seat's
// own read.
var expansionRefused = []string{
	"view", "preset", "cursor", "read_level", "max_lag_seconds", "max_lag_seq",
}

// Viewer is who is asking, as much as an expansion needs to know.
//
// A STRUCT RATHER THAN A HANDLE, because `preset=my_queue` needs two facts
// about the reader and not one: who they are, and which project is theirs —
// its second arm is the unassigned work in their OWN container, and unscoped
// that arm offers every unclaimed task in the company. Both are properties of
// the SURFACE's own credential, and neither can be read from the tracker,
// which holds no org.
type Viewer struct {
	Handle string

	// Project is the viewer's home container, empty where their unit owns
	// none — which narrows `my_queue` to their own assignments rather than
	// widening it to everybody's backlog.
	Project string
}

// Expand resolves `view=` and `preset=` into the parameters they stand for.
//
// THE CALLER'S OWN KEYS WIN, every one of them: the expansion is a set of
// defaults. A preset expands first and a view over it, because a view is the
// more specific thing — somebody saved it — and both lose to what was typed.
func (r *Reader) Expand(ctx context.Context, params map[string]any,
	viewer Viewer) (MapParams, error) {

	// `me` IS RESOLVED FIRST AND ALWAYS, before the early return: a
	// `f.<slug>=me` on a query that names no view and no preset is still
	// a query about the reader, and returning the parameters untouched
	// would hand the compiler a literal "me".
	params, err := resolveViewerKeys(params, viewer.Handle)
	if err != nil {
		return nil, err
	}

	view := strings.TrimSpace(stringOf(params["view"]))
	preset := strings.TrimSpace(stringOf(params["preset"]))
	if view == "" && preset == "" {
		return MapParams(params), nil
	}

	merged := MapParams{}
	if preset != "" {
		defaults, err := expandPreset(preset, viewer)
		if err != nil {
			return nil, err
		}
		maps.Copy(merged, defaults)
	}
	if view != "" {
		defaults, err := r.expandView(ctx, view)
		if err != nil {
			return nil, err
		}
		maps.Copy(merged, defaults)
	}
	// THE CALLER'S LAST, so every explicit key overrides — and `view` and
	// `preset` themselves survive into the parsed query, where they are
	// what an answer can say it came from.
	maps.Copy(merged, MapParams(params))
	return merged, nil
}

// resolveViewerKeys substitutes the reader for every `me` a query names.
//
// ONE PLACE, and it is here rather than in the parser for the reason the
// preset expansion is: a parser that resolved a viewer would be a parser that
// needed one, and the viewer is a property of the SURFACE — its credential,
// its turn's seat — rather than of the text somebody typed. A saved view
// carrying `me` means whoever opens it, which is only true if the
// substitution happens per read.
//
// A COPY, never a mutation of the caller's map: the same parameters are held
// by whoever built them, and rewriting one person's `me` into a handle would
// make the next reader of that map see somebody else's query.
func resolveViewerKeys(params map[string]any, viewer string) (map[string]any, error) {
	var named []string
	for key, value := range params {
		if !strings.HasPrefix(key, FieldKeyPrefix) {
			continue
		}
		if strings.TrimSpace(stringOf(value)) == FieldOpMe {
			named = append(named, key)
		}
	}
	if len(named) == 0 {
		return params, nil
	}
	if viewer == "" {
		slices.Sort(named)
		return nil, fmt.Errorf("tracker: %s names `me` and this read names "+
			"nobody — a surface resolves the viewer from its own credential "+
			"before it asks", strings.Join(named, ", "))
	}
	out := make(map[string]any, len(params))
	maps.Copy(out, params)
	for _, key := range named {
		out[key] = viewer
	}
	return out, nil
}

// expandPreset is one preset's parameters.
// The viewer's own PROJECT is what `my_queue`'s second arm needs, and only the
// caller can supply it: the tracker holds no org, so "this seat's project" is
// a fact about the chart and is resolved before the expansion.
func expandPreset(name string, viewer Viewer) (MapParams, error) {
	open := string(GroupNotStarted) + "," + string(GroupActive)
	switch name {
	case PresetMyQueue:
		if viewer.Handle == "" {
			// A QUEUE WITH NOBODY'S NAME ON IT IS EVERY OPEN TASK,
			// which is the widest possible reading of "mine". Refused
			// naming what is missing rather than answered.
			return nil, fmt.Errorf("tracker: preset=%s is what one person is "+
				"expected to do next and this read names nobody — a surface "+
				"resolves the viewer from its own credential before it asks",
				PresetMyQueue)
		}
		// "WHAT CAN I PICK UP" IS A DISJUNCTION, and both arms matter:
		// the work this person HOLDS, and the work in their project that
		// NOBODY holds. Written as `assignee=me` alone it answered only
		// the first, so a seat whose queue was empty read the company as
		// having nothing for it while its own project's unassigned
		// backlog sat there.
		//
		// AND `blocked=false`, because a task that cannot move is not
		// something to pick up — it is `preset=blocked`'s answer, and
		// putting it here would mean the two presets returned the same
		// rows for the wrong reason.
		mine := MapParams{"assignee": viewer.Handle}
		unclaimed := MapParams{"assignee": "none"}
		if home := strings.TrimSpace(viewer.Project); home != "" {
			// THE SECOND ARM IS SCOPED TO THE VIEWER'S OWN PROJECT.
			// Unscoped it would offer every unassigned task in the
			// company, which is the widest possible reading of "what
			// can I pick up" and the one nobody meant.
			unclaimed["container"] = ContainerProject + ":" + home
		}
		branches, err := json.Marshal([]MapParams{mine, unclaimed})
		if err != nil {
			return nil, fmt.Errorf("tracker: build the %s branches: %w",
				PresetMyQueue, err)
		}
		return MapParams{
			"any":          string(branches),
			"status_group": open,
			"blocked":      "false",
			// PRIORITY THEN DUE, which is the order
			// `tracker_tasks_queue_idx` is built in — so the preset
			// the index is named for is the query it serves.
			"sort": "-priority,due",
		}, nil
	case PresetPriorities:
		if viewer.Handle == "" {
			return nil, fmt.Errorf("tracker: preset=%s is one person's own "+
				"ordered list and this read names nobody — a surface resolves "+
				"the viewer from its own credential before it asks",
				PresetPriorities)
		}
		// NO `sort`, DELIBERATELY. The order of this list is the answer
		// — it is what somebody decided — so it is the list's own, and a
		// sort key here would be a second order overriding the first.
		// [Reader.Tasks] restores it after the rows are read.
		return MapParams{"priorities": viewer.Handle, "status_group": open}, nil
	case PresetTriage:
		// UNASSIGNED OPEN WORK, and that is the honest definition: with
		// one fixed status set there is no intake status to filter on,
		// so "needs somebody to decide" is "nobody has picked it up".
		return MapParams{
			"assignee": "none", "status_group": open, "sort": "-priority,created",
		}, nil
	case PresetBlocked:
		return MapParams{"blocked": "true", "status_group": open}, nil
	case PresetOverdue:
		// THE ALIAS, not a second predicate: `due=overdue` carries its
		// own open condition, and writing the condition again here is
		// how two spellings of one question drift apart.
		return MapParams{"due": "overdue"}, nil
	}
	return nil, fmt.Errorf("tracker: %q is not a preset — the three are %s",
		name, strings.Join(Presets, ", "))
}

// expandView reads one saved view's own parameters.
func (r *Reader) expandView(ctx context.Context, id string) (MapParams, error) {
	var out MapParams
	// A STALE READ, ALWAYS — and deliberately NOT a surface of its own in
	// [statelog.ReadLevelDefaults]. The level here is a property of the
	// ROWS rather than of the reader: the parameters of a saved view are
	// not something anybody writes and then reads back in one gesture, so
	// no caller's surface has an interest in them being fresher. Taking
	// the caller's own level would put a board's furniture on the same
	// round trip as its rows, which is a barrier per navigation to
	// re-read a filter somebody saved last month.
	if _, err := r.log.Read(ctx, statelog.Query{
		Level: statelog.ReadStale, Scope: viewScope(Container{
			Kind: ContainerWorkspace,
		}), Set: true,
	}, func(tx *sql.Tx) error {
		view, held, err := readView(ctx, tx, id)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("tracker: view %s is not on this node — it was "+
				"deleted, or this copy has not caught up: %w",
				id, statelog.ErrUnavailable)
		}
		out = make(MapParams, len(view.Params))
		for key, value := range view.Params {
			if slices.Contains(expansionRefused, key) {
				// A ROW AN OLDER BUILD WROTE, since the save refuses
				// these — dropped rather than honoured, because a
				// saved cursor would resume a page nobody asked for.
				continue
			}
			out[key] = value
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ExpandedQuery is the whole chain: expand, then parse.
//
// ONE ENTRY POINT, so a surface cannot parse without expanding — which would
// answer a `view=` as an unfiltered list, the exact silence this grammar's
// unknown-key refusal exists to stop.
func (r *Reader) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer Viewer, now time.Time, loc *time.Location) (Query, error) {

	merged, err := r.Expand(ctx, params, viewer)
	if err != nil {
		return Query{}, err
	}
	return ParseQuery(merged, now, loc)
}

// stringOf renders a parameter value the way [MapParams] does.
func stringOf(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
