package inbox

import "time"

// RetentionKey is the epoch key a person's inbox horizon reaches every
// domain's applier under: the company's `inbox.retention_days`, in whole
// days, with its default applied.
//
// ONE KEY FOR EVERY DOMAIN THAT KEEPS AN INBOX, because the horizon is the
// company's and not any one domain's: an applier declines to write a notice
// older than it, and a node's sweep deletes the notices that have aged past
// it, and the two must hold ONE horizon or an inbox grows back on every
// replay by the width of their disagreement. A key per domain would be a
// horizon per domain, and one person's inbox aging out on as many clocks as
// it has sources.
const RetentionKey = "inbox.retention_days"

// RetentionDefaultDays is how long a person's inbox notices are kept on a node
// that has not been handed a company, whose epoch states no horizon: a
// published company's always does ([RetentionKey]), with its own default
// applied — this one, the company setting's default (config's
// DefaultInboxRetentionDays, which a test holds it to) — so this is the
// horizon of a replay that runs before any company has.
//
// THE ONE HORIZON THAT DELETES A ROW DERIVED FROM A RECORD, and it is a
// MAILBOX rather than a record: the history those rows were derived from is
// untouched and answers for ever. A year, against the vendor's three months
// uncleared, because a notice costs almost nothing and a founder scrolling
// back a year is a thing that happens.
const RetentionDefaultDays = 365

// Horizon is how long a person's inbox notices are kept, read out of the epoch
// the state-log framework hands an applier: the company's
// `inbox.retention_days`, under [RetentionKey], and [RetentionDefaultDays]
// where the epoch states none. Zero for a horizon of no days, which keeps
// nothing back from a notice.
//
// ONE READING FOR EVERY DOMAIN THAT KEEPS AN INBOX, for [RetentionKey]'s reason:
// an applier that declines a notice past the horizon and a sweep that deletes
// one must hold one horizon, and two domains reading the key their own way
// would age one person's inbox on two clocks. FROM THE EPOCH RATHER THAN FROM
// CONFIG, because an applier reads no configuration at all: two nodes briefly
// on different epochs may write different notices, which is precisely why a
// domain's notice table is classed as diverging rather than compared by the
// identity claim.
func Horizon(epoch map[string]any) time.Duration {
	days := RetentionDefaultDays
	if raw, held := epoch[RetentionKey]; held {
		switch v := raw.(type) {
		case int:
			days = v
		case int64:
			days = int(v)
		case float64:
			days = int(v)
		}
	}
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}
