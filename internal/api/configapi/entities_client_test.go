package configapi_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// TestEntityKindsMatchTheClient holds the names the Configuration screen offers
// against the collections `config_entities` will actually answer for.
//
// THE FAILURE IS A SCREEN THAT ASKS FOR NOTHING. A kind the client spells its
// own way — `seats` for `roles`, `mcp_servers` for `mcp-servers` — is a
// bad-params refusal on every read, rendered as "the query failed" under a tab
// a reader opened on purpose. Nothing else in the tree compares the two, and
// the query went un-asked for long enough that the drift would have been
// invisible either way.
//
// The client list may be a SUBSET: a collection it chooses not to offer is a
// product decision. What it may never contain is a name the engine does not
// serve.
func TestEntityKindsMatchTheClient(t *testing.T) {
	engine := configapi.EntityKinds()

	body, err := clientsource.Literal("../"+clientsource.Tree, "ENTITY_KINDS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Field(body, "kind")
	slices.Sort(client)
	if len(client) == 0 {
		t.Fatal("the dashboard offers no entity kinds, so this gate certifies nothing")
	}

	for _, kind := range client {
		if !slices.Contains(engine, kind) {
			t.Errorf("the Configuration screen offers %q, which config_entities refuses: "+
				"the engine serves %v", kind, engine)
		}
	}
	var missing []string
	for _, kind := range engine {
		if !slices.Contains(client, kind) {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Logf("the engine serves %v, which the Configuration screen does not offer — "+
			"a product decision rather than a failure, but a collection nobody can reach",
			missing)
	}
}
