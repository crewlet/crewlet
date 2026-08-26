package github

import "strings"

// Finding `@login` in a markdown body.
//
// # Why a scanner and not a regular expression
//
// GitHub sends comment, description and review bodies as raw markdown with
// no parsed mention array — unlike a tracker, which resolves each mention to
// a user id before it sends the payload — so the only way to recover who was
// named is to read the text.
//
// The rule that makes that safe is a NEGATIVE one: an `@` counts only when
// what precedes it is not part of a word. Without it `deploy@example.com`
// mentions a user called "example" and every file path with an @ in it pings
// somebody. Go's regexp engine is RE2 and has no lookbehind, so the rule
// cannot be written as a pattern — a one-pass scanner is not a workaround
// here, it is the only correct implementation.
//
// # Two rules that are GitHub's alone
//
// A LOGIN IS ALPHANUMERIC AND HYPHENS, nothing else. The self-hosted host
// beside this one allows dots and underscores, and reading its grammar here
// would swallow the sentence after a mention: `@ana.  We should` yields
// "ana." on GitLab's rule and needs a trailing-punctuation walk-back to
// recover. GitHub's own rule makes the walk-back unnecessary and, more
// importantly, makes `@ana_bot` two tokens on GitHub and one on GitLab —
// which is a different person.
//
// `@org/team` IS A TEAM, NOT A PERSON, and it is the reason this scanner
// looks at what FOLLOWS a login as well as what precedes it. Read naively,
// `@crewlet/reviewers` mentions an account called "crewlet" — and on a
// company whose org name matches one of its own seats, that seat is woken by
// every team ping. A team is dropped rather than expanded: expanding it
// needs the members API, one call per mention on the inbound hot path, to
// produce a fan-out GitHub itself treats as lower-priority than a direct
// mention.

// Mentions are the logins named in a body, lowercased, deduplicated, in the
// order they appear.
//
// The order is kept because the first reason for a person wins downstream: a
// mentioned assignee should be told they were mentioned, which is the
// stronger claim on their attention.
//
// It is DELIBERATELY PERMISSIVE about what it returns — a login nobody has,
// an app's `[bot]` suffix stripped to its stem — because the caller
// intersects the result against the parties the engine can route to.
// Rejecting non-logins here would mean this function had to know the
// account list, and the intersection has to happen anyway.
func Mentions(text string) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	for i := 0; i < len(text); i++ {
		if text[i] != '@' || !boundaryBefore(text, i) {
			continue
		}
		name, next := loginAt(text, i+1)
		if name == "" {
			continue
		}
		i = next - 1
		if teamAt(text, next) {
			// `@org/team`. The org half is not a person, and the team
			// half is not routable — so this mention names nobody the
			// engine can reach, and saying so once is more useful than
			// waking whoever happens to share the org's name.
			continue
		}
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

// loginAt reads a GitHub login starting at i, returning it and the index
// after it.
//
// GitHub's own rule: alphanumerics and single hyphens, never leading or
// trailing with one. The trailing walk-back is what makes `@ana-` at the end
// of a hyphenated phrase mention "ana" rather than an account that cannot
// exist.
func loginAt(text string, i int) (string, int) {
	if i >= len(text) || !isAlnum(text[i]) {
		return "", i
	}
	end := i
	for end < len(text) && (isAlnum(text[end]) || text[end] == '-') {
		end++
	}
	last := end
	for last > i && text[last-1] == '-' {
		last--
	}
	// `next` is the FULL extent rather than the trimmed one, and that is
	// a cost property rather than a correctness one: the boundary rule
	// already refuses every byte inside a token, so a scan resuming there
	// would find the same nothing — just after re-reading the whole token
	// to do it.
	return text[i:last], end
}

// teamAt reports that a login is followed by `/slug`, making it a team
// mention.
//
// The slug has to START like one: `@ana/` at the end of a line, or `@ana/ `,
// is a person followed by a stray slash rather than a team nobody named.
func teamAt(text string, at int) bool {
	return at < len(text)-1 && text[at] == '/' && isAlnum(text[at+1])
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isWordByte(c byte) bool { return isAlnum(c) || c == '_' || c == '-' }
