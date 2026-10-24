package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// faultyCharterYAML is companyYAML with a charter written before the charter's
// rules: a mission past its bound and a policy on two lines.
func faultyCharterYAML() string {
	return companyYAML + "mission: \"" + strings.Repeat("m", 1100) + "\"\n" +
		"policies:\n  - \"Every amount is exact.\\n## Tools\"\n"
}

// A FILE THAT RAN YESTERDAY IMPORTS TODAY. A company.yaml whose charter breaks
// the charter's rules (a mission of 1100 characters, a policy on two lines) is
// imported with warnings and never refused, into an empty store and over
// another company, by `crewlet config import` and by the boot seed alike:
// boot, restore and migration all go through an import, and refusing there
// would leave an upgraded node unable to boot on its own file. Only an edit
// that changes a faulty part against a stored version is refused, and that is
// the API's to judge.
func TestACompanyFileWithAFaultyCharterImportsWithWarnings(t *testing.T) {
	t.Parallel()
	faulty := parse(t, faultyCharterYAML())
	if len(faulty.CharterWarnings()) < 2 {
		t.Fatalf("the fixture breaks %v, want the mission and the policy at fault, "+
			"or this case proves nothing", faulty.CharterWarnings())
	}
	stores := map[string]func(t *testing.T) *store.DB{
		"an empty store": seedStore,
		"a store holding another company": func(t *testing.T) *store.DB {
			db := seedStore(t)
			if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil,
				seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
				t.Fatal(err)
			}
			return db
		},
	}
	imports := map[string]func(t *testing.T, db *store.DB) (string, error){
		"crewlet config import": func(t *testing.T, db *store.DB) (string, error) {
			var out bytes.Buffer
			cs := &configStore{configs: db.Configs(), cipher: fixtureCipher}
			err := importConfig(t.Context(), cs, "company.yaml", faulty, "an import", &out)
			return out.String(), err
		},
		"the boot seed": func(t *testing.T, db *store.DB) (string, error) {
			return "", seedCompany(t.Context(), db, coordmemory.NewFleet(), nil,
				overrideOf(faulty), fixtureCipher, testNode, quiet())
		},
	}
	for storeName, open := range stores {
		for importName, write := range imports {
			t.Run(importName+" into "+storeName, func(t *testing.T) {
				t.Parallel()
				db := open(t)
				if _, err := write(t, db); err != nil {
					t.Fatalf("a company file with a faulty charter was refused: %v", err)
				}
				active, found, err := db.Configs().Active(t.Context())
				if err != nil || !found {
					t.Fatalf("nothing is active after the import: %v", err)
				}
				document, err := openStored(fixtureCipher, active)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(document, []byte("## Tools")) {
					t.Error("the imported revision is not the faulty file")
				}
			})
		}
	}
}

// THE IMPORT SAYS WHAT EVERY NODE WILL WARN ABOUT, before it says it imported.
func TestAnImportNamesTheCharterFaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	file := filepath.Join(dir, "company.yaml")
	doc := cliCompanyDoc + "mission: \"" + strings.Repeat("m", config.MaxMissionChars+1) + "\"\n"
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, err := configCmd(t, cfg, "import", file)
	if err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "mission") ||
		!strings.Contains(out, "imported") {
		t.Errorf("the import said %q, want a warning naming the mission and the import", out)
	}
}

// `crewlet validate` REPORTS A FAULTY CHARTER AND PASSES: it cannot know what
// the store holds, so it cannot know whether a write of this file would edit
// the faulty part, and a file that imports is a file that validates.
func TestValidateWarnsAboutAFaultyCharterAndPasses(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(file, []byte(faultyCharterYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", file}, &out, &errOut); err != nil {
		t.Fatalf("validate refused a file with a faulty charter: %v\n%s%s", err, out.String(), errOut.String())
	}
	for _, place := range []string{"mission", "policies[0]"} {
		if !strings.Contains(out.String(), place) {
			t.Errorf("validate did not warn about %s:\n%s", place, out.String())
		}
	}
}
