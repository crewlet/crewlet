package configapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// movableDoc holds two MCP servers, each with a literal credential of its
// own, so one can be MOVED in the list and the match tested.
const movableDoc = companyDoc + `
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false, env: {TOKEN: tracker-literal}}
  - {name: notion, command: notion-mcp, env: {TOKEN: notion-literal}}
`

// A LIST MEMBER MOVED THROUGH THE WRITE SURFACE KEEPS ITS CREDENTIAL.
//
// The whole loop a builder runs: read the redacted document, reorder a list,
// send it back. A restore that matched by POSITION would hand one server's
// credential to the other — which is not a refusal but a silent swap, so every
// assertion here is on the STORED bytes rather than on the status.
//
// This used to be a seat moved from the root into a unit, which is the same
// match one shape up; a seat is the org chart's own now, and
// [config.Company.RestoreRedacted]'s own suite is where the document-wide
// version of this rule is held.
func TestAMovedListMemberKeepsItsCredentialThroughAPut(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, movableDoc, nil)

	read := s.do(t, http.MethodGet, "/config", "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
	}
	var document map[string]any
	if err := json.Unmarshal(read.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	servers, _ := document["mcp_servers"].([]any)
	servers[0], servers[1] = servers[1], servers[0]
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "__redacted__") {
		t.Fatalf("the moved server carries no mask, so this proves nothing: %s", body)
	}

	res := s.do(t, http.MethodPut, "/config", string(body),
		map[string]string{"X-Summary": "reorder the servers"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	stored := storedTree(t, s)
	if strings.Contains(s.activeDocument(t), "__redacted__") {
		t.Fatalf("a mask was stored as a credential: %s", s.activeDocument(t))
	}
	// EACH VALUE BACK ON ITS OWN MEMBER. A positional restore stores two
	// real credentials and swaps them, which nothing else here would see.
	for name, want := range map[string]string{
		"tracker": "tracker-literal", "notion": "notion-literal",
	} {
		server := serverByName(stored, name)
		env, _ := server["env"].(map[string]any)
		if env["TOKEN"] != want {
			t.Errorf("%s holds %v, want %q — the restore matched by position",
				name, env["TOKEN"], want)
		}
	}
}

// A DOCUMENT READ AND SENT BACK UNCHANGED KEEPS AN EXPLICIT `false` FALSE.
//
// The read once dropped every toggle's state, so a server kept in config with
// `shared: false` was served without the field and the unchanged document sent
// back stored a server that is shared — one child for the whole company where
// the operator asked for one per seat, which hands one seat's credentials to
// another. The assertion is on the read as well as the stored result, because
// the builder edits exactly what the read serves.
func TestAReadDocumentSentBackKeepsAnExplicitFalse(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, movableDoc, nil)

	read := s.do(t, http.MethodGet, "/config", "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
	}
	if !strings.Contains(read.Body.String(), `"shared":false`) {
		t.Fatalf("the read dropped the toggle's explicit false: %s", read.Body)
	}

	res := s.do(t, http.MethodPut, "/config", read.Body.String(),
		map[string]string{"X-Summary": "send it back"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	if stored := s.activeDocument(t); !strings.Contains(stored, `"shared":false`) {
		t.Errorf("sending the read back shared the server: %s", stored)
	}
}
