package setupapi_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// A DISCONNECT NAMES WHAT IT LEAVES BEHIND, ON THE PATH THE DASHBOARD USES.
//
// The store is the company's, so a credential an operator may be sharing with
// another deployment is not something a Disconnect button removes. What the
// answer owes them is the list — and the list existed only on the FORCE path,
// which nothing presses: the ordinary disconnect returned
// `{key, removed, disconnecting, remove_seats, detail}` and named nothing at
// all. Measured: seven credentials survived a disconnect and not one of them
// was named.
//
// It has to be computed HERE, before the intent is recorded, because every
// name in it is derived from a `${VAR}` in the company document and both paths
// end with that block gone.
func TestAnOrdinaryDisconnectNamesTheCredentialsItLeaves(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	// WITH A STATUS STORE, because the ordinary path records the intent on
	// the fleet row and refuses without one.
	s.withPass(t, &recordingPass{kind: integration.KindAtlassian})
	out := s.do(t, http.MethodDelete, "/setup/integrations/atlassian",
		`{"remove_seats": true}`, nil)
	if out.Code != http.StatusAccepted {
		t.Fatalf("disconnect = %d: %s", out.Code, out.Body)
	}
	body := decode(t, out)
	orphans, _ := body["orphaned_secrets"].([]any)

	// THE ORGANIZATION'S OWN KEY, which the old list would have named.
	if !slices.Contains(orphans, any("ORG_KEY")) {
		t.Errorf("orphaned_secrets = %v, want the organization key named", orphans)
	}
	// AND THE SEAT'S, which it never did. A pass mints a token under the
	// name that seat's own mcp_env points at, and Atlassian seals the
	// ADDRESS beside it because its products authenticate
	// base64(address:token) — so a disconnect orphans two values per agent
	// and named neither.
	for _, want := range []string{"SRE_ATLASSIAN", "SRE_EMAIL"} {
		if !slices.Contains(orphans, any(want)) {
			t.Errorf("orphaned_secrets = %v, want the seat's %s named", orphans, want)
		}
	}
}

// AND IT NAMES ONLY WHAT IS THERE.
//
// A list to act on has to be a list of things that exist: telling an operator
// to unset a variable nobody ever set is a step they cannot take, over an
// optional credential this company chose not to use.
func TestADisconnectNamesNoCredentialNobodySet(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	s.withPass(t, &recordingPass{kind: integration.KindGitLab})
	out := s.do(t, http.MethodDelete, "/setup/integrations/gitlab", `{}`, nil)
	if out.Code != http.StatusAccepted {
		t.Fatalf("disconnect = %d: %s", out.Code, out.Body)
	}
	orphans, _ := decode(t, out)["orphaned_secrets"].([]any)

	// The routing token is optional and this company never set one.
	if slices.Contains(orphans, any("GITLAB_ROUTING_TOKEN")) {
		t.Errorf("orphaned_secrets = %v, naming a credential nobody stored", orphans)
	}
	// What it DID set is still named.
	if !slices.Contains(orphans, any("GL_SIGN")) {
		t.Errorf("orphaned_secrets = %v, want the signing secret named", orphans)
	}
}
