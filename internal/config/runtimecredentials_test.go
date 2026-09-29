package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
)

// EVERY CREDENTIAL AN AUTHORED SEAT OR UNIT CARRIES IS ONE THE CHART SEALS.
//
// Two sets of `secret:"true"` tags exist and have to: this package's authored
// types, which the config surface masks by, and internal/org's runtime types,
// which the org chart walks a seat's runtime half by and seals every literal
// it finds. The half is built from the authored seat by [Role.Seat] and
// [org.SeatRuntime], so a credential field tagged here and not there is one
// the chart puts on its log in the clear — every node's rows, every snapshot,
// every backup — while this package's surface masks it and looks correct.
//
// So the two are held against each other THROUGH THE CONVERSION: every
// credential field of an authored seat is filled with a literal of its own,
// the seat is converted exactly as an import converts it, and the runtime
// walk must find every one of those literals and nothing else. Mutation: drop
// a tag from an org runtime field and the literal it holds goes unfound;
// tag a field there that is no credential and the walk finds a value nobody
// put in one.
//
// AND OF THE SAME KIND: a literal from a `secret:"content"` field must be
// walked as content and every other as a setting, because the chart seals a
// file's body whole and cuts a setting around its references — a file tagged
// content on one side and not the other reaches the box as the chart's own
// references strung together, or a header stops sending its token.
func TestEveryAuthoredCredentialIsOneTheChartSeals(t *testing.T) {
	t.Parallel()

	seatLiterals := literals{}
	role := Role{Name: "Seat", Handle: "seat"}
	fillCredentials(reflect.ValueOf(&role).Elem(), false, false, seatLiterals, nil)
	if !slices.Contains(slices.Collect(maps.Values(seatLiterals)), true) {
		t.Fatal("the seat filled no content field, so the kind half of this " +
			"case asserts nothing")
	}
	seatRuntime, err := org.SeatRuntime(role.Seat())
	if err != nil {
		t.Fatalf("encode the seat's runtime half: %v", err)
	}
	assertWalked(t, chart.KindSeat, seatRuntime, seatLiterals)

	// A UNIT'S OWN FIELDS ONLY: its seats and its child units are rows of
	// their own, and the import encodes a unit's half without them.
	unitLiterals := literals{}
	unit := Unit{Name: "Platform", ID: "platform"}
	fillCredentials(reflect.ValueOf(&unit).Elem(), false, false, unitLiterals,
		[]string{"Roles", "Children"})
	flat := unit.Unit()
	flat.Roles, flat.Children = nil, nil
	unitRuntime, err := org.UnitRuntime(flat)
	if err != nil {
		t.Fatalf("encode the unit's runtime half: %v", err)
	}
	assertWalked(t, chart.KindUnit, unitRuntime, unitLiterals)
}

// THE RUNTIME TYPES CAN BE WALKED FAITHFULLY: no credential field sits beneath
// a type that decodes itself, where an encoded walk cannot follow it.
func TestTheRuntimeHalvesAreWalkable(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{reflect.TypeOf(org.Role{}), reflect.TypeOf(org.Unit{})} {
		if err := secrets.Walkable(typ); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

// literals is every literal an authored credential field was filled with, and
// whether that field is CONTENT.
type literals map[string]bool

// assertWalked checks the runtime walk hands over exactly the literals put in,
// each as the kind of credential it was put in as.
func assertWalked(t *testing.T, kind chart.ObjectKind, runtime json.RawMessage,
	put literals) {

	t.Helper()
	if len(put) == 0 {
		t.Fatalf("the %s filled no credential field, so this case asserts nothing", kind)
	}
	found := literals{}
	if _, err := (org.RuntimeShape{}).Credentials(kind, runtime,
		func(path secrets.Path, value string) (string, error) {
			found[value] = path.Content()
			return value, nil
		}); err != nil {
		t.Fatalf("walk the %s's runtime half: %v", kind, err)
	}
	for literal, content := range put {
		walked, ok := found[literal]
		switch {
		case !ok:
			t.Errorf("the %s's runtime walk did not find %q — an authored "+
				"credential field whose runtime field carries no secret tag, so "+
				"the chart would write it to its log in the clear. The half: %s",
				kind, literal, runtime)
		case walked != content:
			t.Errorf("the %s's runtime walk handed %q over as content=%v, and "+
				"the authored field it came from is content=%v — the two tags "+
				"disagree about whether it is a file's body or a setting",
				kind, literal, walked, content)
		}
	}
	for value := range found {
		if _, ok := put[value]; !ok {
			t.Errorf("the %s's runtime walk found %q, which no authored "+
				"credential field holds — a runtime field tagged secret that is "+
				"not one", kind, value)
		}
	}
}

// fillCredentials sets every string an authored credential field can hold to a
// literal of its own, recording whether its field is content and allocating
// whatever it passes through on the way; skip names fields of the top-level
// struct to leave alone.
func fillCredentials(v reflect.Value, secret, content bool, out literals, skip []string) {
	switch v.Kind() {
	case reflect.String:
		if secret {
			literal := fmt.Sprintf("literal-%d", len(out))
			out[literal] = content
			v.SetString(literal)
		}
	case reflect.Pointer:
		if !secret && !holdsCredential(v.Type().Elem(), map[reflect.Type]bool{}) {
			return
		}
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillCredentials(v.Elem(), secret, content, out, nil)
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() || slices.Contains(skip, field.Name) {
				continue
			}
			fillCredentials(v.Field(i), secret || secrets.Field(field),
				content || secrets.ContentField(field), out, nil)
		}
	case reflect.Slice:
		if !secret && !holdsCredential(v.Type().Elem(), map[reflect.Type]bool{}) {
			return
		}
		element := reflect.New(v.Type().Elem()).Elem()
		fillCredentials(element, secret, content, out, nil)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), element))
	case reflect.Map:
		if !secret || v.Type().Key().Kind() != reflect.String {
			return
		}
		element := reflect.New(v.Type().Elem()).Elem()
		fillCredentials(element, secret, content, out, nil)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.ValueOf(fmt.Sprintf("KEY_%d", len(out))).
			Convert(v.Type().Key()), element)
		v.Set(m)
	}
}
