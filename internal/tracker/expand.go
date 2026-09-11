package tracker

import (
	"context"
	"database/sql"
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

// The presets, and there are exactly three: each is a question a person asks
// often enough that the engine's own indexes are named for it.
//
// `tracker_tasks_queue_idx` names `preset=my_queue`, `tracker_task_deps_open_idx`
// names `preset=blocked`, and the overdue date alias's own comment names "the
// preset that means the same thing". A fourth is a decision somebody makes
// with a screen in front of them, not one this list should anticipate.
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
)

// Presets are the three, for a refusal that can name them.
var Presets = []string{PresetMyQueue, PresetBlocked, PresetOverdue}

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

// Expand resolves `view=` and `preset=` into the parameters they stand for.
//
// THE CALLER'S OWN KEYS WIN, every one of them: the expansion is a set of
// defaults. A preset expands first and a view over it, because a view is the
// more specific thing — somebody saved it — and both lose to what was typed.
func (r *Reader) Expand(ctx context.Context, params map[string]any,
	viewer string) (MapParams, error) {

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

// expandPreset is one preset's parameters.
func expandPreset(name, viewer string) (MapParams, error) {
	open := string(GroupNotStarted) + "," + string(GroupActive)
	switch name {
	case PresetMyQueue:
		if viewer == "" {
			// A QUEUE WITH NOBODY'S NAME ON IT IS EVERY OPEN TASK,
			// which is the widest possible reading of "mine". Refused
			// naming what is missing rather than answered.
			return nil, fmt.Errorf("tracker: preset=%s is what one person is "+
				"expected to do next and this read names nobody — a surface "+
				"resolves the viewer from its own credential before it asks",
				PresetMyQueue)
		}
		return MapParams{
			"assignee":     viewer,
			"status_group": open,
			// PRIORITY THEN DUE, which is the order
			// `tracker_tasks_queue_idx` is built in — so the preset
			// the index is named for is the query it serves.
			"sort": "-priority,due",
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
	// A STALE READ, always: the parameters of a saved view are not
	// something a caller writes and then reads back in one gesture, and
	// taking the caller's own level here would put a board's furniture on
	// the same round trip as its rows.
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
	viewer string, now time.Time, loc *time.Location) (Query, error) {

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
