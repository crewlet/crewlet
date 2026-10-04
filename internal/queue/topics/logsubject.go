package topics

import "strings"

// A record's subject on a state log, RELATIVE TO THE LOG'S OWN PREFIX.
//
// Every state log addresses its records the same way: the log's subject
// prefix, the record's kind, and the object's id — which keeps its dots, since
// an alias claim is "<OLDKEY>.<n>" and a page's revision "<page>.<n>". The
// grammar is the same on every log; only the prefix differs.
//
// # Why the builder takes the prefix rather than knowing it
//
// Layout 0 has one log per domain under a fixed prefix, and
// [TrackerLogSubject] and [PagesLogSubject] are this grammar bound to those
// prefixes. A partitioned layout gives a domain one log PER PARTITION, each
// under a prefix of its own ([PartitionLogPrefix]), so a record's subject is a
// function of which log it is published on — and a builder that knew one
// prefix could only ever name layout 0's. Written once here over any prefix,
// the publisher, the wake filter and the applier's dispatch agree about every
// log's subjects for the reason they agree about layout 0's: none of them
// spells the grammar itself.
//
// # What the prefix is
//
// A log's subject prefix as the grammar produces it — a layout-0 constant or
// [PartitionLogPrefix]'s answer — and never re-derived here. An EMPTY prefix
// is the grammar's "no such stream" and is answered as one: a subject under
// no prefix would be `kind.id`, a path in no log's subject space that the
// broker delivers to nobody.

// LogSubject builds the subject for one object on the log whose subject prefix
// is prefix.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// barrier is. An empty KIND is not: it would publish to the prefix itself, a
// real subject inside the log's wildcard that no applier's switch has a case
// for. Nor is a kind holding a dot, which [LogPath] would read back as a
// shorter kind and an id it never had. Both answer the empty string, which
// callers treat as "not publishable" rather than as a subject.
func LogSubject(prefix, kind, id string) string {
	if prefix == "" || kind == "" || strings.Contains(kind, ".") {
		return ""
	}
	if id == "" {
		return prefix + "." + kind
	}
	return prefix + "." + kind + "." + id
}

// LogPath recovers the kind and the id from a subject on the log whose subject
// prefix is prefix, reporting whether the subject was one.
//
// The exact inverse of [LogSubject]: true only for a subject that function
// could have produced under that prefix. The ID keeps its dots, so only the
// FIRST segment after the prefix is the kind — splitting on every dot would
// recover a kind of "alias" and an id of "ENG" from a subject naming ENG-4's
// second claim.
func LogPath(prefix, subject string) (kind, id string, ok bool) {
	if prefix == "" {
		return "", "", false
	}
	rest, found := strings.CutPrefix(subject, prefix+".")
	if !found || rest == "" {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, ".")
	if kind == "" || (found && id == "") {
		return "", "", false
	}
	return kind, id, true
}
