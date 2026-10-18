// Package query compiles the datastore's query grammar to SQL over the field
// index: equality, ordering comparisons, membership, contains and prefix on
// indexed paths, combined by AND, one order path with the document id as the
// tie-break, a bounded limit and a keyset cursor. An unindexed filter is refused
// naming the fix rather than answered by a scan. Pure.
package query
