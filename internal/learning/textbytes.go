package learning

import (
	"reflect"
	"time"
)

// textBytes is how many bytes of text a row read from the store carries: every
// string, list element and map key and value in it, through its structs,
// pointers and interfaces. A listing bounded by bytes counts each row with it,
// WALKED rather than listed field by field, so a text added to a row is counted
// without a second list to keep. Not the encoded size, which adds keys, quotes
// and numbers: the bound it feeds leaves room for the encoding.
func textBytes(v any) int { return walkText(reflect.ValueOf(v)) }

var timeType = reflect.TypeFor[time.Time]()

func walkText(v reflect.Value) int {
	n := 0
	switch v.Kind() {
	case reflect.String:
		n = v.Len()
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v.Len()
		}
		for i := range v.Len() {
			n += walkText(v.Index(i))
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			n += walkText(it.Key()) + walkText(it.Value())
		}
	case reflect.Struct:
		// An instant holds no text, and its location pointer leads to the
		// zone tables of the process, not the row.
		if v.Type() == timeType {
			return 0
		}
		for i := range v.NumField() {
			n += walkText(v.Field(i))
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			n = walkText(v.Elem())
		}
	}
	return n
}

// fitsBytes reports whether a listing holding held bytes in kept rows takes a
// row of size more under maxBytes. The FIRST ROW IS ALWAYS TAKEN, however
// large: a page that held nothing would show a seat whose newest row is huge
// as holding none, while its total says otherwise.
func fitsBytes(kept, held, size, maxBytes int) bool {
	return kept == 0 || held+size <= maxBytes
}
