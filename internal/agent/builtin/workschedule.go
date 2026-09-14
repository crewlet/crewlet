package builtin

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// When a task is due, how big it is, and which sprint it is in.
//
// # These were filterable, sortable, reportable — and unwritable
//
// `tracker_tasks` has carried `start_at`, `due_at`, `estimate_min`, `points`
// and `sprint_number` since migration 0002. The query grammar filters on all
// five and sorts on three; a row's `overdue` flag is derived from `due_at`;
// every figure a sprint report carries is a sum over `points` or
// `estimate_min`; and the `overdue` preset, the `due=` date tokens and the
// `sprint=` membership values are all documented surface.
//
// NOTHING COULD SET ANY OF THEM. `create_work_item` accepted none and
// `update_work_item` accepted none, and the only producer of a sprint patch in
// the whole tree was the ROLLOVER — which moves work that is already in a
// sprint, so a sprint could never come to contain anything in the first place.
//
// The consequences were silent and total, which is why nothing reported it: a
// due date could not exist, so `due=overdue` matched nothing and the overdue
// flag was never true; an estimate could not exist, so every sprint figure was
// zero, every sprint reported 100% unestimated, and velocity was zero for a
// team that delivered everything it took on; and a sprint could not be filled,
// so the cadence duty minted, started and closed empty sprints for ever.
//
// The write path was complete the whole time — [tracker.TaskPatch] carries all
// five and the applier writes the rows and maintains the stays. What was
// missing was the six lines of tool surface that reach it.
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
				"Sprint figures are summed in whichever of this and `points` " +
				"the project's own measure names." + clears,
		},
		"points": map[string]any{
			"type": nullable("number"),
			"description": "How big this is on the team's own scale. The " +
				"other half of the measure above." + clears,
		},
		"sprint": map[string]any{
			"type": nullable("integer"),
			"description": "The sprint number this is planned into — " +
				"`sprint_report` lists the ones a project has. Setting it " +
				"opens the task's membership of that sprint, which is what " +
				"every sprint figure is derived from." + clears,
		},
	}
}

// schedule is the five values, resolved, with absent and cleared told apart.
type schedule struct {
	Start     *time.Time
	Due       *time.Time
	DueAllDay *bool
	Estimate  *int
	Points    *float64
	Sprint    *int

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
			minutes, ok := scheduleInt(raw)
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
			points, ok := scheduleFloat(raw)
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
	if raw, held := args["sprint"]; held {
		if raw == nil {
			out.Cleared["sprint"] = true
		} else {
			number, ok := scheduleInt(raw)
			if !ok {
				return out, fmt.Sprintf("%s could not read `sprint`: %s. It is "+
					"the sprint's NUMBER — `sprint_report` lists the ones this "+
					"project has. Pass null to take the task out of its sprint.",
					tool, argString(args, "sprint"))
			}
			if number <= 0 {
				return out, fmt.Sprintf("%s was given sprint %d, and sprints "+
					"are numbered from 1 — `sprint_report` lists the ones this "+
					"project has. Pass null to take the task out of its "+
					"sprint.", tool, number)
			}
			out.Sprint = &number
		}
	}
	return out, ""
}

// scheduleInt and scheduleFloat read a number, reporting whether they COULD.
//
// # Why not [argInt] and [argFloat]
//
// Because both answer a fallback for a value they cannot read, and every field
// here is a pointer precisely because its zero is a SETTING rather than an
// absence: zero minutes and zero points both mean UNESTIMATED. So an
// unreadable value became `&0` and the write succeeded — `estimate_minutes:
// "two hours"` answered `applied` and wiped the estimate, which is the exact
// failure this function's own header says it exists to prevent, in the half of
// it that was not written to the rule. [argFloat]'s doc states the condition
// under which its zero is right — "every caller of this one has a field whose
// zero IS its default" — and these three are the callers that broke it.
//
// The parse is the WHOLE string rather than [fmt.Sscanf]'s prefix, which is
// the other half of the same bug: `Sscanf("%d")` reads "2 days" as 2, so a
// two-day estimate was stored as two MINUTES and nothing was refused.
func scheduleInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		// JSON has one number type, so a whole number arrives here. A
		// fraction is not a whole number of minutes and is refused
		// rather than truncated to one nobody typed.
		//
		// FINITE FIRST, because the fraction test does not cover it: an
		// infinity IS its own truncation, so it passed, and `int(+Inf)`
		// is not defined by the language — it lands on the platform's
		// minimum int, which the negative check below then refuses as a
		// NEGATIVE estimate. Right answer, wrong reason, and a message
		// naming a sign nobody typed.
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

func scheduleFloat(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, finite(v)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		// [strconv.ParseFloat] ACCEPTS "NaN", "Inf" and "infinity" in
		// every casing, which is why the check is here rather than left
		// to the caller: a size of NaN passed the `points < 0` guard
		// below — every comparison with NaN is false — and an infinity
		// passed it honestly, so both reached the writer. Downstream
		// neither is a number a sprint can be summed with, and JSON
		// cannot even encode them, so the failure surfaced as a broken
		// answer somewhere with no memory of who typed it.
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil && finite(n)
	}
	return 0, false
}

// finite is what a size has to be: a real number a sprint's figures can be
// summed from, which NaN and the infinities are not.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

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
	task.Sprint = s.Sprint
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
	if s.Sprint != nil {
		patch.Sprint, touched = s.Sprint, true
	} else if s.Cleared["sprint"] {
		// ZERO IS OUT OF EVERY SPRINT, which is how the rollover already
		// spells it — see the `backlog` arm of [tracker.Writer]'s own
		// spillover walk. A nil here would mean "not named".
		none := 0
		patch.Sprint, touched = &none, true
	}
	return touched
}
