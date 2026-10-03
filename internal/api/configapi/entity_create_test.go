package configapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// serversDoc declares one MCP server, so an add has something to keep in
// place and a name to collide with.
const serversDoc = companyDoc + `
mcp_servers:
  - name: github
    command: github-mcp
`

// createOnly is the header set an add sends: a summary and the create-only
// condition, `If-None-Match: *`.
func createOnly(summary string) map[string]string {
	return map[string]string{"X-Summary": summary, "If-None-Match": "*"}
}

// AN ADD IS A CREATE-ONLY WRITE AT THE NEW ENTITY'S OWN ADDRESS, and it lands
// AFTER every server already declared: the prompt lists servers in
// declaration order, so an add that reordered them would move every seat's
// tool block. Nothing else in the document moves either.
func TestACreateOnlyWriteAddsAnMCPServerAfterTheOthers(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)

	res := s.do(t, http.MethodPut, "/config/mcp-servers/linear",
		`{"name":"linear","transport":"http","url":"https://mcp.example.com","headers":{"Authorization":"${LINEAR_TOKEN}"}}`,
		createOnly("add the linear server"))
	if res.Code != http.StatusCreated {
		t.Fatalf("create-only PUT = %d, want 201: %s", res.Code, res.Body.String())
	}
	ids, err := s.service().Entities(t.Context(), configapi.EntityMCPServers)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"github", "linear"}) {
		t.Fatalf("servers after the add = %v, want github and linear", ids)
	}

	var stored struct {
		MCPServers []struct {
			Name    string            `json:"name"`
			Headers map[string]string `json:"headers"`
		} `json:"mcp_servers"`
		Providers struct {
			LLM map[string]json.RawMessage `json:"llm"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(s.activeDocument(t)), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.MCPServers) != 2 || stored.MCPServers[0].Name != "github" || stored.MCPServers[1].Name != "linear" {
		t.Errorf("the stored order is %+v, want github then linear", stored.MCPServers)
	}
	// THE REFERENCE IS STORED AS WRITTEN, never resolved into the revision.
	if got := stored.MCPServers[1].Headers["Authorization"]; got != "${LINEAR_TOKEN}" {
		t.Errorf("the header was stored as %q, want the reference", got)
	}
	if _, kept := stored.Providers.LLM["zulu"]; !kept || len(stored.Providers.LLM) != 1 {
		t.Errorf("the add touched the providers: %v", stored.Providers.LLM)
	}
}

// A PLAIN PUT STILL NEVER CREATES — the create is the request that says so.
// And `If-None-Match: *` on an entity route is about the ENTITY: it used to
// reach the document's own precondition and be refused `already_configured`,
// a sentence about the company when the caller had asked about one server.
func TestCreateOnlyAtAnEntityAddressIsAboutTheEntity(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)

	res := s.do(t, http.MethodPut, "/config/mcp-servers/linear",
		`{"name":"linear","command":"linear-mcp"}`,
		map[string]string{"X-Summary": "add sideways"})
	if res.Code != http.StatusNotFound {
		t.Fatalf("a PUT without the create condition = %d, want 404: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "If-None-Match") {
		t.Errorf("the 404 does not say how to add one: %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "already_configured") {
		t.Errorf("the entity route answered about the company: %s", res.Body.String())
	}
}

// AN ADD THAT FINDS ONE IS REFUSED, never turned into a replacement: the form
// that sent it never showed the existing server's launch command or
// credentials, so a replace would overwrite what nobody looked at.
func TestACreateOnlyWriteRefusesANameAlreadyTaken(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)
	before := s.activeDocument(t)

	res := s.do(t, http.MethodPut, "/config/mcp-servers/github",
		`{"name":"github","command":"somebody-else"}`, createOnly("add github again"))
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("create of a taken name = %d, want 412: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "entity_exists") {
		t.Errorf("the refusal does not say the name is taken: %s", res.Body.String())
	}
	if after := s.activeDocument(t); after != before {
		t.Error("a refused create changed the stored document")
	}
}

// A CREATE CANNOT RENAME EITHER: the path is the address, and a body naming
// another server would land under a name the caller did not ask for.
func TestACreateOnlyWriteKeepsThePathAsTheName(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)

	res := s.do(t, http.MethodPut, "/config/mcp-servers/linear",
		`{"name":"jira","command":"jira-mcp"}`, createOnly("add linear"))
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "identity_mismatch") {
		t.Fatalf("create under another name = %d, want 400 identity_mismatch: %s",
			res.Code, res.Body.String())
	}
}

// EVERY COLLECTION THIS SURFACE ADDRESSES, IT CREATES BY ADDRESS. A collection
// whose members have a place the path cannot name is the chart's, which is not
// addressed here at all; one added to the table without saying how a member of
// it is added would reach a create with nothing to call. Mutation: drop one
// collection's create and this fails.
func TestEveryCollectionIsCreatable(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)
	bodies := map[string]string{
		configapi.EntityLLMProviders: `{"type":"anthropic","model":"m","api_keys":["k"]}`,
		configapi.EntityMCPServers:   `{"name":"fresh","command":"fresh-mcp"}`,
	}
	for _, kind := range configapi.EntityKinds() {
		body, known := bodies[kind]
		if !known {
			t.Errorf("%s is addressed here and this case has no member to add to it: "+
				"say how one is created, and add it here", kind)
			continue
		}
		res := s.do(t, http.MethodPut, "/config/"+kind+"/fresh?dry_run=true", body,
			map[string]string{"If-None-Match": "*"})
		if res.Code != http.StatusOK {
			t.Errorf("a create-only check of %s = %d, want 200: %s", kind, res.Code, res.Body.String())
		}
	}
}

// ONE PRECONDITION PER WRITE. `If-Match` names the DOCUMENT's revision an edit
// was read from, and an address with nothing at it has no representation for
// that tag to describe — so the two together are refused rather than one of
// them silently ignored.
func TestACreateOnlyWriteRefusesAnIfMatchBesideIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)
	tag := s.do(t, http.MethodGet, "/config", "", nil).Header().Get("ETag")

	headers := createOnly("add linear")
	headers["If-Match"] = tag
	res := s.do(t, http.MethodPut, "/config/mcp-servers/linear",
		`{"name":"linear","command":"linear-mcp"}`, headers)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "conflicting_preconditions") {
		t.Fatalf("create with If-Match = %d, want 400 conflicting_preconditions: %s",
			res.Code, res.Body.String())
	}
}

// A CREATE IS CHECKED LIKE ANY WRITE: a dry run validates the whole company
// with the server in it, and stores nothing — and an invalid server (an http
// transport with no address) is refused before anything is stored.
func TestACreateOnlyDryRunStoresNothingAndValidatesTheServer(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)
	before := s.activeDocument(t)

	checked := s.do(t, http.MethodPut, "/config/mcp-servers/linear?dry_run=true",
		`{"name":"linear","command":"linear-mcp"}`, map[string]string{"If-None-Match": "*"})
	if checked.Code != http.StatusOK {
		t.Fatalf("create dry run = %d, want 200: %s", checked.Code, checked.Body.String())
	}
	invalid := s.do(t, http.MethodPut, "/config/mcp-servers/linear?dry_run=true",
		`{"name":"linear","transport":"http"}`, map[string]string{"If-None-Match": "*"})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("dry run of an http server with no url = %d, want 400: %s",
			invalid.Code, invalid.Body.String())
	}
	if after := s.activeDocument(t); after != before {
		t.Error("a dry run changed the stored document")
	}
}

// A PROVIDER IS CREATED BY ITS KEY, the other flat collection: nothing but the
// key identifies it, so the path is the whole of its address.
func TestACreateOnlyWriteAddsAProviderUnderItsKey(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, serversDoc)

	res := s.do(t, http.MethodPut, "/config/llm-providers/yankee",
		`{"type":"anthropic","model":"claude-sonnet-5","api_keys":["${YANKEE_KEY}"]}`,
		createOnly("add a second provider"))
	if res.Code != http.StatusCreated {
		t.Fatalf("create-only provider PUT = %d, want 201: %s", res.Code, res.Body.String())
	}
	ids, err := s.service().Entities(t.Context(), configapi.EntityLLMProviders)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"yankee", "zulu"}) {
		t.Errorf("providers after the add = %v", ids)
	}
}
