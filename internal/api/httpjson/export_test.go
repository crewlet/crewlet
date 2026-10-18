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

// UnauthorizedReasons are the reasons `unauthorized` has a sentence of its own
// for, so the gate in this package's tests can hold them to the authority
// table's reasons in both directions.
func UnauthorizedReasons() []string { return slices.Sorted(maps.Keys(refusals)) }
