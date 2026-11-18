package builtin

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/worklet"
)

// When a task is due and how big it is: the work tools' writes of `start_at`,
// `due_at`, `estimate_min` and `points`, which the board filters, sorts and
// totals on.
//
// A date is written in the tokens `due=` filters on ([period.Resolve]), so the
// two spellings cannot drift apart.
//
// A null clears a value and a missing key leaves it, which is why each is a
// pointer on the patch: otherwise a date could never be taken back off.

// scheduleSchema is the five parameters, declared once for both tools so a
// create and an update cannot accept different ones, and an update's wait.
func scheduleSchema(update bool) map[string]any {
	clears := ""
	// An update's type admits null, the clear [readSchedule] reads: a client
	// that validates against the schema would otherwise refuse it before the
	// tool. A create has nothing to clear, so a null there is no instruction.
	nullable := func(t string) any { return t }
	if update {
		clears = " null clears it."
		nullable = func(t string) any { return []string{t, "null"} }
	}
	out := map[string]any{
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
	if update {
		out["wait_until"] = map[string]any{
			"type": nullable("string"),
			"description": "Set the item aside until a real date, in the spellings of `due`: " +
				"you are not handed it before then, and are handed it once when it comes. " +
				"It is the item's `start`, with `wait_reason` beside it. null ends the wait. " +
				"A wait on a person is an `ask` on " + CommentOnWorkTool + ", never a date: " +
				"an agent's wait whose reason names a person it has not asked is refused.",
		}
		out["wait_reason"] = map[string]any{
			"type": "string",
			"description": "Required with `wait_until`: one line saying what the item " +
				"waits for, shown beside the date.",
		}
	}
	return out
}

// schedule is the four values, resolved, with absent and cleared told apart.
type schedule struct {
	Start     *time.Time
	Due       *time.Time
	DueAllDay *bool
	Estimate  *int
	Points    *float64

	// WaitReason is a wait's reason, read with `wait_until` into Start.
	WaitReason *string

	// Cleared names the keys explicitly set to null: a create ignores them,
	// an update clears them on the patch.
	Cleared map[string]bool
}

// readSchedule reads the five, refusing BY NAME anything it cannot resolve:
// a dropped date would answer `applied`, which a model reads as having set one.
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
		anchor, err := period.Resolve(token, now, loc)
		if err != nil {
			return out, fmt.Sprintf("%s could not read `%s`: %s", tool, key,
				worklet.Sentence(err))
		}
		if anchor.At.IsZero() {
			// The tracker refuses the zero instant too
			// ([worklet.TaskPatch.CheckDates]), but in the patch's words.
			return out, fmt.Sprintf("%s was given `%s` %q, which is the zero instant "+
				"(January of year one) and no date: name a real one.", tool, key, clip(token))
		}
		at := anchor.At
		*target = &at
		// A token naming a day sets the due date's all-day flag, so it
		// renders as "16 April" rather than "16 April, 00:00".
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

// readWait reads an update's `wait_until` into the plan's start, with the
// `wait_reason` it requires; null or empty ends the wait.
func readWait(args map[string]any, now time.Time, loc *time.Location, plan *schedule,
	tool string) string {

	raw, held := args["wait_until"]
	_, start := args["start"]
	reason := strings.TrimSpace(argString(args, "wait_reason"))
	switch {
	case !held && reason != "":
		return tool + " was given `wait_reason` without `wait_until`: a reason says " +
			"why the item waits until a date. Nothing was written."
	case !held:
		return ""
	case start:
		return tool + " was given both `wait_until` and `start`, which set the same " +
			"date: give one. Nothing was written."
	}
	token := ""
	if raw != nil {
		token = strings.TrimSpace(argString(args, "wait_until"))
	}
	if token == "" {
		plan.Start, plan.Cleared["start"] = nil, true
		return ""
	}
	anchor, err := period.Resolve(token, now, loc)
	switch {
	case err != nil:
		return fmt.Sprintf("%s could not read `wait_until`: %s", tool, worklet.Sentence(err))
	case !anchor.At.After(now):
		return fmt.Sprintf("%s was given a `wait_until` that has passed (%s): a wait "+
			"is until a later date. Nothing was written.", tool, anchor.At.Format(time.RFC3339))
	case reason == "":
		return tool + " was given `wait_until` without `wait_reason`: say in one line " +
			"what the item waits for, which it shows beside the date. Nothing was written."
	case len(reason) > worklet.MaxWaitReason || strings.ContainsAny(reason, "\r\n"):
		return fmt.Sprintf("%s was given a `wait_reason` that is not one line of at most %d "+
			"bytes. Nothing was written.", tool, worklet.MaxWaitReason)
	}
	at := anchor.At
	plan.Start, plan.WaitReason = &at, &reason
	return ""
}

// applyToTask writes what a CREATE was given. A create clears nothing: there
// is no previous value for a null to take back.
func (s schedule) applyToTask(task *worklet.Task) {
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
// A cleared date is its flag ([worklet.TaskPatch.ClearStart],
// [worklet.TaskPatch.ClearDue]): nil on a patch means "not named", and a date
// has no empty value (the patch type's doc).
func (s schedule) applyToPatch(patch *worklet.TaskPatch) bool {
	touched := false
	if s.Start != nil {
		patch.StartAt, touched = s.Start, true
		patch.WaitReason = s.WaitReason
	} else if s.Cleared["start"] {
		patch.ClearStart, touched = true, true
	}
	if s.Due != nil {
		patch.DueAt, touched = s.Due, true
		patch.DueAllDay = s.DueAllDay
	} else if s.Cleared["due"] {
		patch.ClearDue, touched = true, true
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
