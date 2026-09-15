package configapi_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// dashboardTree is the client source this gate reads — the SOURCE, never the
// built bundle, which is minified and carries no declaration to find.
const dashboardTree = "../../../dashboard/src"

var entityKindsDecl = regexp.MustCompile(`(?s)const ENTITY_KINDS = \[(.*?)\] as const`)

// WHATEVER IS BETWEEN THE QUOTES, never a shape that looks right. A class of
// `[a-z0-9-]+` does not match `mcp_servers`, so a kind spelled with the wrong
// separator was not extracted at all and the gate passed on a client that
// could never read that collection — which is the failure it exists to catch,
// one level up.
var kindField = regexp.MustCompile(`kind:\s*"([^"]*)"`)

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
	client := clientEntityKinds(t)
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

// clientEntityKinds reads the one declaration in the dashboard tree.
func clientEntityKinds(t *testing.T) []string {
	t.Helper()
	var found []string
	if err := walkClient(dashboardTree, func(source string) {
		if m := entityKindsDecl.FindStringSubmatch(source); m != nil {
			found = append(found, m[1])
		}
	}); err != nil {
		t.Fatalf("the dashboard tree at %s could not be walked, so this gate "+
			"certifies nothing: %v", dashboardTree, err)
	}
	if len(found) != 1 {
		t.Fatalf("%d files under %s declare ENTITY_KINDS, want exactly one — none is "+
			"a gate certifying nothing, and two are two copies that can drift from "+
			"each other as well as from the engine", len(found), dashboardTree)
	}
	var kinds []string
	for _, m := range kindField.FindAllStringSubmatch(found[0], -1) {
		kinds = append(kinds, m[1])
	}
	slices.Sort(kinds)
	return kinds
}

func walkClient(root string, each func(source string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := root + "/" + entry.Name()
		if entry.IsDir() {
			if err := walkClient(path, each); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx") {
			continue
		}
		if strings.Contains(entry.Name(), ".test.") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		each(string(source))
	}
	return nil
}
