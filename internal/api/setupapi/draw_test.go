package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/setup"
)

// googleCloudKey is the federation key's row of the Google Cloud listing.
func (s *surface) googleCloudKey(t *testing.T) (setupapi.ToolState, setup.Requirement) {
	t.Helper()
	res := s.do(t, http.MethodGet, "/setup/integrations/google_cloud", "", nil)
	var state setupapi.ToolState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	for _, r := range state.Requirements {
		if r.Field == "federation_key" {
			return state, r
		}
	}
	t.Fatal("the Google Cloud listing has no federation key")
	return setupapi.ToolState{}, setup.Requirement{}
}

// drawn is the draw route's answer to body.
type drawn struct {
	Public *setup.PublicView `json:"public"`
	Needs  string            `json:"needs"`
	Detail string            `json:"detail"`
	Field  string            `json:"field"`
}

func (s *surface) draw(t *testing.T, body string) (int, drawn) {
	t.Helper()
	res := s.do(t, http.MethodPost, "/setup/integrations/google_cloud/draw", body, nil)
	var out drawn
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("draw answered %d: %s", res.Code, res.Body)
	}
	return res.Code, out
}

// GOOGLE CLOUD'S BLOCK IS THERE BEFORE ANY SAVE, AND FOLLOWS WHAT IS TYPED:
// the form's first write makes the key alone, which starts no connection, and
// the block is redrawn through the route from the typed project, a value that
// is no project ID refused at its field and never drawn.
//
// Mutation: let the route draw from a typed key, or draw an unchecked
// project, and a case fails.
func TestGoogleCloudsBlockIsShownBeforeAnySaveAndFollowsWhatIsTyped(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	if _, key := s.googleCloudKey(t); !key.MintFirst || key.Present || key.Public != nil {
		t.Fatalf("before the form opens, the key is %+v, want one made first and not held", key)
	}
	s.connect(t, "google_cloud", `{"generate": ["federation_key"]}`)
	state, key := s.googleCloudKey(t)
	if state.Configured || !key.Present || key.Public == nil || !key.Public.Live {
		t.Fatalf("after the key alone: configured %v, key %+v; want a live block and nothing started",
			state.Configured, key)
	}
	code, got := s.draw(t, `{"field": "federation_key", "values": {"identity_project": "acme-ids-7", "atlas_read": "true"}}`)
	if code != http.StatusOK || got.Public == nil || !strings.Contains(got.Public.Text, "IDENTITY=acme-ids-7\n") ||
		!strings.Contains(got.Public.Text, "crewlet-atlas") {
		t.Fatalf("drawn %d %+v", code, got)
	}
	for _, tc := range []struct {
		body  string
		code  int
		field string
	}{
		{`{"field": "federation_key", "values": {"identity_project": "acme; rm -rf ~"}}`, http.StatusBadRequest,
			"identity_project"},
		{`{"field": "federation_key", "values": {"federation_key": "a key of mine"}}`, http.StatusBadRequest, ""},
		{`{"field": "provider", "values": {}}`, http.StatusBadRequest, ""},
	} {
		code, got := s.draw(t, tc.body)
		if code != tc.code || got.Public != nil || got.Field != tc.field {
			t.Errorf("%s: %d %+v, want %d refusing %q", tc.body, code, got, tc.code, tc.field)
		}
	}
}
