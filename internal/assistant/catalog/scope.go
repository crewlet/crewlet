package catalog

import (
	"strings"
)

// THE SCOPES: what each read's arguments narrowed it to, as the person reads
// it on the step and the card ([Tool.Scope]). Two reads of one tool are told
// apart by these words and nothing else, so every argument that changes which
// rows come back is said here, and nothing the person did not narrow (a page
// size) is.

// scopeOf joins a call's scope parts, leaving out the empty ones.
func scopeOf(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ", ")
}
