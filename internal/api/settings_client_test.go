package api_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD READS EXACTLY THE SETTINGS THE ENGINE SENDS: both interfaces
// member for member against their Go structs, and the two unions value for
// value against the engine's closed sets, in both directions. A kind the
// engine adds and the client does not name is a row no screen can type.
func TestTheDashboardReadsExactlyTheSettingsTheEngineSends(t *testing.T) {
	t.Parallel()
	heldBothWays(t, "SettingsAnswer", reflect.TypeFor[queries.SettingsAnswer](), 1)
	heldBothWays(t, "SettingRow", reflect.TypeFor[queries.SettingRow](), 8)
	for _, tc := range []struct {
		union  string
		engine []string
	}{
		{"SettingGroup", backendNames(queries.SettingGroups)},
		{"SettingKind", backendNames(queries.SettingKinds)},
	} {
		client, err := clientsource.Union(clientsource.Tree(t), tc.union)
		if err != nil {
			t.Fatal(err)
		}
		engine := slices.Sorted(slices.Values(tc.engine))
		slices.Sort(client)
		if !slices.Equal(engine, client) {
			t.Errorf("the dashboard's %s union is %v and the engine sends %v",
				tc.union, client, engine)
		}
	}
}
