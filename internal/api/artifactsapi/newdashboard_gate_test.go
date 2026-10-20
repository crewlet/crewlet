package artifactsapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/clientsource"
)

var updateCatalogue = flag.Bool("update-catalogue", false,
	"rewrite the New dashboard page's catalogue fixture from dashspec.Catalogue")

// newDashboardTestdata is where New dashboard's suite keeps the engine's
// catalogue and the boards it composes from it.
func newDashboardTestdata(t *testing.T) string {
	t.Helper()
	return filepath.Join(clientsource.Tree(t), "routes", "artifacts", "new", "dashboard", "testdata")
}

// NEW DASHBOARD COMPOSES FROM THE ENGINE'S CATALOGUE, so the fixture its
// suite composes from is the catalogue this build answers, exactly: a source,
// a parameter or a column the engine grew or dropped is a suite composing
// against a catalogue no engine serves.
func TestNewDashboardsCatalogueIsTheEngines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(newDashboardTestdata(t), "catalogue.json")
	want, err := json.MarshalIndent(dashspec.Catalogue(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *updateCatalogue {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not dashspec.Catalogue(): run go test ./internal/api/artifactsapi -run "+
			"TestNewDashboardsCatalogueIsTheEngines -update-catalogue, then the dashboard suite "+
			"with -u", path)
	}
}

// EVERY BOARD NEW DASHBOARD COMPOSES IS ONE THE ENGINE PUBLISHES: each
// ready-made board and a board of every figure every source suggests, as the
// dashboard's suite wrote them, held to the board format by the very read a
// publish validates with.
func TestNewDashboardComposesBoardsTheEngineAccepts(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(newDashboardTestdata(t), "composed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var boards map[string]json.RawMessage
	if err := json.Unmarshal(raw, &boards); err != nil {
		t.Fatal(err)
	}
	if len(boards) == 0 {
		t.Fatal("the suite composed no board")
	}
	for name, spec := range boards {
		_, err := dashspec.Read(specVersion(spec))
		var invalid *dashspec.Invalid
		switch {
		case errors.As(err, &invalid):
			t.Errorf("the %q board breaks the format: %v", name, invalid)
		case err != nil:
			t.Errorf("the %q board: %v", name, err)
		}
	}
}

// specVersion is a version holding one spec and nothing else.
type specVersion []byte

func (specVersion) Paths() []string { return []string{dashspec.EntryPath} }

func (specVersion) Essence(p string) (string, bool) {
	return dashspec.EssenceJSON, p == dashspec.EntryPath
}

func (v specVersion) Read(p string) ([]byte, error) {
	if p != dashspec.EntryPath {
		return nil, os.ErrNotExist
	}
	return v, nil
}
