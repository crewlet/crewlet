package setupapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/setup"
)

// A SAVED AWS FORM SHOWS ITS CLOUDSHELL BLOCK, through the route and the real
// sealed store: the block is rendered from the bundle its `${NAME}` names,
// not from the reference itself, which resolves to nothing.
func TestASavedAWSFormShowsItsCloudShellBlock(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.connect(t, "aws", `{
		"values": {"enabled": "true", "slug": "acme", "home_region": "eu-west-1"},
		"generate": ["certificate_authority"]
	}`)
	res := s.do(t, http.MethodGet, "/setup/integrations/aws", "", nil)
	var state setupapi.ToolState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	for _, r := range state.Requirements {
		if r.Field != "certificate_authority" {
			continue
		}
		if r.Public == nil || !strings.Contains(r.Public.Text, "BEGIN CERTIFICATE") ||
			!strings.Contains(r.Public.Text, "--stack-name crewlet-acme") {
			t.Fatalf("the saved form's block is %+v", r.Public)
		}
		return
	}
	t.Fatal("the AWS listing has no certificate authority")
}

// A SAVED IDENTITY PROJECT PUTS GOOGLE CLOUD'S SETUP AT ITS CLOUD SHELL STEP:
// the steps before it are done and the block to run is on the key's row, so a
// reader of the answer knows the step without reading anything else.
func TestASavedIdentityProjectPutsGoogleCloudAtItsCloudShellStep(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.connect(t, "google_cloud", `{
		"values": {"enabled": "true", "identity_project": "acme-crewlet-id"},
		"generate": ["federation_key"]
	}`)
	res := s.do(t, http.MethodGet, "/setup/integrations/google_cloud", "", nil)
	var state setupapi.ToolState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	steps := state.Guide.Prepare
	at := slices.IndexFunc(steps, func(step setup.Step) bool { return !step.Done })
	if at < 1 || steps[at].Title != "Run the block in Cloud Shell" {
		var done []bool
		for _, step := range steps {
			done = append(done, step.Done)
		}
		t.Errorf("the steps read done %v, want every step before the Cloud Shell block's", done)
	}
	for _, r := range state.Requirements {
		if r.Field == "federation_key" && (r.Public == nil || r.Public.Text == "") {
			t.Errorf("the key's row carries no block to run: %+v", r.Public)
		}
	}
}
