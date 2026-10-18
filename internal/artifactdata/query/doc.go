// Package query compiles the datastore's query grammar to SQL over the field
// index: equality, inequality, ordering comparisons, membership (`in`), array
// containment (`contains`, a chip set) and string prefix on indexed paths,
// combined by AND; one order path with the document id as the tie-break; a
// bounded limit; and a keyset cursor. Pure: it names no table it does not
// compile against and opens nothing.
//
// # An unindexed filter is refused, never answered by a scan
//
// A value inside a document body cannot be indexed, and a query that decodes
// every body is a full scan wearing an index's name. So every path a query
// names must be one the field index holds: a top-level field (every one is
// indexed) or a dotted path its collection declares. Anything else is refused
// [ErrNotIndexed], naming the path and the fix (declare it in the collection's
// indexed paths), rather than answered slowly.
//
// # What the index holds, and what that costs a long string
//
// One row per indexed scalar of a live document: its kind (`s` string, `n`
// number, `b` boolean, `z` null), its number as a float for numbers and
// booleans, and its text for strings truncated to [TextIndexBytes]. An array of
// scalars indexes each element, at `ord` one and up; a scalar sits at `ord`
// zero, so equality never matches an array's element and `contains` never
// matches a scalar. A top-level field holding nothing scalar (an empty list, an
// object) keeps one presence marker below ord zero, kind `p`, which no clause
// compiled here reads: it is what a schema change finds the documents holding a
// column through. A string literal longer than the truncation is matched on
// its truncated prefix in SQL and named in [Compiled.Recheck], so the reader
// re-checks the body: a long string is never a false positive.
package query
