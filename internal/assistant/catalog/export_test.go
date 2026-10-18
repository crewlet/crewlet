package catalog

import (
	"maps"
	"slices"
)

// What the external tests reach of the package's own decisions.
var (
	// NarrowingQuery is [narrowingQuery].
	NarrowingQuery = narrowingQuery
	// Narrowing is [narrowing].
	Narrowing = narrowing
)

// ProbeScreens are the screens [Open] decides through a read of the record
// itself ([probes]), sorted.
func ProbeScreens() []string { return slices.Sorted(maps.Keys(probes)) }
