package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
)

// A SAVED AWS FORM SHOWS ITS CLOUDSHELL BLOCK, through the route and the real
// sealed store: Save mints the certificate authority, and the listing renders
// the block from the bundle its `${NAME}` names. Handed the whole reference,
// the resolver found nothing and the block said the stack could not be
// rendered on every company connected from the dashboard.
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
