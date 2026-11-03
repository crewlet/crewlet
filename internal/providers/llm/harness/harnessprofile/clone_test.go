package harnessprofile

import (
	"reflect"
	"testing"
)

// A CLONE SHARES NOTHING A CALLER CAN WRITE THROUGH. Builtin hands out a clone
// because callers merge overrides into it, and a slice, map or pointer the
// clone still shares with the table lets one provider's overrides rewrite
// what every later provider reads. Clone lists its fields by hand, so a field
// added without a line there is shared silently — asked of every field by
// reflection here, so the next one cannot be missed.
func TestCloneSharesNoMutableField(t *testing.T) {
	t.Parallel()
	var original Profile
	fill(reflect.ValueOf(&original).Elem())
	clone := original.Clone()
	if !reflect.DeepEqual(original, clone) {
		t.Fatal("Clone changed a value it should only have copied")
	}
	assertUnshared(t, "Profile", reflect.ValueOf(original), reflect.ValueOf(clone))
}

// fill gives every slice, map and pointer under v one element, so a shared
// backing store is visible.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			fill(v.Field(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fill(key)
		elem := reflect.New(v.Type().Elem()).Elem()
		fill(elem)
		m.SetMapIndex(key, elem)
		v.Set(m)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	}
}

// assertUnshared fails on any slice, map or pointer the two values share.
func assertUnshared(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		for i := range a.NumField() {
			assertUnshared(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
		}
	case reflect.Slice:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares the original's backing array", path)
		}
		for i := range a.Len() {
			assertUnshared(t, path+"[]", a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares the original's map", path)
		}
	case reflect.Pointer:
		if !a.IsNil() && a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares the original's pointer", path)
		}
		if !a.IsNil() && !b.IsNil() {
			assertUnshared(t, path, a.Elem(), b.Elem())
		}
	}
}
