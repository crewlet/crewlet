package topics

import "strings"

// The org chart's own log, and why its grammar sits beside the tracker's and
// the knowledge base's.
//
// The chart domain is the state-log framework's FOURTH, and its third strictly
// ordered one. What is particular to it is that ONE of its subjects carries
// the whole STRUCTURE — `crewlet.chart.log.tree` — while a unit's and a seat's
// own CONTENT each get a subject of their own. That split is the domain's, and
// internal/chart is where it is argued; what lives here is the GRAMMAR the two
// halves are composed into.
//
// It lives here for the reason the other two domains' does — the publisher
// builds the subject, the wake feed's consumer filters on it, and the applier's
// kind switch dispatches on the kind inside it, and none of the three can see
// the others. Written by hand in each place, a kind added in one and missed in
// another is not an error anywhere: the record publishes into a subject the
// filter does not cover, nobody is woken, and the company simply looks quiet.

const (
	// ChartLogStream is the org chart's mutation stream.
	ChartLogStream = "CREWLET_CHART_LOG"

	// ChartLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	ChartLogPrefix = "crewlet.chart.log"

	// ChartLogWildcard is the subject space the stream is created with.
	ChartLogWildcard = ChartLogPrefix + ".>"
)

// The object kinds are NOT constants here, for the reason the tracker's and
// the pages log's are not: a kind is one bare word — "tree", "unit", "seat" —
// and this package's own marker guard matches a dotless constant by shape, so
// exporting "seat" from here would fail the build on every string literal in
// the engine that happens to be that word. The kinds are a typed enum in
// internal/chart, which is the package that switches on them.

// ChartLogSubject builds the subject for one object.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// structure and the barrier both are. An empty KIND is not: it would publish
// to the prefix itself, a real subject inside the wildcard that the applier's
// switch has no case for, so it answers the empty string and callers must
// treat that as "not publishable" rather than as a subject.
func ChartLogSubject(kind, id string) string {
	if kind == "" {
		return ""
	}
	if id == "" {
		return ChartLogPrefix + "." + kind
	}
	return ChartLogPrefix + "." + kind + "." + id
}

// ChartLogPath recovers the kind and the id from a subject on the chart log,
// reporting whether the subject was one.
//
// The exact inverse of [ChartLogSubject]. The ID keeps its dots — a rekey's is
// the key an object is claiming, and a unit key is folded prose that may carry
// one — so only the FIRST segment is the kind, and splitting on every dot would
// recover an id of one fragment from a subject naming a whole key.
func ChartLogPath(subject string) (kind, id string, ok bool) {
	rest, found := strings.CutPrefix(subject, ChartLogPrefix+".")
	if !found || rest == "" {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, ".")
	if kind == "" || (found && id == "") {
		return "", "", false
	}
	return kind, id, true
}
