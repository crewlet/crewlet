// Package skillname is the one place a skill's name is said as a harness
// lists it, for the engine and the note helper alike: both must name a
// skill exactly as its harness does, the engine to tell when a home skill
// hides one of the run's (internal/sandbox/executor) and the helper to report
// a load under the name the harness used (internal/notehelper). It imports
// nothing but the standard library's strings, so the helper's builds stay
// small.
//
// Grok Build 1.0.92 lists a skill under [Grok] of its frontmatter `name`, or
// of its directory's name where that fails, or not at all ([GrokListed]); the
// rule was measured case by case on 1.0.92, and Grok's own docs understate
// it.
package skillname

import "strings"

// GrokMax is the most characters a name Grok Build lists may hold, counted
// after [Grok] (measured on 1.0.92: 65 fails, the cap applies to the result).
const GrokMax = 64

// Grok is s as Grok Build 1.0.92 rewrites a skill's name: ASCII letters
// lowered, each maximal run of characters outside [a-z0-9] (every non-ASCII
// character among them) made one '-', and '-' trimmed at both ends. The
// result may be empty or longer than [GrokMax], which Grok does not list.
func Grok(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}

// GrokListed is the name Grok Build 1.0.92 lists a skill under, its
// frontmatter name (named: whether it has a `name` key) and its directory's
// name given: [Grok] of the name where that is 1 to [GrokMax] characters,
// else of the directory's name where that is; false where neither is. A
// collision with another skill's listed name is the caller's to settle.
func GrokListed(name string, named bool, dir string) (string, bool) {
	if named {
		if n := Grok(name); n != "" && len(n) <= GrokMax {
			return n, true
		}
	}
	if n := Grok(dir); n != "" && len(n) <= GrokMax {
		return n, true
	}
	return "", false
}
