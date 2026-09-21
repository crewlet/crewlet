package org

import (
	"encoding/json"
	"reflect"
	"testing"
)

// THE RUNTIME DOCUMENT AND THE CHART'S ROWS PARTITION A SEAT.
//
// # What this is guarding
//
// A seat is stored twice over: its structure and identity as ROWS the chart
// domain owns, and everything else as one opaque document those rows carry.
// Every field of [Role] has to be in exactly one of those halves, and the
// failure of "neither" is the quiet one — a field nobody put in the document
// and nobody gave a column to is a setting an operator wrote, a validator
// accepted, and the running company never sees. There is no symptom: the seat
// exists, the value is in the file, and the engine behaves as though it were
// unset.
//
// "Both" is the other failure and it is not much better: two copies of one
// fact, of which the one inside an opaque blob is the copy nothing validates,
// nothing indexes and nothing can arbitrate — so a rename that moved the row
// leaves the old name inside the document, and the view reads whichever half
// it unpacked last.
//
// # Why it walks the type rather than checking a list
//
// A list would be a third copy to keep in step. This asks the encoder what it
// KEPT — by encoding a value whose every field is set and reading back which
// names survived — and holds that against the fields the rows are declared to
// own. A field added to [Role] is in neither until somebody decides, which is
// the decision this exists to force.
func TestTheRuntimeDocumentAndTheChartRowsPartitionASeat(t *testing.T) {
	t.Parallel()
	assertPartition(t, reflect.TypeFor[Role](), seatRowFields,
		func(v reflect.Value) (json.RawMessage, error) {
			return SeatRuntime(v.Interface().(*Role))
		})
}

// AND A UNIT, on the same terms.
func TestTheRuntimeDocumentAndTheChartRowsPartitionAUnit(t *testing.T) {
	t.Parallel()
	assertPartition(t, reflect.TypeFor[Unit](), unitRowFields,
		func(v reflect.Value) (json.RawMessage, error) {
			return UnitRuntime(v.Interface().(*Unit))
		})
}

// assertPartition holds one type's fields against the two halves.
//
// # It measures what the encoder CLEARS, not what the JSON contains
//
// The obvious version reads the encoded object's keys and calls a field
// "in the document" when its name is there. That is wrong in both directions
// on this type: a field with no `omitempty` emits its zero value after being
// cleared (so it looks present), and one with `omitzero` over a type whose
// state is unexported emits nothing however it was set (so it looks absent).
//
// So this fills a value, encodes it, decodes the result back, and asks which
// fields came back ZERO. A field the encoder cleared is one the chart's rows
// must own; a field that survived is one the document carries. Nothing about
// tags or omit rules enters into it.
func assertPartition(t *testing.T, typ reflect.Type, rows map[string]string,
	encode func(reflect.Value) (json.RawMessage, error)) {

	t.Helper()

	filled := reflect.New(typ)
	fill(filled.Elem(), 0)
	body, err := encode(filled)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	after := reflect.New(typ)
	if err := json.Unmarshal(body, after.Interface()); err != nil {
		t.Fatalf("the runtime document does not decode back: %v (%s)", err, body)
	}

	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		before := filled.Elem().Field(i)
		zero := reflect.Zero(field.Type).Interface()
		if reflect.DeepEqual(before.Interface(), zero) {
			// THE FILLER COULD NOT SET IT, so this case cannot tell
			// cleared from untouched. Reported rather than skipped:
			// silence here is how a field slips into neither half.
			t.Errorf("%s.%s could not be filled with a non-zero value, so "+
				"this gate cannot say which half owns it. Teach `fill` the "+
				"type, or give it a constructor.", typ.Name(), field.Name)
			continue
		}
		cleared := reflect.DeepEqual(after.Elem().Field(i).Interface(), zero)
		_, inRows := rows[field.Name]

		switch {
		case !cleared && inRows:
			t.Errorf("%s.%s is in BOTH halves: the rows own it (%s) and the "+
				"runtime document carries it too.\n"+
				"Two copies of one fact, and the one in the blob is the copy "+
				"nothing validates and nothing can arbitrate — a write that "+
				"moved the row would leave the blob's copy behind.",
				typ.Name(), field.Name, rows[field.Name])
		case cleared && !inRows:
			t.Errorf("%s.%s is in NEITHER half: the encoder clears it and no "+
				"row is declared to own it.\n"+
				"It is a setting an operator can write, a validator accepts, "+
				"and the running company never sees — with no symptom, because "+
				"the seat exists and the value is in the file. Either name the "+
				"row that owns it in this package's row-field table, or stop "+
				"clearing it.", typ.Name(), field.Name)
		}
	}
}

// fill sets every exported field to something non-zero, bounded by depth.
//
// BOUNDED because a unit contains units: an unbounded walk over [Unit] fills
// `Children[0].Children[0]…` for ever. Three levels is past every nesting the
// two types have.
func fill(v reflect.Value, depth int) {
	if depth > 3 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), depth+1)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		fill(key, depth+1)
		value := reflect.New(v.Type().Elem()).Elem()
		fill(value, depth+1)
		v.SetMapIndex(key, value)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), depth+1)
	case reflect.Struct:
		// A TYPE WITH ITS OWN CONSTRUCTOR first, because a struct whose
		// state is unexported cannot be filled field by field — and one
		// left zero would be reported as unfillable rather than placed.
		if toggle, ok := v.Addr().Interface().(*Toggle); ok {
			*toggle = On()
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), depth+1)
			}
		}
	}
}
