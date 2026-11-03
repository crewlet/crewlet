package inbox

import "slices"

// SnoozeScope is what an inbox read does with the notices a person snoozed.
//
// THREE VALUES, because there are three questions. "My inbox" hides what the
// person said "not now" to — a snooze that the default read returned anyway
// would be a gesture that does nothing. "Everything" keeps them, marked. And
// "what have I put off" is ONLY them, which the two-valued flag this replaced
// could not ask: `include_snoozed` returned every notice with the snoozed ones
// among it, so the dashboard's Snoozed tab listed the whole inbox.
//
// A snooze whose time has come is NOT snoozed under any scope: it is back in
// the inbox, reported unsnoozed, and absent from `only` — see
// [Classifier.Classify].
type SnoozeScope string

const (
	// SnoozeExclude hides every live snooze — the inbox as a person works it.
	SnoozeExclude SnoozeScope = "exclude"
	// SnoozeInclude returns every notice, the snoozed ones marked.
	SnoozeInclude SnoozeScope = "include"
	// SnoozeOnly returns only the notices still asleep.
	SnoozeOnly SnoozeScope = "only"
)

// SnoozeScopes are the three, in the order a surface lists them.
var SnoozeScopes = []SnoozeScope{SnoozeExclude, SnoozeInclude, SnoozeOnly}

// Valid reports whether a scope off the wire is one this build knows. The
// empty string is not: a read that picked one of the three for an absent
// scope would answer the other two's question wrong, so a surface resolves an
// absent scope to its own default — [SnoozeExclude], which is what "my
// inbox" means — before it reads.
func (s SnoozeScope) Valid() bool { return slices.Contains(SnoozeScopes, s) }
