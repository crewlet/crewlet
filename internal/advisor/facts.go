package advisor

import "slices"

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

// factWords is every fact key a detector states. TestEveryFactADetectorStatesHasWords
// holds every key in this package's detectors to an entry.
var factWords = map[string]factWord{
	"absorbed":       {"Wakes for changes already read", UnitCount},
	"addressed":      {"Notices put to the person", UnitCount},
	"agent_comments": {"Agent comments", UnitCount},
	"agents":         {"Agents", UnitCount},
	"agents_open":    {"Open items all agents hold", UnitItems},
	"answers":        {"Answers", UnitCount},
	"auxiliary":      {"Tokens on learning", UnitTokens},
	"cap":            {"Round cap", UnitCount},
	"decided":        {"Decisions", UnitCount},
	"delivered":      {"Pull requests the webhook delivered", UnitCount},
	"entries":        {"Atlas entries", UnitCount},
	"fires":          {"Schedule runs", UnitCount},
	"hours":          {"Hours", UnitHours},
	"idle":           {"Idle", UnitCount},
	"logs":           {"Logs without a verified backup", UnitCount},
	"memory_bytes":   {"Memory ceiling", UnitBytes},
	"merges":         {"Merges put to people", UnitCount},
	"moot":           {"Answers that came too late", UnitCount},
	"moved":          {"Runs that moved work", UnitCount},
	"named":          {"Times named with no question", UnitCount},
	"nothing":        {"Turns that committed nothing", UnitTurns},
	"notices":        {"Notices", UnitCount},
	"open":           {"Open items", UnitItems},
	"pickups":        {"Pickups", UnitCount},
	"polled":         {"Pull requests found by polling", UnitCount},
	"pressing":       {"Pressing items", UnitItems},
	"questions":      {"Open questions", UnitCount},
	"ready":          {"Ready items waiting", UnitItems},
	"recommended":    {"Taken as recommended", UnitCount},
	"refused":        {"Turns refused on budget", UnitTurns},
	"runs":           {"Runs", UnitCount},
	"superseded":     {"Answers overtaken", UnitCount},
	"tokens":         {"Tokens", UnitTokens},
	"turns":          {"Turns", UnitTurns},
	"waiting":        {"Items waiting on it", UnitItems},
	"woke":           {"Wakes", UnitCount},
	"woken":          {"Agent wakes by a mention", UnitCount},
	"words":          {"Pickups answered in words only", UnitCount},
}

// FactLabel is how a fact is labelled, its key where it has no words.
func FactLabel(key string) string {
	if w, ok := factWords[key]; ok {
		return w.label
	}
	return key
}

// FactUnit is what a fact's number counts, a count where it has no words.
func FactUnit(key string) Unit {
	if w, ok := factWords[key]; ok {
		return w.unit
	}
	return UnitCount
}
