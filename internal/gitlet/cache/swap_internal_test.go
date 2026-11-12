package cache

import (
	"reflect"
	"slices"
	"testing"
)

// EVERY FIELD OF AN INCARNATION IS CLASSIFIED FOR THE SWAP (swap.go's
// swapFields), both ways and in the struct's order: a field the table does
// not name is one a swap would silently leave behind — a verdict of the files
// it links lost, or a reader's state carried into a copy it never held — so a
// part that adds a field to incarnation cannot land it without saying what a
// swap does with it. Each entry is of a class there is, says why, and carries
// exactly when its class does: a carried field the promote has no carry for
// is one the promote never copies.
//
// Mutations: add a field to incarnation (named nowhere); drop a row (a field
// classified nowhere); a carried row with no carry.
func TestEveryIncarnationFieldIsClassifiedForTheSwap(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[incarnation]()
	var fields []string
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	var named []string
	for _, f := range swapFields {
		switch {
		case slices.Contains(named, f.name):
			t.Errorf("swapFields classifies %s twice", f.name)
		case !slices.Contains(fields, f.name):
			t.Errorf("swapFields classifies %s, which is no field of incarnation", f.name)
		}
		named = append(named, f.name)
		if !slices.Contains(swapClasses, f.class) {
			t.Errorf("%s is of class %q, which is none of %v", f.name, f.class, swapClasses)
		}
		if f.why == "" {
			t.Errorf("%s says no why: the next reader is the one tempted to change its class", f.name)
		}
		if carries := f.class == carried || f.class == carriedFiltered; (f.carry != nil) != carries {
			t.Errorf("%s is %s with a carry %t: a field is carried by its own carry exactly when its class "+
				"carries it", f.name, f.class, f.carry != nil)
		}
	}
	for _, name := range fields {
		if !slices.Contains(named, name) {
			t.Errorf("incarnation.%s is classified nowhere: say what a swap does with it in swapFields", name)
		}
	}
	if slices.Equal(slices.Sorted(slices.Values(fields)), slices.Sorted(slices.Values(named))) &&
		!slices.Equal(fields, named) {
		t.Errorf("swapFields lists the fields in another order than incarnation declares them:\n%v\nwant\n%v",
			named, fields)
	}
}
