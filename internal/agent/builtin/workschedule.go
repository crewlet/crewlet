package builtin

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// When a task is due and how big it is.
//
// # These were filterable, sortable, reportable — and unwritable
//
// `tracker_tasks` has carried `start_at`, `due_at`, `estimate_min` and
// `points` since migration 0002. The query grammar filters on all four and
// sorts on three; a row's `overdue` flag is derived from `due_at`; a total is
// a sum over `points` or `estimate_min`; and the `overdue` preset and the
// `due=` date tokens are documented surface.
//
// NOTHING COULD SET ANY OF THEM. `create_work_item` accepted none and
// `update_work_item` accepted none.
//
// The consequences were silent and total, which is why nothing reported it: a
// due date could not exist, so `due=overdue` matched nothing and the overdue
// flag was never true, and an estimate could not exist, so every total was
// zero for a team that had sized all of its work.
//
// The write path was complete the whole time — [tracker.TaskPatch] carries all
// four and the applier writes the rows. What was missing was the few lines of
// tool surface that reach it.
//
// # One grammar for a date, shared with the filter that reads it
//
// A due date is set with the same tokens `due=` already filters on —
// [tracker.ResolveDate]: an instant, a calendar date, `+7d`, `today`, `eow`.
// A second spelling here would be the copy that stops matching, and it would
// make a model that can ask for "everything due this week" unable to say "due
// this week" about one task.
//
// # `null` clears, and a missing key changes nothing
//
// The difference is the reason every one of these is a pointer on the patch:
// "leave the due date alone" and "this task has no due date any more" are
// different edits, and a tool that could only express the first would make a
// date impossible to take back off.

// scheduleSchema is the five parameters, declared once for both tools.
//
// ONE DECLARATION because a create that accepts a due date and an update that
// does not is the same bug one step later — which is the shape the two tools
// were actually in, both at zero.
func scheduleSchema(update bool) map[string]any {
	clears := ""
	// THE TYPE ADMITS THE NULL THE DESCRIPTION PROMISES. An update's five
	// fields are cleared by passing null — [readSchedule] reads it, and
	// `clears` below tells the model so — but the type said `string` and
	// `integer` alone, and a caller that VALIDATES against this schema
	// refuses the null before the tool is ever reached. The gesture was
	// documented, implemented, and unreachable through any strict client.
	//
	// The union is on the UPDATE only: a create has nothing to clear, so
	// a null there is a value nobody meant rather than an instruction.
	nullable := func(t string) any { return t }
	if update {
		clears = " null clears it."
		nullable = func(t string) any { return []string{t, "null"} }
	}
	return map[string]any{
		"due": map[string]any{
			"type": nullable("string"),
			"description": "When this is due: a date (2031-04-16), an instant, " +
				"or one of the relative words `due=` filters on — today, " +
				"tomorrow, eow (the week's end: midnight ending Sunday, " +
				"since a week starts on Monday), eom (midnight ending the " +
				"month), or an offset like +7d." + clears,
		},
		"start": map[string]any{
			"type": nullable("string"),
			"description": "When work on this should start, in the same " +
				"spellings as `due`." + clears,
		},
		"estimate_minutes": map[string]any{
			"type": nullable("integer"),
			"description": "How long this is expected to take, in minutes. " +
				"The other half of the size, beside `points`." + clears,
		},
		"points": map[string]any{
			"type": nullable("number"),
			"description": "How big this is on the team's own scale. The " +
				"other half of the size above." + clears,
		},
	}
}

// schedule is the four values, resolved, with absent and cleared told apart.
type schedule struct {
	Start     *time.Time
	Due       *time.Time
	DueAllDay *bool
	Estimate  *int
	Points    *float64

	// Cleared names the keys that were explicitly set to null, which a
	// create ignores — a new task has nothing to clear — and an update
	// turns into a zero value on the patch.
	Cleared map[string]bool
}

// readSchedule reads the five, refusing anything it cannot resolve BY NAME.
//
// REFUSED RATHER THAN DROPPED. A date this grammar cannot read is the one
// argument where silently ignoring it is worst: the write succeeds, the answer
// says `applied`, and the task simply has no due date — which a model reads as
// having set one.
func readSchedule(args map[string]any, now time.Time, loc *time.Location,
	tool string) (schedule, string) {

	out := schedule{Cleared: map[string]bool{}}

	for key, target := range map[string]**time.Time{
		"due":   &out.Due,
		"start": &out.Start,
	} {
		raw, held := args[key]
		if !held {
			continue
		}
		if raw == nil {
			out.Cleared[key] = true
			continue
		}
		token := strings.TrimSpace(argString(args, key))
		if token == "" {
			out.Cleared[key] = true
			continue
		}
		anchor, err := tracker.ResolveDate(token, now, loc)
		if err != nil {
			return out, fmt.Sprintf("%s could not read `%s`: %v", tool, key, err)
		}
		at := anchor.At
		*target = &at
		// A TOKEN THAT NAMED A DAY SETS THE ALL-DAY FLAG, so a renderer
		// shows "16 April" rather than "16 April, 00:00" for a date
		// nobody gave a time to. It rides the DUE date alone, which is
		// the only one the flag is about.
		if key == "due" {
			allDay := anchor.AllDay
			out.DueAllDay = &allDay
		}
	}

	if raw, held := args["estimate_minutes"]; held {
		if raw == nil {
			out.Cleared["estimate_minutes"] = true
		} else {
			minutes, ok := argIntValue(raw)
			if !ok {
				return out, fmt.Sprintf("%s could not read `estimate_minutes`: "+
					"%s. It is a whole number of MINUTES — 90, not \"90m\" and "+
					"not \"an hour and a half\". Pass null to clear it.",
					tool, argString(args, "estimate_minutes"))
			}
			if minutes < 0 {
				return out, fmt.Sprintf("%s was given a negative "+
					"`estimate_minutes`; an estimate is a duration.", tool)
			}
			out.Estimate = &minutes
		}
	}
	if raw, held := args["points"]; held {
		if raw == nil {
			out.Cleared["points"] = true
		} else {
			points, ok := argFloatValue(raw)
			if !ok {
				return out, fmt.Sprintf("%s could not read `points`: %s. It is "+
					"a number — 3 or 0.5, not a word. Pass null to clear it.",
					tool, argString(args, "points"))
			}
			if points < 0 {
				return out, fmt.Sprintf("%s was given negative `points`; a "+
					"size is not negative.", tool)
			}
			out.Points = &points
		}
	}
	return out, ""
}

// applyToTask writes what a CREATE was given. A create clears nothing: there
// is no previous value for a null to take back.
func (s schedule) applyToTask(task *tracker.Task) {
	task.StartAt = s.Start
	task.DueAt = s.Due
	if s.DueAllDay != nil {
		task.DueAllDay = *s.DueAllDay
	}
	if s.Estimate != nil {
		task.EstimateMinutes = *s.Estimate
	}
	if s.Points != nil {
		task.Points = *s.Points
	}
}

// applyToPatch writes what an UPDATE was given, cleared keys included.
//
// A CLEARED DATE IS THE ZERO INSTANT rather than a nil pointer, because nil on
// a patch means "not named" — the whole point of the pointer — so clearing has
// to be a value the applier can recognise as empty.
func (s schedule) applyToPatch(patch *tracker.TaskPatch) bool {
	touched := false
	if s.Start != nil {
		patch.StartAt, touched = s.Start, true
	} else if s.Cleared["start"] {
		patch.StartAt, touched = &time.Time{}, true
	}
	if s.Due != nil {
		patch.DueAt, touched = s.Due, true
		patch.DueAllDay = s.DueAllDay
	} else if s.Cleared["due"] {
		patch.DueAt, touched = &time.Time{}, true
		allDay := false
		patch.DueAllDay = &allDay
	}
	if s.Estimate != nil {
		patch.EstimateMinutes, touched = s.Estimate, true
	} else if s.Cleared["estimate_minutes"] {
		zero := 0
		patch.EstimateMinutes, touched = &zero, true
	}
	if s.Points != nil {
		patch.Points, touched = s.Points, true
	} else if s.Cleared["points"] {
		zero := 0.0
		patch.Points, touched = &zero, true
	}
	return touched
}
