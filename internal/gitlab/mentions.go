package gitlab

import "strings"

// Finding `@username` in a markdown body.
//
// # Why a scanner and not a regular expression
//
// GitLab sends comment and description bodies as raw markdown with no parsed
// mention array — unlike a tracker, which resolves each mention to a user id
// before it sends the payload — so the only way to recover who was named is
// to read the text.
//
// The rule that makes that safe is a NEGATIVE one: an `@` counts only when
// what precedes it is not part of a word. Without it, `deploy@example.com`
// mentions a user called "example" and every file path with an @ in it pings
// somebody. Go's regexp engine is RE2 and has no lookbehind, so the rule
// cannot be written as a pattern — a one-pass scanner is not a workaround
// here, it is the only correct implementation.

// Mentions are the usernames named in a body, lowercased, deduplicated, in
// the order they appear.
//
// The order is kept because the first reason for a person wins downstream: a
// mentioned assignee should be told they were mentioned, which is the
// stronger claim on their attention.
//
// It is DELIBERATELY PERMISSIVE about what it returns — `@here`, `@all`, a
// username nobody has — because the caller intersects the result against the
// parties the engine can route to. Rejecting non-usernames here would mean
// this function had to know the workspace, and the intersection has to
// happen anyway.
func Mentions(text string) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	for i := 0; i < len(text); i++ {
		if text[i] != '@' || !boundaryBefore(text, i) {
			continue
		}
		name, next := username(text, i+1)
		if name == "" {
			continue
		}
		i = next - 1
		lower := strings.ToLower(name)
		if !seen[lower] {
			seen[lower] = true
			out = append(out, lower)
		}
	}
	return out
}

// boundaryBefore reports that the byte before an `@` does not continue a
// word.
//
// A `/` is excluded alongside the word bytes, which the character class
// alone would not do: a path fragment like `docs/@internal` is a file, not a
// person, and it is common enough in a code host's comment bodies to matter.
func boundaryBefore(text string, at int) bool {
	if at == 0 {
		return true
	}
	c := text[at-1]
	return !isWordByte(c) && c != '/'
}

// username reads a GitLab username starting at i, returning it and the index
// after it.
//
// GitLab's own rule: it begins and ends with alphanumeric, and may contain
// dots, underscores and hyphens in between. The trailing-punctuation rule is
// what makes `@ana.` at the end of a sentence mention "ana" rather than
// "ana." — and a trailing dot is how most mentions in prose actually end.
func username(text string, i int) (string, int) {
	if i >= len(text) || !isAlnum(text[i]) {
		return "", i
	}
	end := i
	for end < len(text) && (isAlnum(text[end]) ||
		text[end] == '.' || text[end] == '_' || text[end] == '-') {
		end++
	}
	// Walk back over the interior bytes that may not END a username.
	last := end
	for last > i && !isAlnum(text[last-1]) {
		last--
	}
	// `next` is the FULL extent rather than the trimmed one, and that is
	// a COST property rather than a correctness one: the boundary rule
	// already refuses every byte inside a token, so a scan that resumed
	// there would find the same nothing — just after re-reading the whole
	// token to do it. On a body of long dotted names that is quadratic.
	return text[i:last], end
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isWordByte(c byte) bool { return isAlnum(c) || c == '_' }
