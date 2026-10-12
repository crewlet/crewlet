package sandbox

import "strings"

// ValidBranch reports whether name is one git accepts for a branch: the rules
// `git check-ref-format --branch` applies, held to the name as written.
//
// A BRANCH A RUN NAMES IS AN IDENTIFIER THE ENGINE HANDS ON — to a person
// reading the run's record, and to a later run told to check it out — and the
// one authority on what names a branch is git. A name git refuses is not a
// branch anybody pushed: it is prose where a name was asked for ("the fix"
// has a space in it), or a revision rather than a branch (`main~1`, `@{u}`),
// and passed on it sends the next run looking for something that does not
// exist. Spelled out here rather than asked of a git binary, because the
// engine host need not have one and the box's own is the coding agent's.
//
// THE RULES, each git's own: no control character, space, `~`, `^`, `:`,
// `?`, `*`, `[` or `\` anywhere; no `..`, no `@{` and no `//`; no `/` at
// either end and no `.` at the end; no `/`-separated part that begins with
// `.` or ends with `.lock`; and — for a branch, where git reads them as
// something else — not `HEAD`, not `@`, and nothing that begins with `-`.
// What git allows past those (any other byte, non-ASCII included) is allowed
// here too.
func ValidBranch(name string) bool {
	switch {
	case name == "", name == "HEAD", name == "@", name[0] == '-', name[0] == '/':
		return false
	case strings.HasSuffix(name, "/"), strings.HasSuffix(name, "."):
		return false
	case strings.Contains(name, ".."), strings.Contains(name, "@{"), strings.Contains(name, "//"):
		return false
	}
	for i := range len(name) {
		if c := name[i]; c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return false
		}
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
