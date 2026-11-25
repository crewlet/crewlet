package setupapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/googlecloud"
)

// stepsGolden is each outside-step integration's setup, as GET
// /setup/integrations/{kind} answers it at every point of its steps, which
// the dashboard's Setup steps suite renders the form from.
const stepsGolden = "testdata/setup_steps.json"

// stepsState is one point of an integration's steps: the saves before it, and
// the setup the route then answers, as sent (a field's value is written only
// on the wire, so the answer is never read back through ToolState).
type stepsState struct {
	Name string         `json:"name"`
	Tool map[string]any `json:"tool"`
}

// stepsTool is one integration's setup at each point of its steps, in order.
type stepsTool struct {
	Kind   string       `json:"kind"`
	States []stepsState `json:"states"`
}

// awsOutput is the stack output of the company acme in us-east-2.
const awsOutput = "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/0f0e0d0c-0b0a-4908-8706-050403020100," +
	"arn:aws:rolesanywhere:us-east-2:123456789012:profile/00010203-0405-4607-8809-0a0b0c0d0e0f," +
	"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-provisioner," +
	"arn:aws:rolesanywhere:us-east-2:123456789012:profile/10010203-0405-4607-8809-0a0b0c0d0e0f," +
	"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-broker"

// renderSteps walks each outside-step integration's steps through the route:
// nothing saved, the first save that mints the key the block carries, and
// the block's output pasted.
func renderSteps(t *testing.T) []byte {
	t.Helper()
	walks := []struct {
		kind  string
		saves []struct{ name, body string }
	}{
		{"google_cloud", []struct{ name, body string }{
			{"the identity project saved", `{"values": {"enabled": "true", "identity_project": "acme-crewlet-id"},
				"generate": ["federation_key"]}`},
			{"the provider saved", `{"values": {"provider": "` + googlecloud.ProviderName("123456789012") + `"}}`},
		}},
		{"aws", []struct{ name, body string }{
			{"the short name and Region saved", `{"values": {"enabled": "true", "slug": "acme",
				"home_region": "us-east-2"}, "generate": ["certificate_authority"]}`},
			{"the stack output saved", `{"values": {"connection": "` + awsOutput + `"}}`},
		}},
	}
	var out []stepsTool
	for _, walk := range walks {
		s := newSurface(t)
		s.seed(t)
		tool := stepsTool{Kind: walk.kind, States: []stepsState{{Name: "nothing saved", Tool: readTool(t, s, walk.kind)}}}
		for _, save := range walk.saves {
			s.connect(t, walk.kind, save.body)
			tool.States = append(tool.States, stepsState{Name: save.name, Tool: readTool(t, s, walk.kind)})
		}
		out = append(out, tool)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// readTool is the route's answer for kind, the block's text (a fresh key
// each run) set to one line so the file is stable.
func readTool(t *testing.T, s *surface, kind string) map[string]any {
	t.Helper()
	res := s.do(t, http.MethodGet, "/setup/integrations/"+kind, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", kind, res.Code, res.Body)
	}
	var state map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	reqs, _ := state["requirements"].([]any)
	for _, r := range reqs {
		if p, ok := r.(map[string]any)["public"].(map[string]any); ok {
			p["text"] = "# the block, rendered for the saved key\n"
		}
	}
	return state
}

// THE STEPS THE DASHBOARD CHECKS ARE THE ENGINE'S: testdata/setup_steps.json
// is the route's answer at each point of the outside-step integrations'
// steps, and the dashboard's suite (SetupSteps.guide.test.tsx) renders the
// form from it and finds every field and control a step taken in Crewlet
// names. `make setup-steps` rewrites it.
func TestTheSetupStepsMatchTheirGoldenFile(t *testing.T) {
	t.Parallel()
	got := renderSteps(t)
	if os.Getenv("CREWLET_REGENERATE_SETUP_STEPS") == "1" {
		if err := os.MkdirAll(filepath.Dir(stepsGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stepsGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(stepsGolden)
	if err != nil {
		t.Fatalf("read %s: %v. `make setup-steps` writes it", stepsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches what the route answers. Run `make setup-steps`, read the diff, and "+
			"check the dashboard's Setup steps suite still passes on it.\n--- rendered now ---\n%s", stepsGolden, got)
	}
}
