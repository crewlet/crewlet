package topics

import "strings"

// A record's subject on a state log, RELATIVE TO THE LOG'S OWN PREFIX.
//
// Every log: the subject prefix, the record's kind, and the object's id, which
// keeps its dots ("<OLDKEY>.<n>", "<page>.<n>"). The builder takes the prefix
// because the framework runs every domain, a test's included, under one no
// constant here names; none of publisher, wake filter or applier spells the
// grammar, so they agree. [TrackerLogSubject], [PagesLogSubject],
// [IamLogSubject], [ArtifactsLogSubject], [ArtifactDataLogSubject],
// [AssistantLogSubject] and [ChatLogSubject] bind it to those logs' prefixes.
// The prefix is its domain's, never re-derived. An EMPTY prefix is "no such
// stream": a subject under none would be `kind.id`, delivered to nobody.

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
