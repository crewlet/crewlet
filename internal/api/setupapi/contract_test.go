package setupapi_test

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE FORM READS WHAT THE SETUP WIRE SENDS: each declaration in
// `dashboard/src/contract/setup.ts` is held, both ways, to the JSON fields its
// Go type marshals, read off the tags; each union to its engine closed set.
func TestTheSetupFormReadsWhatTheSetupWireSends(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		iface string
		typ   reflect.Type
		// added is what a MarshalJSON writes beside the tags, and whether
		// it may be omitted.
		added map[string]bool
	}{
		{"SetupStep", reflect.TypeFor[setup.Step](), nil},
		{"SetupCaution", reflect.TypeFor[setup.Caution](), nil},
		{"SetupRequirement", reflect.TypeFor[setup.Requirement](), map[string]bool{"value": true}},
		{"SetupToolState", reflect.TypeFor[setupapi.ToolState](), nil},
		{"SetupSeatState", reflect.TypeFor[setupapi.SeatState](), nil},
		{"SetupSSHServers", reflect.TypeFor[setupapi.SSHServers](), nil},
		{"SetupSSHServer", reflect.TypeFor[setupapi.SSHServer](), nil},
		{"SetupSSHInventoryCommand", reflect.TypeFor[setupapi.SSHInventoryCommand](), nil},
		{"SetupVerdict", reflect.TypeFor[setup.Verdict](), nil},
		{"SetupVerdictFact", reflect.TypeFor[setup.Fact](), nil},
		{"SetupVerdictWarning", reflect.TypeFor[setup.Warning](), nil},
	} {
		members, err := clientsource.Interface(tree, tc.iface)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the client's declaration", err)
		}
		declared := map[string]bool{}
		for _, m := range members {
			declared[m.Name] = !m.Optional
		}
		sent := jsonFields(tc.typ)
		maps.Copy(sent, tc.added)
		for _, field := range slices.Sorted(maps.Keys(sent)) {
			if _, ok := declared[field]; !ok {
				t.Errorf("the engine sends %s on a %s and the client does not declare it", field, tc.iface)
			}
		}
		for _, field := range slices.Sorted(maps.Keys(declared)) {
			omitted, ok := sent[field]
			switch {
			case !ok:
				t.Errorf("%s declares %s and the engine never sends it, so every screen "+
					"that trusts the type reads it as undefined", tc.iface, field)
			case declared[field] && omitted:
				t.Errorf("%s declares %s REQUIRED and the engine omits it when empty", tc.iface, field)
			}
		}
	}
	for _, tc := range []struct {
		union string
		want  []string
	}{
		{"SetupRequirementKind", spelled(setup.Kinds)},
		{"VerifyOutcome", spelled(setup.VerifyOutcomes)},
	} {
		got, err := clientsource.Union(tree, tc.union)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s is %q in the client and %q in the engine", tc.union, got, tc.want)
		}
	}
}

// jsonFields is the JSON object a value of typ marshals to, by its tags: each
// field's name, mapped to whether it is omitted when empty. An embedded
// struct without a tag is flattened, as encoding/json flattens it.
func jsonFields(typ reflect.Type) map[string]bool {
	out := map[string]bool{}
	for f := range typ.Fields() {
		tag, tagged := f.Tag.Lookup("json")
		if f.Anonymous && !tagged && f.Type.Kind() == reflect.Struct {
			maps.Copy(out, jsonFields(f.Type))
			continue
		}
		if !f.IsExported() || tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = strings.Contains(options, "omitempty")
	}
	return out
}

// spelled is a closed set as the wire spells it.
func spelled[S ~[]E, E ~string](set S) []string {
	out := make([]string, len(set))
	for i, member := range set {
		out[i] = string(member)
	}
	return out
}
