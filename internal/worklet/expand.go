package worklet

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

// Saved views and presets are DEFAULTS: loaded first, with the caller's
// explicit keys overriding them, so a person who opens a saved board and picks
// another assignee gets the view with that one key changed.
//
// The expansion is not [ParseQuery]'s: a view is a ROW, so reading one is I/O,
// and the parser must not fail on a store (the reason a custom field's ref
// reaches the compiler unresolved). So it is a step BEFORE the parse:
// [Reader.Expand] answers the merged parameters and the caller parses those. A
// view carrying `view=` would expand into itself and one carrying `cursor=`
// resume a page nobody asked for; [checkView], the only writer of a view's
// parameters, refuses both.

// The presets: five questions a person asks often enough that a screen puts
// each on a tab. `tracker_tasks_queue_idx` names `preset=my_queue`,
// `tracker_task_deps_open_idx` `preset=blocked`, and the overdue date alias's
// comment names `preset=overdue`; priorities and triage are the two lists a
// PERSON has that the rows alone cannot express.
const (
	// PresetMyQueue is what the viewer is expected to do next: their open
	// work, most important first. The one preset that needs WHO is asking,
	// hence [Reader.Expand]'s viewer.
	PresetMyQueue = "my_queue"

	// PresetBlocked is open work that cannot move.
	PresetBlocked = "blocked"

	// PresetOverdue is open work past its due date: the `due=overdue`
	// alias's own predicate, not a second one that drifts from it.
	PresetOverdue = "overdue"

	// PresetPriorities is the caller's own ordered list, open tasks only.
	// Its ORDER is the answer, so it expands to the grammar's `priorities` key
	// rather than to a `sort=`.
	PresetPriorities = "priorities"

	// PresetTriage is the open work nobody has picked up: with ONE FIXED
	// STATUS SET a company cannot declare an intake (`triage`) status, so the
	// unassigned open work is what "needs somebody to decide" means.
	PresetTriage = "triage"
)

// Presets are the five, for a refusal that can name them.
var Presets = []string{
	PresetMyQueue, PresetPriorities, PresetTriage, PresetBlocked, PresetOverdue,
}

// expansionRefused are the keys a view or a preset may not carry: `view`
// and `preset` (an expansion that expanded again would loop); `cursor` and
// `around` (the page boundary and the task a caller stands on are that
// caller's own; a saved `around` would answer every reader's "3 of 18" about
// a task they never opened); `read_level`, `max_lag_seconds`, `max_lag_seq`
// and `min_position` (freshness belongs to the SURFACE asking: a saved one
// would silently downgrade a seat's read, or pin every later reader to the
// position the view was saved at).
var expansionRefused = []string{
	"view", "preset", "cursor", "around", "read_level", "max_lag_seconds",
	"max_lag_seq", "min_position",
}

// Viewer is who is asking, as much as an expansion needs to know.
//
// A STRUCT, not a handle: `preset=my_queue` needs who the reader is and which
// project is theirs (its unassigned arm, unscoped, offers every unclaimed task
// in the company). Both come from the SURFACE's credential; the tracker holds
// no org.
type Viewer struct {
	Handle string

	// Project is the viewer's home container, empty where their unit owns
	// none — which narrows `my_queue` to their own assignments rather than
	// widening it to everybody's backlog.
	Project string
}

// Expand resolves `view=` and `preset=` into the parameters they stand for.
//
// THE CALLER'S OWN KEYS WIN: the expansion is defaults. A preset expands
// first and a view over it, the more specific thing, and both lose to what was
// typed. [mergeExpansion] merges, for [Reader.Views]'s pinned-view count too.
func (r *Reader) Expand(ctx context.Context, params map[string]any,
	viewer Viewer) (MapParams, error) {

	var presetDefaults, viewDefaults MapParams
	if preset := strings.TrimSpace(stringOf(params["preset"])); preset != "" {
		defaults, err := expandPreset(preset, viewer)
		if err != nil {
			return nil, err
		}
		presetDefaults = defaults
	}
	if view := strings.TrimSpace(stringOf(params["view"])); view != "" {
		defaults, err := r.expandView(ctx, view)
		if err != nil {
			return nil, err
		}
		viewDefaults = defaults
	}
	return mergeExpansion(params, presetDefaults, viewDefaults, viewer.Handle)
}

// savedDefaults is what a saved view's own parameters contribute to an
// expansion: all of them, since the save refuses every key [expansionRefused]
// names.
func savedDefaults(params map[string]string) MapParams {
	out := make(MapParams, len(params))
	for key, value := range params {
		out[key] = value
	}
	return out
}

// mergeExpansion is what a read's parameters mean once a preset and a saved
// view are read: the preset's defaults, the view's over them, the caller's
// over both, then `me` resolved to the viewer over the RESULT, because a `me`
// means the reader wherever it was written: a view saved with
// `f.reviewers=me` must open for whoever opens it, not reach the compiler as
// a literal "me".
//
// ONE FUNCTION FOR BOTH READERS of a view, the board ([Reader.Expand]) and
// its pin's count ([countView]), so the sidebar number stays the board's.
func mergeExpansion(caller map[string]any, preset, view MapParams,
	viewer string) (MapParams, error) {

	merged := make(MapParams, len(preset)+len(view)+len(caller))
	maps.Copy(merged, preset)
	maps.Copy(merged, view)
	// THE CALLER'S LAST, so every explicit key overrides; `view` and
	// `preset` survive into the parsed query, which names them as its source.
	maps.Copy(merged, MapParams(caller))
	// `me` IS RESOLVED ALWAYS: an `f.<slug>=me` on a query naming no view
	// or preset is still a query about the reader.
	resolved, err := resolveViewerKeys(merged, viewer)
	if err != nil {
		return nil, err
	}
	return MapParams(resolved), nil
}

// resolveViewerKeys substitutes the reader for every `me` a query names.
//
// Here, not in the parser, for the preset expansion's reason: the viewer is a
// property of the SURFACE (its credential, its turn's seat), not of the text
// typed, and a saved view's `me` means whoever opens it only if substituted
// per read. It answers a COPY: whoever built the caller's map still holds it,
// and rewriting its `me` would show the next reader somebody else's query.
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
		return nil, fmt.Errorf("worklet: %s names `me` and this read names "+
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

// expandPreset is one preset's parameters. The viewer's PROJECT, which
// `my_queue`'s second arm needs, comes from the caller: the tracker holds no
// org, so it is resolved from the chart before the expansion.
func expandPreset(name string, viewer Viewer) (MapParams, error) {
	open := string(GroupNotStarted) + "," + string(GroupActive)
	switch name {
	case PresetMyQueue:
		if viewer.Handle == "" {
			// A QUEUE WITH NOBODY'S NAME ON IT is every open task, the
			// widest reading of "mine": refused, naming what is missing.
			return nil, fmt.Errorf("worklet: preset=%s is what one person is "+
				"expected to do next and this read names nobody — a surface "+
				"resolves the viewer from its own credential before it asks",
				PresetMyQueue)
		}
		// "WHAT CAN I PICK UP" IS A DISJUNCTION: the work this person
		// HOLDS, and the work in their project NOBODY holds; `assignee=me`
		// alone hides that backlog from a seat whose queue is empty.
		//
		// AND `blocked=false`: a task that cannot move is not something to
		// pick up, it is `preset=blocked`'s answer.
		out := MapParams{
			"status_group": open,
			"blocked":      "false",
			// PRIORITY THEN DUE, the order `tracker_tasks_queue_idx`
			// is built in, so the index serves this preset.
			"sort": "-priority,due",
		}
		home := strings.TrimSpace(viewer.Project)
		if home == "" {
			// A VIEWER WITH NO PROJECT OF THEIR OWN HAS NO SECOND ARM:
			// unscoped, it would be every unassigned task in the company,
			// the reading this arm's scope prevents. A seat whose unit files
			// no project, or a token or service account bound to no seat,
			// picks up what it holds.
			out["assignee"] = viewer.Handle
			return out, nil
		}
		// THE SECOND ARM IS SCOPED TO THE VIEWER'S OWN PROJECT.
		mine := MapParams{"assignee": viewer.Handle}
		unclaimed := MapParams{"assignee": "none",
			"container": ContainerProject + ":" + home}
		branches, err := json.Marshal([]MapParams{mine, unclaimed})
		if err != nil {
			return nil, fmt.Errorf("worklet: build the %s branches: %w",
				PresetMyQueue, err)
		}
		out["any"] = string(branches)
		return out, nil
	case PresetPriorities:
		if viewer.Handle == "" {
			return nil, fmt.Errorf("worklet: preset=%s is one person's own "+
				"ordered list and this read names nobody — a surface resolves "+
				"the viewer from its own credential before it asks",
				PresetPriorities)
		}
		// NO `sort`, DELIBERATELY: the order somebody decided is the
		// answer, and a sort key would override it. [Reader.Tasks]
		// restores it after the rows are read.
		return MapParams{"priorities": viewer.Handle, "status_group": open}, nil
	case PresetTriage:
		// UNASSIGNED OPEN WORK, as [PresetTriage] defines it.
		return MapParams{
			"assignee": "none", "status_group": open, "sort": "-priority,created",
		}, nil
	case PresetBlocked:
		return MapParams{"blocked": "true", "status_group": open}, nil
	case PresetOverdue:
		// THE ALIAS, not a second predicate: `due=overdue` carries its
		// own open condition, and a copy here would drift from it.
		return MapParams{"due": "overdue"}, nil
	}
	return nil, fmt.Errorf("worklet: %q is not a preset; the three are %s",
		name, strings.Join(Presets, ", "))
}

// expandView reads one saved view's own parameters.
func (r *Reader) expandView(ctx context.Context, id string) (MapParams, error) {
	var out MapParams
	// A STALE READ, ALWAYS, and deliberately not a surface of its own in
	// [statelog.ReadLevelDefaults]: nobody writes a view's parameters and reads
	// them back in one gesture, so the level belongs to the ROWS, and the
	// caller's own would cost a board a barrier per navigation to re-read a
	// filter somebody saved last month.
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
			return fmt.Errorf("worklet: view %s is not on this node; this "+
				"copy has not caught up with it: %w",
				id, statelog.ErrUnavailable)
		}
		// A DELETED VIEW IS A FACT, not a copy behind: running it is
		// refused for good rather than as a read to try again.
		if view.Deleted() {
			return invalid("view %s was deleted, so there is no saved query "+
				"to run: list the views there are with %s", id, ListWorkViewsTool)
		}
		out = savedDefaults(view.Params)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ExpandedQuery is the whole chain: expand, then parse.
//
// ONE ENTRY POINT, so no surface parses without expanding and answers a
// `view=` as an unfiltered list, the silence the unknown-key refusal exists
// to stop.
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
