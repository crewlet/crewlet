package api_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// THE DASHBOARD KNOWS EXACTLY THE SERVICES THE ENGINE SENDS: `OrgServices`
// member for member against [api.OrgServices], and its three unions value for
// value against the backends' own closed sets, in both directions.
//
// Which of the dashboard's surfaces exist turns on these values, so a value
// the engine sends that the client does not name is a sidebar drawn for a
// company it does not describe, and a value the client names that the engine
// never sends is a branch nobody can reach.
func TestTheDashboardKnowsExactlyTheServicesTheEngineSends(t *testing.T) {
	t.Parallel()
	heldBothWays(t, "OrgServices", reflect.TypeFor[api.OrgServices](), 3)

	for _, tc := range []struct {
		union  string
		engine []string
	}{
		{"TrackerBackend", backendNames(config.TrackerBackends)},
		{"KnowledgeBackend", backendNames(config.KnowledgeBackends)},
		{"ChatBackend", backendNames(config.ChatBackends)},
	} {
		t.Run(tc.union, func(t *testing.T) {
			t.Parallel()
			client, err := clientsource.Union(clientsource.Tree(t), tc.union)
			if err != nil {
				t.Fatal(err)
			}
			if len(tc.engine) == 0 {
				t.Fatal("the engine declares no backend, so this gate certifies nothing")
			}
			engine := slices.Sorted(slices.Values(tc.engine))
			slices.Sort(client)
			if !slices.Equal(engine, client) {
				t.Errorf("the dashboard's %s union is %v and the engine sends %v",
					tc.union, client, engine)
			}
		})
	}
}

func backendNames[T ~string](set []T) []string {
	out := make([]string, len(set))
	for i, b := range set {
		out[i] = string(b)
	}
	return out
}
