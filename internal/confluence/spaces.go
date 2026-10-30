package confluence

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// SpacesOf is every space the company document names: each unit's and each
// seat's `space`, in key order. It is the reach a seat's Confluence scope is
// held within, the counterpart of jira.ProjectsOf.
func SpacesOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	seen := map[string]bool{}
	for u := range o.AllUnits() {
		if key := NormalizeSpaceKey(u.Space); key != "" {
			seen[key] = true
		}
	}
	for seat := range o.AllRoles() {
		if key := NormalizeSpaceKey(seat.Space); key != "" {
			seen[key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// NormalizeSpaceKey is a space key as Confluence matches it: a global space's
// key upper-cased, as Confluence stores it, and a personal space's
// (`~username`, `~<account id>`) trimmed and otherwise left exactly as
// written, because Confluence matches those case-sensitively and an
// upper-cased one names no space at all.
func NormalizeSpaceKey(key string) string {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "~") {
		return key
	}
	return org.NormalizeScope(key)
}
