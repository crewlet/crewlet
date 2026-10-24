package datasetsapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/iam"
)

// binding is how one catalogue source is answered: the question asked, the
// parameters its arguments become, and the projection of the answer into
// rows. A source with no question is the agents' overview.
type binding struct {
	// question is the registry question, or "" for the agents overview.
	question string

	// params are the question's parameters for the source's arguments,
	// resolved for viewer: a personal scope names the viewer's own record,
	// never the author's.
	params func(args map[string]string, viewer iam.Principal) map[string]any

	// rows projects the answer, as the REST route encodes it, into rows
	// keyed by the source's columns, and reports whether the answer was a
	// page of a longer set.
	rows func(raw []byte) ([]map[string]any, bool, error)
}

// openStatusGroups is the tracker's open work: what a listing with no status
// group reads.
const openStatusGroups = "not_started,active"

// trackerScopes are the presets a `tracker.items` scope stands for. `all` and
// `mine` are no preset: `mine` is the viewer's own assignments, named by the
// record the viewer's own tools write under ([iam.RecordOwner]).
var trackerScopes = map[string]string{
	"queue": "my_queue", "priorities": "priorities", "triage": "triage",
	"blocked": "blocked", "overdue": "overdue",
}

// bindings is every catalogue source this surface answers; [New] refuses a
// catalogue entry without one, and a binding the catalogue does not list.
var bindings = map[string]binding{
	"tracker.items": {
		question: "work_items",
		params: func(args map[string]string, viewer iam.Principal) map[string]any {
			p := map[string]any{}
			scope := args["scope"]
			switch preset, ok := trackerScopes[scope]; {
			case ok:
				p["preset"] = preset
			case scope == "mine":
				p["assignee"] = iam.RecordOwner(viewer)
			}
			if g := args["status_group"]; g != "" {
				p["status_group"] = g
			} else if p["preset"] == nil {
				p["status_group"] = openStatusGroups
			}
			if project := args["project"]; project != "" {
				p["project"] = project
			}
			if limit := args["limit"]; limit != "" {
				p["limit"] = limit
			}
			return p
		},
		rows: listOf("items", "next_cursor", map[string]string{
			"key": "key", "title": "title", "status": "status", "status_group": "status_group",
			"priority": "priority", "assignee": "assignee", "project": "project", "due": "due",
			"updated": "updated", "points": "points", "blocked": "blocked", "overdue": "overdue",
		}, "blocked", "overdue"),
	},
	"tracker.workload": {
		question: "work_workload",
		params: func(args map[string]string, _ iam.Principal) map[string]any {
			p := map[string]any{}
			if unit := args["unit"]; unit != "" {
				p["unit"] = unit
			}
			return p
		},
		rows: listOf("rows", "", map[string]string{
			"handle": "handle", "open": "open", "points": "points", "blocked": "blocked",
			"overdue": "overdue", "unscheduled": "unscheduled",
		}),
	},
	"spend.seats": {
		question: "tokens",
		params: func(args map[string]string, _ iam.Principal) map[string]any {
			p := map[string]any{}
			if days := args["days"]; days != "" {
				p["days"] = days
			}
			return p
		},
		rows: listOf("by_agent", "", map[string]string{
			"handle": "handle", "name": "role", "person": "person", "total_tokens": "total_tokens",
			"input_tokens": "input_tokens", "output_tokens": "output_tokens", "calls": "calls",
		}, "person"),
	},
	"spend.daily": {
		question: "token_series",
		params: func(args map[string]string, _ iam.Principal) map[string]any {
			return map[string]any{"days": args["days"], "group": args["group"], "bucket": "day"}
		},
		rows: dailyRows,
	},
	"agents.status": {
		rows: func(raw []byte) ([]map[string]any, bool, error) {
			var seats []map[string]any
			if err := decode(raw, &seats); err != nil {
				return nil, false, err
			}
			out := make([]map[string]any, len(seats))
			for i, seat := range seats {
				out[i] = map[string]any{
					"handle":       seat["handle"],
					"name":         seat["role"],
					"activity":     seat["activity"],
					"phase":        seat["current_phase"],
					"paused":       seat["paused"] != nil,
					"last_turn_at": pluck(seat, "last_turn.ended_at"),
					"last_outcome": pluck(seat, "last_turn.outcome"),
				}
			}
			return out, false, nil
		},
	},
}

// listOf projects the array at field of an answer object, each column read
// from the row's field the map names; paged is the field whose presence says
// the array is one page of more. falseWhenAbsent are the boolean fields the
// answer leaves out when false (`omitempty`), which are false rather than no
// value: a task that is not blocked is not of unknown blockedness.
func listOf(field, paged string, columns map[string]string,
	falseWhenAbsent ...string) func([]byte) ([]map[string]any, bool, error) {
	return func(raw []byte) ([]map[string]any, bool, error) {
		var answer map[string]any
		if err := decode(raw, &answer); err != nil {
			return nil, false, err
		}
		list, ok := answer[field].([]any)
		if !ok && answer[field] != nil {
			return nil, false, fmt.Errorf("the answer's %q is not a list", field)
		}
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			row, ok := item.(map[string]any)
			if !ok {
				return nil, false, fmt.Errorf("the answer's %q holds a row that is not an object", field)
			}
			projected := make(map[string]any, len(columns))
			for col, path := range columns {
				projected[col] = pluck(row, path)
				if projected[col] == nil && slices.Contains(falseWhenAbsent, col) {
					projected[col] = false
				}
			}
			out = append(out, projected)
		}
		more := paged != "" && answer[paged] != nil && answer[paged] != ""
		return out, more, nil
	}
}

// dailyRows projects a spend series into one row per day and band: each
// named band in the series' own order, then the rest as Other, a band with
// nothing that day left out as the series leaves it out.
func dailyRows(raw []byte) ([]map[string]any, bool, error) {
	var series struct {
		Points []struct {
			Window string                    `json:"window"`
			Groups map[string]map[string]any `json:"groups"`
			Other  map[string]any            `json:"other"`
		} `json:"series"`
		ByGroup []struct {
			Group string `json:"group"`
			Label string `json:"label"`
		} `json:"by_group"`
	}
	if err := decode(raw, &series); err != nil {
		return nil, false, err
	}
	var out []map[string]any
	for _, point := range series.Points {
		for _, band := range series.ByGroup {
			bucket, label := point.Groups[band.Group], band.Label
			if band.Group == "" {
				bucket, label = point.Other, "Other"
			} else if label == "" {
				label = band.Group
			}
			if bucket == nil || bucket["total_tokens"] == nil {
				continue
			}
			out = append(out, map[string]any{"day": point.Window, "band": label,
				"total_tokens": bucket["total_tokens"]})
		}
	}
	return out, false, nil
}

// pluck is the value at a dotted path of an object, nil where any step is
// absent.
func pluck(obj map[string]any, path string) any {
	var cur any = obj
	for _, step := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[step]
	}
	switch cur.(type) {
	case map[string]any, []any:
		// A COLUMN IS A SCALAR: an object where a binding expected one is
		// a binding that read the wrong field, answered as no value
		// rather than as a string of JSON a widget would draw.
		return nil
	}
	return cur
}

// decode reads JSON keeping numbers exact.
func decode(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

// checkBindings holds the bindings and the catalogue to each other: every
// source bound and every binding a source. A wiring error, refused at [New];
// that every projection answers exactly the source's columns is the suite's.
func checkBindings() error {
	for _, src := range dashspec.Catalogue() {
		if _, ok := bindings[src.Name]; !ok {
			return fmt.Errorf("datasetsapi: the catalogue's %s has no binding", src.Name)
		}
	}
	for name := range bindings {
		if _, ok := dashspec.Source(name); !ok {
			return fmt.Errorf("datasetsapi: %s is bound and not in the catalogue", name)
		}
	}
	return nil
}
