package topics

import "strings"

// Match reports whether a subject matches a subscription pattern.
//
// The wildcard grammar is NATS's: `*` matches exactly one segment, `>` one or
// more trailing segments and only last. A pattern with no wildcards is exact.
//
// It lives beside the subject grammar because a backend matching privately can
// disagree with another about which events a dashboard sees, and a subscriber
// that receives too few events looks like a quiet company. A natively matching
// broker (JetStream) does not call it; the in-memory twin and any backend
// without native matching use it so they agree rather than approximate.
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
