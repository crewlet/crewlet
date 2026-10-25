package api

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// A NODE WITH NO WRITER IS HANDED NO WRITE TOOL, and every read it serves.
//
// [readsOnly] names the per-actor seams by hand, and a seam added to
// [builtin.WorkDeps] and missed there would register its tools over a writer
// that is nil on exactly the node that hands out none — one in a maintenance
// mode. So every per-actor seam is read off the struct, set, and asserted
// cleared, and a read seam beside them asserted kept.
//
// Mutation: drop any one line of readsOnly and this goes red naming the seam.
func TestReadsOnlyClearsEveryWriterSeam(t *testing.T) {
	t.Parallel()
	actor := reflect.TypeFor[builtin.Actor]()
	var d builtin.WorkDeps
	v := reflect.ValueOf(&d).Elem()
	var seams []string
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if field.Type.Kind() != reflect.Func || field.Type.NumIn() != 1 ||
			field.Type.In(0) != actor {
			continue
		}
		seams = append(seams, field.Name)
		v.Field(i).Set(reflect.MakeFunc(field.Type, func([]reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.Zero(field.Type.Out(0))}
		}))
	}
	if len(seams) == 0 {
		t.Fatal("builtin.WorkDeps declares no per-actor seam, so this test " +
			"certifies nothing")
	}
	d.DefaultProject = func(string) string { return "" }
	got := reflect.ValueOf(readsOnly(d))
	for _, name := range seams {
		if !got.FieldByName(name).IsNil() {
			t.Errorf("readsOnly keeps builtin.WorkDeps.%s, so a node with no "+
				"writer registers its tools over a nil writer", name)
		}
	}
	if got.FieldByName("DefaultProject").IsNil() {
		t.Error("readsOnly cleared DefaultProject, a read seam the reads keep")
	}
}
