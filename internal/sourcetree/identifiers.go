package sourcetree

import (
	"bytes"
	"fmt"
	"go/token"
)

// Identifiers is a prefilter for Go source on the identifiers a gate's
// matcher compares: a file whose bytes spell none of them cannot hold the
// construct, so a gate need not parse it.
//
// EXACT, not a heuristic, because Go has no escapes in an identifier: a name
// in the source is the bytes of the name, whether it is a selector, a callee,
// a composite literal's type or key, or a declaration. A STRING is not — an
// import path or a literal's value can be spelled with escapes the bytes
// never show — which is why a gate keyed on an import parses the imports
// instead. So a gate that hands this the same table its matcher reads skips
// exactly the files it would have parsed and found nothing in.
type Identifiers struct {
	names [][]byte
}

// MustIdentifiers is the [Identifiers] prefilter for names, each of which
// must be a Go identifier: a prefilter on anything else is not exact, and a
// gate built on one would skip a file its matcher would flag. It panics, as
// [regexp.MustCompile] does, on a name that is not one, or on no names.
func MustIdentifiers(names ...string) Identifiers {
	if len(names) == 0 {
		panic("sourcetree: an Identifiers prefilter of no names is one a " +
			"gate's table emptied out from under it — name what the matcher compares")
	}
	var ids Identifiers
	for _, name := range names {
		if !token.IsIdentifier(name) {
			panic(fmt.Sprintf("sourcetree: %q is not a Go identifier, so a "+
				"prefilter on its bytes is not exact", name))
		}
		ids.names = append(ids.names, []byte(name))
	}
	return ids
}

// In reports whether src spells any of the identifiers. The zero value names
// none and admits every file, the one answer that cannot hide a construct.
func (ids Identifiers) In(src []byte) bool {
	if len(ids.names) == 0 {
		return true
	}
	for _, name := range ids.names {
		if bytes.Contains(src, name) {
			return true
		}
	}
	return false
}
