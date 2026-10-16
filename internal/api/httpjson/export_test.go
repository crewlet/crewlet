package httpjson

import (
	"maps"
	"slices"
)

// PackagePrefixes is [goPackages] for the external test that holds it to the
// tree, sorted.
func PackagePrefixes() []string {
	return slices.Sorted(maps.Keys(goPackages))
}
