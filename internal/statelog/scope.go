package statelog

import (
	"slices"
	"strings"
)

// ScopeSeparator is the one thing the framework knows about a scope path.
//
// A domain chooses its own path alphabet; the framework knows only that a path
// is a hierarchy written left to right with this separator between levels,
// enough to compute CONTAINMENT. So the read levels and the deferral probe
// need no domain, and a domain adds a level to its own strings, not to the
// framework.
const ScopeSeparator = "/"

// ScopePath builds a scope path from its segments, skipping empty ones so a
// missing id can never produce a path with a hole in it.
//
// EVERY DOMAIN WHOSE PATH CAN MISS A SEGMENT (an object's container, say)
// builds it here: a copy that kept a blank segment would build
// `project//task/7` where the others build `project/task/7`, and [Ancestors]
// and the deferral probe would read two paths for one object, letting a write
// step over a record deferred on its container.
func ScopePath(segments ...string) string {
	kept := make([]string, 0, len(segments))
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, ScopeSeparator)
}

// Ancestors returns path and every path that contains it, longest first.
//
//	"project/ENG/task/7" -> ["project/ENG/task/7", "project/ENG/task",
//	                         "project/ENG", "project"]
//
// The closure makes a scope probe a SET MEMBERSHIP test rather than a pattern
// match: a record deferred on the container of an object this write is about
// must be found by a probe that only knows the object.
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
// It is one half of the deferral probe: a stored deferred path found HERE
// COVERS something this operation is about, exactly or as its container. A
// deferred path BENEATH one of these is [ScopeSet.Roots]'s half.
//
// It is NOT an intersection test: two siblings share every ancestor above
// them, so comparing closures answers true for every pair of objects in one
// company.
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
// It is the probe's other half: a record deferred BENEATH something this
// operation is about (one task inside a project this write rewrites) is not in
// the upward-walking closure; a descendant test walks down from these. Only
// roots are needed, since a descendant of a nested path is a descendant of its
// outermost ancestor in the set.
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
// SHARING AN ANCESTOR IS NOT CONTAINMENT: two sibling projects sit under one
// family, and a probe that read that as an intersection would refuse every
// write in the company once one record anywhere was deferred.
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
// It is the predicate the SQL probe implements, kept pure so the rule is
// testable without a database and the two can be checked against each other:
// wrong in one direction is a refused write, in the other a silent overwrite.
// The directions are the probe's two clauses: a stored path in [Closure]
// COVERS something the query is about, and one under a [Roots] entry is
// COVERED by it. Neither finds the other's case.
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
