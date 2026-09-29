package config_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// regenerateDerived names the variable that writes the golden files instead of
// comparing against them. Only a person reviewing the diff sets it; CI never
// does, so a stale golden fails there.
const regenerateDerived = "CREWLET_REGENERATE_DERIVED"

// THE DERIVED HIERARCHY IS PINNED, CASE BY CASE, AGAINST FILES A PERSON READ.
//
// Every rule a dashboard must not re-implement is decided here once, so every
// one of them is exercised by a document written to exercise it, and by the
// examples a founder copies. A change to the org model that moves a manager,
// a lead or a report shows up as a diff in a file somebody has to accept.
func TestTheDerivedHierarchyMatchesItsGoldenFiles(t *testing.T) {
	t.Parallel()
	inputs, err := filepath.Glob(filepath.Join("testdata", "derived", "*.company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	examples, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// A glob that matched nothing would pass every case it did not find.
	if len(inputs) < 5 || len(examples) < 2 {
		t.Fatalf("found %d cases and %d examples, want at least 5 and 2", len(inputs), len(examples))
	}
	for _, input := range append(inputs, examples...) {
		golden := filepath.Join("testdata", "derived",
			strings.TrimSuffix(filepath.Base(input), ".company.yaml")+".derived.json")
		if strings.HasPrefix(input, filepath.Join("..", "..", "examples")) {
			golden = filepath.Join("testdata", "derived", "example-"+filepath.Base(golden))
		}
		t.Run(filepath.Base(input), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.ParseCompanyDocument(raw)
			if err != nil {
				t.Fatalf("parse %s: %v", input, err)
			}
			got, err := json.MarshalIndent(config.Derive(cfg), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			if os.Getenv(regenerateDerived) != "" {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read %s: %v", golden, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s is not what Derive answers for %s.\n\ngot:\n%s\n"+
					"If the change is intended, read the diff, then regenerate:\n"+
					"  %s=1 go test ./internal/config -run TestTheDerivedHierarchyMatchesItsGoldenFiles\n",
					golden, input, got, regenerateDerived)
			}
		})
	}
}

// THE ANONYMOUS FORM CARRIES NO PATH, and leaves the value it came from alone:
// a projection built from the same answer as a guarded one must not strip the
// guarded one's paths.
func TestTheDerivedHierarchyWithoutPaths(t *testing.T) {
	t.Parallel()
	cfg := parsed(t, "name: Acme\nroles:\n  - name: Designer\n    unit: product\nunits:\n  - name: Product\n")
	full := config.Derive(cfg)
	public := full.WithoutPaths()
	raw, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"path"`) || strings.Contains(string(raw), `"unit_path"`) {
		t.Errorf("the anonymous form carries a path: %s", raw)
	}
	if full.Seats[0].Path != "roles[0]" || full.Seats[0].UnitPath != "units[0]" || full.Units[0].Path != "units[0]" {
		t.Errorf("stripping the copy changed the original: %+v", full)
	}
	// PlacedByRef SURVIVES the strip, because the document it is a fact
	// about is the one this derivation read: what goes is the path a
	// reader has no way to point into, not the placement itself.
	if placed := public.Seats[0].PlacedByRef; placed == nil || !*placed ||
		!reflect.DeepEqual(public.Units[0].Seats, []string{"designer"}) {

		t.Errorf("the anonymous form lost what it is for: %+v", public)
	}
	var empty config.Derived
	if got := empty.WithoutPaths(); got.Seats != nil || got.Units != nil {
		t.Errorf("an empty hierarchy gained lists: %+v", got)
	}
}

// A RENAMED SEAT OR UNIT CARRIES THE ADDRESSES IT STILL ANSWERS TO.
//
// A link somebody kept names the address its seat or unit had when they kept
// it. With only the current handle and key on the projection, a client opening
// that link found nothing — and one falling back to the NAME opened whichever
// namesake came first. The aliases are the engine's own ([org.Organization.Role]
// and [org.Organization.Unit] resolve through them), stated only where a rename
// made them aliases, and they survive the anonymous strip.
func TestADerivedSeatAndUnitCarryTheAddressesARenameRetired(t *testing.T) {
	t.Parallel()
	renamed := &org.Role{Name: "Product Lead", DeclaredHandle: "lead",
		OriginHandle: "pm", FormerHandles: []string{"pm-2", "pm"}}
	never := &org.Role{Name: "Designer", DeclaredHandle: "designer", OriginHandle: "designer"}
	moved := &org.Unit{Name: "Product", ID: "product",
		OriginKey: "prod", FormerKeys: []string{"prod"}, Roles: []*org.Role{never}}
	still := &org.Unit{Name: "Ops", ID: "ops", OriginKey: "ops"}
	derived := config.DeriveFrom(&org.Organization{
		Name: "Acme", Roles: []*org.Role{renamed}, Units: []*org.Unit{moved, still},
	}).WithoutPaths()

	seats := map[string]config.DerivedSeat{}
	for _, s := range derived.Seats {
		seats[s.Handle] = s
	}
	if got := seats["lead"]; got.OriginHandle != "pm" ||
		!reflect.DeepEqual(got.FormerHandles, []string{"pm-2", "pm"}) {
		t.Errorf("the renamed seat carries origin %q and former %v", got.OriginHandle, got.FormerHandles)
	}
	if got := seats["designer"]; got.OriginHandle != "" || got.FormerHandles != nil {
		t.Errorf("a seat never renamed states an alias: %+v", got)
	}
	units := map[string]config.DerivedUnit{}
	for _, u := range derived.Units {
		units[u.ID] = u
	}
	if got := units["product"]; got.OriginKey != "prod" || !reflect.DeepEqual(got.FormerKeys, []string{"prod"}) {
		t.Errorf("the renamed unit carries origin %q and former %v", got.OriginKey, got.FormerKeys)
	}
	if got := units["ops"]; got.OriginKey != "" || got.FormerKeys != nil {
		t.Errorf("a unit never renamed states an alias: %+v", got)
	}
}
