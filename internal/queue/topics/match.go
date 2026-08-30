package topics

import "strings"

// Match reports whether a subject matches a subscription pattern.
//
// The wildcard grammar is NATS's, which Crewlet's subject names were already
// written in: `*` matches exactly one segment, `>` matches one or more
// trailing segments and may only appear last. A pattern with no wildcards is
// an exact match.
//
// This lives here, beside the subject grammar itself, because a backend that
// implements matching privately is a backend that can disagree with another
// one about which events a dashboard sees — and a subscriber that quietly
// receives too few events looks exactly like a quiet company.
//
// A backend whose broker matches natively (JetStream) does not call this;
// it exists so the in-memory twin and any future backend without native
// matching agree with it rather than approximating it.
func Match(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	if pattern == "" || subject == "" {
		return false
	}

	pat := strings.Split(pattern, ".")
	sub := strings.Split(subject, ".")

	for i, token := range pat {
		switch token {
		case ">":
			// `>` is only a wildcard as the final token, and it needs at
			// least one segment to match — "a.>" does not match "a".
			return i == len(pat)-1 && len(sub) > i
		case "*":
			// Redundant, deliberately. No input can reach it: a `*` past
			// the end of the subject means len(pat) > i == len(sub), so
			// the length equality below already refuses. It stays because
			// this branch is the one place a later edit would be tempted
			// to read sub[i], and an out-of-range read here would panic
			// inside a delivery path rather than fail a match.
			if i >= len(sub) {
				return false
			}
		default:
			if i >= len(sub) || sub[i] != token {
				return false
			}
		}
	}
	return len(pat) == len(sub)
}
