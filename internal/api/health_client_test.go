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
// [api.Health], which the `stream` query answers whole. It had drifted with
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
	members, err := clientsource.Interface(clientsource.Tree, "EngineHealth")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{} // member → optional
	for _, m := range members {
		declared[m.Name] = m.Optional
	}

	sent := map[string]bool{} // tag → omitted when empty
	typ := reflect.TypeFor[api.Health]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			t.Fatalf("api.Health.%s has no json name, so the wire key is the Go "+
				"field name and nothing on the client can be held to it", field.Name)
		}
		sent[name] = slices.Contains(strings.Split(opts, ","), "omitempty")
	}
	// A FLOOR, not a count: a gate that read nothing off either side must
	// not pass as agreement.
	if len(sent) < 10 || len(declared) < 10 {
		t.Fatalf("read %d field(s) off api.Health and %d member(s) off EngineHealth, "+
			"so this gate compares less than the envelope carries", len(sent), len(declared))
	}

	for name, omitted := range sent {
		optional, ok := declared[name]
		switch {
		case !ok:
			t.Errorf("api.Health sends %q and EngineHealth does not declare it, so no "+
				"screen can read it — add it to contract/health.ts", name)
		case omitted && !optional:
			t.Errorf("api.Health omits %q when it is empty and EngineHealth declares "+
				"it REQUIRED — make it optional, or every screen reading it reads "+
				"undefined as a value", name)
		}
	}
	for name := range declared {
		if _, ok := sent[name]; !ok {
			t.Errorf("EngineHealth declares %q and api.Health never sends it — a "+
				"renamed or removed field leaves exactly this behind", name)
		}
	}
}
