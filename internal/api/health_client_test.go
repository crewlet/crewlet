package api_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD DECLARES EXACTLY THE HEALTH THE ENGINE REPORTS.
//
// `EngineHealth` (`contract/health.ts`) is the dashboard's type for
// [api.Health], which the `health` push carries whole — and `HealthAlarms` its
// type for the alarm count nested in it. It had drifted with
// nothing to say so: `stall_lag_seconds` and `unproven_seconds` — the lag that
// climbs towards the watchdog ending the process, and the seats a teardown
// stranded — were on the wire and in no declaration, so no screen could read
// either without first discovering the field exists.
//
// BOTH WAYS, and ONE MORE RULE. Every JSON tag on the struct is a member of the
// interface and every member is a tag. And a field the engine OMITS when empty
// is optional in the interface: a required member the wire can leave out is a
// type promising every screen a value that is not there.
//
// Read off the struct with reflection rather than restated here, so a field
// added to [api.Health] is covered the moment it compiles.
func TestTheDashboardDeclaresExactlyTheHealthTheEngineReports(t *testing.T) {
	t.Parallel()
	// The envelope, and the one struct it nests that the contract declares
	// itself. `seeded_from` is eventfan's Coverage, held by that package's
	// own gate against contract/coverage.ts.
	for _, pair := range []struct {
		declaration string
		typ         reflect.Type
		floor       int
	}{
		{"EngineHealth", reflect.TypeFor[api.Health](), 10},
		{"HealthAlarms", reflect.TypeFor[api.HealthAlarms](), 2},
	} {
		t.Run(pair.declaration, func(t *testing.T) {
			t.Parallel()
			heldBothWays(t, pair.declaration, pair.typ, pair.floor)
		})
	}
}

// heldBothWays compares one Go struct's JSON tags with one declaration's
// members, in both directions, with a floor so a gate that read nothing off
// either side cannot pass as agreement.
func heldBothWays(t *testing.T, declaration string, typ reflect.Type, floor int) {
	t.Helper()
	members, err := clientsource.Interface(clientsource.Tree, declaration)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{} // member → optional
	for _, m := range members {
		declared[m.Name] = m.Optional
	}

	sent := map[string]bool{} // tag → omitted when empty
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			t.Fatalf("%s.%s has no json name, so the wire key is the Go "+
				"field name and nothing on the client can be held to it", typ, field.Name)
		}
		sent[name] = slices.Contains(strings.Split(opts, ","), "omitempty")
	}
	// A FLOOR, not a count: a gate that read nothing off either side must
	// not pass as agreement.
	if len(sent) < floor || len(declared) < floor {
		t.Fatalf("read %d field(s) off %s and %d member(s) off %s, "+
			"so this gate compares less than the envelope carries",
			len(sent), typ, len(declared), declaration)
	}

	for name, omitted := range sent {
		optional, ok := declared[name]
		switch {
		case !ok:
			t.Errorf("%s sends %q and %s does not declare it, so no "+
				"screen can read it — add it to contract/health.ts", typ, name, declaration)
		case omitted && !optional:
			t.Errorf("%s omits %q when it is empty and %s declares "+
				"it REQUIRED — make it optional, or every screen reading it reads "+
				"undefined as a value", typ, name, declaration)
		}
	}
	for name := range declared {
		if _, ok := sent[name]; !ok {
			t.Errorf("%s declares %q and %s never sends it — a "+
				"renamed or removed field leaves exactly this behind", declaration, name, typ)
		}
	}
}
