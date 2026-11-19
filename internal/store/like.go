package store

import "strings"

// LikeEscape is the escape character every LIKE in this codebase uses.
//
// A backslash, and the SQL must say so: `LIKE ? ESCAPE '\'`. SQLite has no
// default escape character at all — without the clause, the escaping below
// would make the pattern worse rather than safer, since a literal backslash
// would then match a backslash and the wildcard it precedes would still be a
// wildcard.
const LikeEscape = `\`

// LikeContains builds a LIKE pattern matching rows that CONTAIN needle, with
// every wildcard in needle escaped so it matches itself.
//
// `"%" + text + "%"` is wrong for the text people type: `%` and `_` are LIKE's
// wildcards, so "100%" matches every row and "a_b" matches "axb". Not an
// injection (the value is still bound), but a wrong answer nothing explains.
//
// The pattern is meant to be bound to `column LIKE ? ESCAPE '\'`.
func LikeContains(needle string) string {
	return "%" + escapeLike(needle) + "%"
}

// LikePrefix builds a LIKE pattern matching rows that START with prefix.
func LikePrefix(prefix string) string {
	return escapeLike(prefix) + "%"
}

// escapeLike makes every LIKE metacharacter in s match itself.
//
// THE ESCAPE CHARACTER FIRST. Escaping `%` and `_` before the backslash would
// escape the backslashes this function just wrote, turning each into a literal
// backslash followed by an unescaped wildcard.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, LikeEscape, LikeEscape+LikeEscape)
	s = strings.ReplaceAll(s, "%", LikeEscape+"%")
	return strings.ReplaceAll(s, "_", LikeEscape+"_")
}
