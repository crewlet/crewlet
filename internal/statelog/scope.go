package statelog

import (
	"slices"
	"strings"
)

// ScopeSeparator is the one thing the framework knows about a scope path.
//
// A domain chooses its own path alphabet — what a container is, what a family
// is, what an object is called — and the framework never names any of it. What
// it does know is that a path is a hierarchy written left to right with this
// separator between levels, which is exactly enough to compute CONTAINMENT:
// whether one path covers another.
//
// That is the whole seam. It lets the read levels and the write path's
// deferral probe be written before any domain exists, and it means a domain
// that adds a level of hierarchy adds it to its own strings rather than to the
// framework.
const ScopeSeparator = "/"

// Ancestors returns path and every path that contains it, longest first.
//
//	"project/ENG/task/7" -> ["project/ENG/task/7", "project/ENG/task",
//	                         "project/ENG", "project"]
//
// The closure is what makes a scope probe a SET MEMBERSHIP test rather than a
// pattern match: a record deferred on the container of an object this write is
// about must be found by a probe that only knows the object.
func Ancestors(path string) []string {
	path = strings.Trim(strings.TrimSpace(path), ScopeSeparator)
	if path == "" {
		return nil
	}
	parts := strings.Split(path, ScopeSeparator)
	out := make([]string, 0, len(parts))
	for i := len(parts); i > 0; i-- {
		out = append(out, strings.Join(parts[:i], ScopeSeparator))
	}
	return out
}

// Closure is every path in this scope plus every path that contains one,
// sorted and deduplicated.
//
// It is one half of the deferral probe, and only one: a stored deferred path
// found HERE is a path that COVERS something this operation is about, either
// exactly or as its container. It says nothing about a deferred path BENEATH
// one of these, which is what [ScopeSet.Roots] is for.
//
// It is NOT an intersection test. Two sibling paths share every ancestor above
// them, so comparing two closures for a common element answers true for every
// pair of objects in one company.
func (s ScopeSet) Closure() []string {
	seen := map[string]struct{}{}
	for _, p := range s.Paths {
		for _, a := range Ancestors(p) {
			seen[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Roots is every path in this scope with nothing above it inside the scope,
// normalised and sorted.
//
// It is the other half of the probe, and the direction the first half cannot
// see: a record deferred on an object BENEATH something this operation is
// about — one task inside a project this write is rewriting — is not in the
// closure, because the closure walks upward. A descendant test walks down.
//
// Only the roots are needed, because a descendant of a nested path is a
// descendant of its outermost ancestor in the set: testing both would ask the
// same question twice.
func (s ScopeSet) Roots() []string {
	norm := make([]string, 0, len(s.Paths))
	for _, p := range s.Paths {
		if p = strings.Trim(strings.TrimSpace(p), ScopeSeparator); p != "" {
			norm = append(norm, p)
		}
	}
	slices.Sort(norm)
	norm = slices.Compact(norm)

	out := make([]string, 0, len(norm))
	for _, p := range norm {
		covered := false
		for _, root := range out {
			if p == root || strings.HasPrefix(p, root+ScopeSeparator) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// Covers reports whether outer contains inner — the same path, or an ancestor
// of it.
//
// SHARING AN ANCESTOR IS NOT CONTAINMENT, and the distinction is the whole
// predicate: two sibling projects both sit under the same family, and a probe
// that treated that as an intersection would refuse every write in the company
// the moment one record anywhere was deferred.
func Covers(outer, inner string) bool {
	outer = strings.Trim(strings.TrimSpace(outer), ScopeSeparator)
	inner = strings.Trim(strings.TrimSpace(inner), ScopeSeparator)
	if outer == "" || inner == "" {
		return false
	}
	return outer == inner || strings.HasPrefix(inner, outer+ScopeSeparator)
}

// Intersects reports whether two scopes are about any object in common, under
// containment in BOTH directions.
//
// It is the predicate the SQL probe implements, written here as a pure
// function so the rule is testable without a database and so the two can be
// checked against each other. Getting it wrong in one direction is the
// difference between a refused write and a silent overwrite.
//
// The two directions are the probe's two clauses: a stored path in [Closure]
// is one that COVERS something the query is about, and a stored path under a
// [Roots] entry is one the query COVERS. Neither finds the other's case.
func (s ScopeSet) Intersects(other ScopeSet) bool {
	for _, p := range s.Normalised().Paths {
		for _, q := range other.Normalised().Paths {
			if Covers(p, q) || Covers(q, p) {
				return true
			}
		}
	}
	return false
}

// Normalised is this scope with its paths trimmed, sorted and deduplicated —
// what is STORED, so two records declaring the same objects in a different
// order produce the same rows.
func (s ScopeSet) Normalised() ScopeSet {
	out := make([]string, 0, len(s.Paths))
	for _, p := range s.Paths {
		if p = strings.Trim(strings.TrimSpace(p), ScopeSeparator); p != "" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return ScopeSet{Paths: slices.Compact(out)}
}
