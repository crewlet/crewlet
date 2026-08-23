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
// Backends whose broker matches natively (JetStream, Pulsar) do not call
// this; it exists so the in-memory twin and any future backend without
// native matching agree with them rather than approximating them.
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
