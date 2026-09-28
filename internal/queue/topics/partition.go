package topics

import (
	"strconv"
	"strings"
)

// The partitioned estate's logs, and why every one of their names carries the
// layout.
//
// A LAYOUT divides the replicated estate into SPACES of numbered partitions,
// and gives each partition one log per domain it carries — the tracker's
// mutations and their vectors in a tracker partition, the pages and theirs in
// a pages partition. Each of those logs is a stream of its own with a subject
// space of its own, and the three parties that compare against the single
// logs' names above compare against these too without seeing each other: the
// provisioner creates the stream, the publisher writes under its prefix, and a
// consumer filters on its wildcard. So the grammar is here, once.
//
//	stream           CREWLET_L<n>_<SPACE>_<NNN>_<DOMAIN>   CREWLET_L1_TRACKER_007_VECTORS
//	subject prefix   crewlet.l<n>.<space>.<nnn>.<domain>   crewlet.l1.tracker.007.vectors
//	subjects         the prefix + ".>"
//	partition        <space>.<nnn>                         tracker.007
//
// # The layout number is in every name
//
// A repartition does not rename a stream. It creates a DISJOINT set under the
// next layout number and seals the old one, so one stream name never means two
// histories. Reusing a name across layouts would be the recreated-stream
// hazard the state-log framework's package doc spends a page defending
// against: a position recorded against the old stream would be a plausible
// number in the new one, and nothing comparing the two could tell.
//
// # Layout 0 is today's three logs, under today's names
//
// Layout 0 is the estate as it has always been — one space, `estate`, whose
// one partition carries all three domains — and its names are NOT this
// grammar's. They are the constants the single logs have always had
// (TrackerLogStream, TrackerVectorsStream, PagesLogStream and their prefixes
// and wildcards), answered FROM those constants rather than spelled again, so
// a node running layout 0 addresses the streams a running fleet already holds
// and no build between here and the first partitioned layout renames one.
// Layout 0 names only those three logs, and only in estate.000: anything else
// asked of it is not a stream it has, and aliasing a partitioned-looking
// request onto today's stream would put two histories behind one name.
//
// # Injective, and why a space and a domain are lowercase words
//
// Two different requests must never name one stream or overlap one subject
// space. A space and a domain are therefore each a WORD — a lowercase letter,
// then lowercase letters or digits — and the index is exactly three digits:
//
//   - no `.`, `*`, `>` or whitespace, because each is a subject token;
//   - no `_`, because `_` separates the stream name's parts: with it, space
//     `a` and domain `b_007_c` in partition 7 and space `a_007_b` with domain
//     `c` in the same partition would both be CREWLET_L1_A_007_B_007_C;
//   - no capital, because the stream spells both parts in capitals and
//     upper-casing must not merge `Tracker` with `tracker`.
//
// Split on `_`, every stream name then has exactly five parts and every
// subject prefix exactly five tokens, each part read back unambiguously —
// which is the whole proof that the grammar is injective. The second token,
// `l<n>`, is also what keeps every partitioned log out of every other
// crewlet.* namespace: no other one begins `l` and a digit.
//
// A request the grammar cannot name is answered with the empty string, as
// everywhere in this package, and callers must treat that as "no such
// stream" rather than as a name.
//
// # Why the two roots are constants named …Root
//
// This package's guard derives what a hand-built name looks like from its own
// constants. A partitioned name's committed head is followed by a NUMBER — the
// layout's — so it is not bounded by a separator the way every other marker
// is, and the guard cannot tell `crewlet.l1.` from `crewlet.log` by
// separators alone. A constant whose name ends in `Root` is how the grammar
// declares such a head, and the guard flags it wherever a digit, a formatting
// verb or the end of the literal follows it. Unexported, because nothing
// outside this file should compose a name from a root: the builders below are
// the whole interface.
const (
	partitionLogStreamRoot  = "CREWLET_L"
	partitionLogSubjectRoot = "crewlet.l"
)

// MaxPartitionIndex is the highest index the grammar can name: a partition's
// index is written in exactly three digits in every stream, subject and file
// name, so the grammar holds a thousand partitions in a space and no more.
// Three digits rather than a width that grows, because a name's width then
// never depends on the count — `tracker.007` sorts before `tracker.010` as a
// string, which is the order every listing of partitions is read in.
const MaxPartitionIndex = 999

// PartitionName is the name of the index'th partition of a space: the space,
// a dot, and the index in three digits — `tracker.007`.
//
// It is two tokens of every subject its partition's logs carry, which is why
// its spelling is this package's rather than the caller's. The empty string
// for a space that is not a word or an index outside 0…MaxPartitionIndex.
func PartitionName(space string, index int) string {
	if !isWord(space) || index < 0 || index > MaxPartitionIndex {
		return ""
	}
	return space + "." + threeDigits(index)
}

// ParsePartitionName recovers the space and the index from a partition's
// name, reporting whether it was one.
//
// The exact inverse of [PartitionName]: true only for a name that function
// could have produced, so `tracker.7`, `tracker.0007` and `Tracker.007` are
// all refused. A lenient reading would give one partition several names, and
// every place that keys on the name — a register row, a map entry, a lease —
// would then hold it more than once.
func ParsePartitionName(name string) (space string, index int, ok bool) {
	space, digits, found := strings.Cut(name, ".")
	if !found || !isWord(space) || len(digits) != 3 {
		return "", 0, false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return "", 0, false
		}
	}
	index, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	return space, index, true
}

// PartitionLogStream names the stream of one domain's log in one partition
// of a layout: CREWLET_L1_TRACKER_007_TRACKER, or under layout 0 the name the
// domain's single log has always had.
func PartitionLogStream(layout int, space string, index int, domain string) string {
	if layout == 0 {
		stream, _, _ := layoutZeroLog(space, index, domain)
		return stream
	}
	if !partitionable(layout, space, index, domain) {
		return ""
	}
	return partitionLogStreamRoot + strconv.Itoa(layout) +
		"_" + strings.ToUpper(space) +
		"_" + threeDigits(index) +
		"_" + strings.ToUpper(domain)
}

// PartitionLogPrefix is what a record's own path is appended to on that log:
// crewlet.l1.tracker.007.tracker, or under layout 0 the domain's own prefix.
// The partition's [PartitionName] is its middle two tokens.
func PartitionLogPrefix(layout int, space string, index int, domain string) string {
	if layout == 0 {
		_, prefix, _ := layoutZeroLog(space, index, domain)
		return prefix
	}
	if !partitionable(layout, space, index, domain) {
		return ""
	}
	return partitionLogSubjectRoot + strconv.Itoa(layout) +
		"." + PartitionName(space, index) +
		"." + domain
}

// PartitionLogWildcard is the subject space that log's stream is created
// with: its prefix and `.>`, or under layout 0 the domain's own wildcard.
func PartitionLogWildcard(layout int, space string, index int, domain string) string {
	if layout == 0 {
		_, _, wildcard := layoutZeroLog(space, index, domain)
		return wildcard
	}
	prefix := PartitionLogPrefix(layout, space, index, domain)
	if prefix == "" {
		return ""
	}
	return prefix + ".>"
}

// layoutZeroLog answers layout 0's names for one log, from the constants the
// three single logs have always had — or three empty strings for a log
// layout 0 does not have.
//
// The words are written HERE, inside a function, and never as package
// constants: the guard derives a marker from every top-level constant in this
// package, and a dotless one is matched by shape — so a constant spelling
// `tracker` would fail the build on every literal in the engine that happens
// to be the word. It is the reason the object kinds are kept out of this
// package too (see tracker.go).
//
// They are the domains' own Name()s and the state-log framework's layout-0
// space, neither of which this package may import. Two tests hold them
// against the originals: internal/statelog's, that layout 0's estate space
// names these streams, and internal/engine's, that every registered domain's
// log in layout 0 is the stream that domain declares today.
func layoutZeroLog(space string, index int, domain string) (stream, prefix, wildcard string) {
	const estate = "estate"
	if space != estate || index != 0 {
		return "", "", ""
	}
	switch domain {
	case "tracker":
		return TrackerLogStream, TrackerLogPrefix, TrackerLogWildcard
	case "vectors":
		return TrackerVectorsStream, TrackerVectorsPrefix, TrackerVectorsWildcard
	case "pages":
		return PagesLogStream, PagesLogPrefix, PagesLogWildcard
	}
	return "", "", ""
}

// partitionable reports whether a partitioned layout's grammar can name this
// log injectively. See the file's doc for why each part is what it is.
func partitionable(layout int, space string, index int, domain string) bool {
	return layout >= 1 && PartitionName(space, index) != "" && isWord(domain)
}

// isWord reports a lowercase letter followed by lowercase letters or digits.
func isWord(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// threeDigits renders an index already checked to be 0…MaxPartitionIndex.
func threeDigits(index int) string {
	s := strconv.Itoa(index)
	return strings.Repeat("0", 3-len(s)) + s
}
