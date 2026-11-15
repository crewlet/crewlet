package cron

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE SCHEDULE FORM CHECKS THE GRAMMAR THIS PACKAGE PARSES, field for field.
//
// The form names the field a mistake is in while it is typed ("The minute must
// be 0 to 59."), from a copy of this package's field table. A copy kept beside
// the form with nothing comparing it had drifted: it refused `6-7`, which
// [Parse] reads as the weekend, and read `JAN` in the weekday field. So every
// field is a row, in the engine's order, with the engine's bounds and the
// names it reads, each name at its value's place.
func TestTheDashboardReadsTheFieldsTheGrammarHas(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CRON_FIELDS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, f := range fields {
		want = append(want, f.name)
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("CRON_FIELDS is keyed %q and the grammar's fields are %q, in that order", keys, want)
	}
	for _, f := range fields {
		row, err := clientsource.Property(body, f.name)
		if err != nil {
			t.Fatal(err)
		}
		for bound, engine := range map[string]int{"min": f.min, "max": f.max} {
			if got := clientsource.Scalars(row, bound); !slices.Equal(got, []string{strconv.Itoa(engine)}) {
				t.Errorf("CRON_FIELDS[%q].%s is %q and the grammar's is %d", f.name, bound, got, engine)
			}
		}
		list, err := clientsource.Property(row, "names")
		if err != nil {
			t.Fatal(err)
		}
		// A NAME'S VALUE IS ITS PLACE in the dashboard's list, from the
		// field's least value.
		names := clientsource.Strings(list)
		read := make(map[string]int, len(names))
		for i, name := range names {
			read[name] = f.min + i
		}
		if !maps.Equal(read, f.aliases) && (len(read) > 0 || len(f.aliases) > 0) {
			t.Errorf("CRON_FIELDS[%q].names reads %v and the grammar reads %v", f.name, read, f.aliases)
		}
	}
}
