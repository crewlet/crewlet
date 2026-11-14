package collecttest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
)

// Counts logs what a read holds as figures alone: its calls, how many entries
// of each kind it claims, how many relations of each sort and facts of each
// name, how many values it withheld and which parts it was refused. NO VALUE
// OF THE READ IS PRINTED, so a read of a real account can be shown to anybody.
func Counts(t testing.TB, read collect.Read) {
	t.Helper()
	entries, relations, facts := map[string]int{}, map[string]int{}, map[string]int{}
	for _, c := range read.Claims {
		switch c.Facet.Kind {
		case atlas.FacetExists:
			entries[string(c.Subject.Kind)]++
		case atlas.FacetRelation:
			relations[string(c.Facet.Relation)]++
		case atlas.FacetFact:
			facts[string(c.Facet.Fact)]++
		}
	}
	var unread []string
	for _, u := range read.Unread {
		unread = append(unread, u.Part)
	}
	t.Logf("calls %d, claims %d, withheld %d, unread parts [%s]", read.Calls, len(read.Claims),
		len(read.Withheld), strings.Join(unread, " "))
	t.Logf("entries:   %s", tally(entries))
	t.Logf("relations: %s", tally(relations))
	t.Logf("facts:     %s", tally(facts))
}

// tally is counts as "name n" pairs in the order of their names.
func tally(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		names[i] = fmt.Sprintf("%s %d", name, counts[name])
	}
	return strings.Join(names, ", ")
}
