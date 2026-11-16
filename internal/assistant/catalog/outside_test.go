package catalog_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// outsideTypes is the answer type each card with a step outside Crewlet reads
// it from; a new one is declared here or the build fails.
var outsideTypes = map[string]reflect.Type{
	"get_seat_access":        reflect.TypeFor[seataccess.LaneView](),
	"search_grant_catalogue": reflect.TypeFor[seataccess.LaneView](),
}

// A STEP OUTSIDE CREWLET IS ONLY AN ADDRESS THE ENGINE BUILT: a field tagged
// `link:"engine"`. Mutation: point it at `at_vendor.label` and this fails.
func TestAStepOutsideCrewletIsOnlyAnAddressTheEngineBuilt(t *testing.T) {
	t.Parallel()
	var drawn []string
	for _, tool := range catalog.Tools() {
		for variant, spec := range tool.Cards {
			if spec.Outside == nil {
				continue
			}
			if !slices.Contains(drawn, tool.Name) {
				drawn = append(drawn, tool.Name)
			}
			typ, declared := outsideTypes[tool.Name]
			switch {
			case !declared:
				t.Errorf("%s/%s draws a step outside Crewlet from an answer whose type "+
					"outsideTypes does not declare", tool.Name, variant)
			case !assistanttest.EngineLink(typ, spec.Outside.Path):
				t.Errorf("%s/%s draws %q as a step outside Crewlet, which %v does not declare "+
					"`link:\"engine\"`", tool.Name, variant, spec.Outside.Path, typ)
			}
		}
	}
	for name := range outsideTypes {
		if !slices.Contains(drawn, name) {
			t.Errorf("outsideTypes declares %s, which draws no step outside Crewlet", name)
		}
	}
}
