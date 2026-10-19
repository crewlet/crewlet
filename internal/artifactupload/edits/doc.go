// Package edits applies old_str/new_str edits to one version's text, all or
// nothing: each old_str must occur exactly once in the text as it stands after
// the previous edits, the text must be UTF-8, and the result must stay inside
// the size limits. An edit names the BASE version it was written against and is
// never re-aimed at a newer head. Pure.
package edits
