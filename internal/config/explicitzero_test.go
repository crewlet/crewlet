package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// AN EXPLICIT ZERO A COMPANY MAY HOLD SURVIVES THE STORE.
//
// A stored revision is decoded onto the defaults ([DecodeCompany]), and every
// numeric knob is written `omitempty` — so a non-pointer knob whose default is
// not zero, and whose validator admits zero, loses an authored 0 on its first
// round trip: the encoder drops it, and the decode puts the default back. A
// company that switched the knob off had it switched back on by the first
// stored read, with nothing anywhere saying so. Such a knob is a pointer read
// through an accessor that applies the default (TurnEngine.ExecutorFloor is
// one), or its validator refuses the zero.
//
// WALKED, NOT LISTED: every numeric field of the company, at every depth the
// defaults reach, is set to zero in turn; a zero validation refuses AT THAT
// FIELD is fine, and any other must come back from the store as zero. A list
// of the knobs known today is exactly what let the next copy land.
func TestAnExplicitZeroSurvivesTheStore(t *testing.T) {
	t.Parallel()
	base := func() Company {
		c := DefaultCompany()
		c.Name = "Acme"
		return c
	}
	if err := ptr(base()).Validate(); err != nil {
		t.Fatalf("the defaults do not validate on their own: %v", err)
	}
	var fields []numericField
	numericFields(reflect.TypeOf(Company{}), nil, "", &fields)
	if len(fields) == 0 {
		t.Fatal("the walk found no numeric field: it is not reading the company")
	}
	checked := 0
	for _, f := range fields {
		c := base()
		v := reflect.ValueOf(&c).Elem().FieldByIndex(f.index)
		if v.IsZero() {
			continue // a zero default round-trips as itself
		}
		checked++
		v.SetZero()
		if err := c.Validate(); err != nil && strings.Contains(err.Error(), f.path+":") {
			continue // refused where it is written: no store ever holds it
		}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("%s: marshal: %v", f.path, err)
		}
		back, err := DecodeCompany(raw)
		if err != nil {
			t.Fatalf("%s: decode: %v", f.path, err)
		}
		if got := reflect.ValueOf(back).Elem().FieldByIndex(f.index); !got.IsZero() {
			t.Errorf("%s: an explicit 0 passes validation and comes back from the store as %v; "+
				"make it a pointer read through an accessor that applies the default, or "+
				"refuse the zero", f.path, got)
		}
	}
	if checked == 0 {
		t.Fatal("no numeric field has a non-zero default: the walk is not reading the defaults")
	}
}

func ptr[T any](v T) *T { return &v }

// numericField is one int or float field of the company, by its index path
// and its document path.
type numericField struct {
	index []int
	path  string
}

// numericFields collects every non-pointer numeric field reachable through
// nested structs (never through a map, a slice or a pointer: the defaults put
// nothing behind those, so a zero there round-trips as itself).
func numericFields(t reflect.Type, index []int, path string, out *[]numericField) {
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		at := name
		if path != "" {
			at = path + "." + name
		}
		idx := append(append([]int(nil), index...), i)
		switch sf.Type.Kind() {
		case reflect.Struct:
			if sf.Type == reflect.TypeOf(time.Time{}) {
				continue
			}
			numericFields(sf.Type, idx, at, out)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			*out = append(*out, numericField{index: idx, path: at})
		}
	}
}
