package config_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
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
	examplesDir := filepath.Join(sourcetree.Root(t), "examples")
	examples, err := filepath.Glob(filepath.Join(examplesDir, "*.company.yaml"))
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
		if filepath.Dir(input) == examplesDir {
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
