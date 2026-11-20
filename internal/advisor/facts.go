package advisor

import (
	"slices"
	"sync"
)

// Unit is what a fact's number counts.
type Unit string

// The units a fact is counted in.
const (
	UnitHours   Unit = "hours"
	UnitItems   Unit = "items"
	UnitTurns   Unit = "turns"
	UnitTokens  Unit = "tokens"
	UnitPercent Unit = "percent"
	UnitCount   Unit = "count"
	UnitMinutes Unit = "minutes"
	UnitBytes   Unit = "bytes"
)

// Units is every unit.
func Units() []Unit {
	return []Unit{UnitHours, UnitItems, UnitTurns, UnitTokens, UnitPercent, UnitCount, UnitMinutes, UnitBytes}
}

// Valid reports whether u is a unit this build names.
func (u Unit) Valid() bool { return slices.Contains(Units(), u) }

// factWord is how a fact is shown: a label and what its number counts.
type factWord struct {
	label string
	unit  Unit
}

// factWords is every fact key a detector states, built at first use, as
// explain.go's patterns are, off every push's hook. TestEveryFactADetectorStatesHasWords
// holds every key in this package's detectors to an entry.
var factWords = sync.OnceValue(func() map[string]factWord {
	return map[string]factWord{
		"absorbed":          {"Wakes for changes already read", UnitCount},
		"addressed":         {"Notices put to the person", UnitCount},
		"agent_comments":    {"Agent comments", UnitCount},
		"waits_day":         {"Waits in the last day", UnitCount},
		"paused_skips":      {"Runs skipped while the agent was paused", UnitCount},
		"missed_skips":      {"Runs missed while no server ran the schedules", UnitCount},
		"wait_minutes":      {"Item-minutes work waited for the seats", UnitMinutes},
		"busy_percent":      {"Busy share of the available time", UnitPercent},
		"idle_percent":      {"Idle share of the available time", UnitPercent},
		"items":             {"Items worked", UnitItems},
		"seats":             {"Seats", UnitCount},
		"limit_minutes":     {"Item-minutes held on the model's limit", UnitMinutes},
		"out_minutes":       {"Minutes the entry was out on its limit", UnitMinutes},
		"tokens_per_turn":   {"Tokens per turn", UnitTokens},
		"median_per_turn":   {"The company's median tokens per turn", UnitTokens},
		"fires_a_day":       {"Schedule runs a day", UnitCount},
		"schedules":         {"Schedules", UnitCount},
		"due_minutes":       {"Item-minutes work waited to start", UnitMinutes},
		"limit_hits":        {"Times the entry hit its limit", UnitCount},
		"seats_held":        {"Seats it held", UnitCount},
		"worst_day_minutes": {"Minutes out on its worst day", UnitMinutes},
		"standing_minutes":  {"Item-minutes the work wanted the seat or was held", UnitMinutes},
		"minutes":           {"Minutes waited this week", UnitMinutes},
		"quick_clears":      {"Alarms gone within ten minutes", UnitCount},
		"alarm_turns":       {"Turns an alarm woke", UnitTurns},
		"agents":            {"Agents", UnitCount},
		"agents_open":       {"Open items all agents hold", UnitItems},
		"answers":           {"Answers", UnitCount},
		"auxiliary":         {"Tokens on learning", UnitTokens},
		"blocked":           {"Items blocked on nothing anyone holds", UnitItems},
		"unchanged_hours":   {"Hours the oldest has stood unchanged", UnitHours},
		"twin_open":         {"Open items the least loaded seat on the job holds", UnitItems},
		"not_started":       {"Open items not yet started", UnitItems},
		"cap":               {"Round cap", UnitCount},
		"decided":           {"Decisions", UnitCount},
		"delivered":         {"Pull requests the webhook delivered", UnitCount},
		"entries":           {"Atlas entries", UnitCount},
		"fires":             {"Schedule runs", UnitCount},
		"hours":             {"Hours", UnitHours},
		"idle":              {"Idle", UnitCount},
		"logs":              {"Logs without a verified backup", UnitCount},
		"memory_bytes":      {"Memory ceiling", UnitBytes},
		"merges":            {"Merges put to people", UnitCount},
		"moot":              {"Answers that came too late", UnitCount},
		"moved":             {"Runs that moved work", UnitCount},
		"named":             {"Times named with no question", UnitCount},
		"nothing":           {"Turns that committed nothing", UnitTurns},
		"notices":           {"Notices", UnitCount},
		"open":              {"Open items", UnitItems},
		"pickups":           {"Pickups", UnitCount},
		"polled":            {"Pull requests found by polling", UnitCount},
		"pressing":          {"Pressing items", UnitItems},
		"questions":         {"Open questions", UnitCount},
		"ready":             {"Ready items waiting", UnitItems},
		"recommended":       {"Taken as recommended", UnitCount},
		"refused":           {"Turns refused on budget", UnitTurns},
		"runs":              {"Runs", UnitCount},
		"superseded":        {"Answers overtaken", UnitCount},
		"tokens":            {"Tokens", UnitTokens},
		"turns":             {"Turns", UnitTurns},
		"waiting":           {"Items waiting on it", UnitItems},
		"woke":              {"Wakes", UnitCount},
		"woken":             {"Agent wakes by a mention", UnitCount},
		"words":             {"Pickups answered in words only", UnitCount},
	}
})

// FactLabel is how a fact is labelled, its key where it has no words.
func FactLabel(key string) string {
	if w, ok := factWords()[key]; ok {
		return w.label
	}
	return key
}

// FactUnit is what a fact's number counts, a count where it has no words.
func FactUnit(key string) Unit {
	if w, ok := factWords()[key]; ok {
		return w.unit
	}
	return UnitCount
}
