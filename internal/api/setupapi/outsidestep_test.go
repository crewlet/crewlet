package setupapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// The values an outside step's case saves and pastes. Obviously fake.
const (
	gcpIdentity   = "acme-crewlet-id"
	gcpProvider   = "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine"
	awsConnection = "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/0f0e0d0c-0b0a-4908-8706-050403020100," +
		"arn:aws:rolesanywhere:us-east-2:123456789012:profile/00010203-0405-4607-8809-0a0b0c0d0e0f," +
		"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-provisioner," +
		"arn:aws:rolesanywhere:us-east-2:123456789012:profile/10010203-0405-4607-8809-0a0b0c0d0e0f," +
		"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-broker"
)

// outsideStep is one integration whose last field a block prints: the save that
// mints the block's key, and the field the block prints.
type outsideStep struct {
	kind    integration.Kind
	save    string
	printed string
	pasted  string
	minted  string
}

var outsideSteps = []outsideStep{
	{integration.KindGoogleCloud, `{"values": {"enabled": "true", "identity_project": "` + gcpIdentity + `"},
		"generate": ["federation_key"]}`, "provider", gcpProvider, "federation_key"},
	{integration.KindAWS, `{"values": {"enabled": "true", "slug": "acme", "home_region": "us-east-2"},
		"generate": ["certificate_authority"]}`, "connection", awsConnection, "certificate_authority"},
}

// halfSetUp is a surface whose kind has its first step saved and its key
// minted, the printed field still empty.
func halfSetUp(t *testing.T, step outsideStep) *surface {
	t.Helper()
	s := newSurface(t)
	s.seed(t)
	s.connect(t, string(step.kind), step.save)
	return s
}

// A FIELD A BLOCK PRINTS IS TESTED THROUGH THE ROUTE, as a key is, though it is
// no credential: the route hands the integration's probe the pasted value and
// the minted key from the store. Mutation: refuse a field that is not a secret
// in the route again, and both kinds answer 400 without asking the probe.
func TestAFieldABlockPrintsIsCheckedThroughTheRoute(t *testing.T) {
	t.Parallel()
	for _, step := range outsideSteps {
		t.Run(string(step.kind), func(t *testing.T) {
			t.Parallel()
			s := halfSetUp(t, step)
			probe := &recordingProbe{verdict: setup.Works("Works."), read: []string{step.minted}}
			s.withProbe(t, step.kind, probe.probe)
			res := s.do(t, http.MethodPost, "/setup/integrations/"+string(step.kind)+"/verify",
				verifyBody(step.printed, step.pasted, ""), nil)
			if res.Code != http.StatusOK || decode(t, res)["outcome"] != string(setup.VerifyWorks) {
				t.Fatalf("verify = %d %s, want the probe's verdict", res.Code, res.Body)
			}
			seen := probe.seen(t)
			if seen.field != step.printed || seen.values[step.printed] != step.pasted ||
				!strings.Contains(seen.values[step.minted], "-----BEGIN") {
				t.Errorf("the probe was asked about %q with %d values, want %s pasted and the key held",
					seen.field, len(seen.values), step.printed)
			}
		})
	}
}

// A HALF SET-UP FORM SAYS THE PRINTED FIELD IS NEEDED TO FINISH, PRINTED BY THE
// BLOCK'S ROW, and the block names the saved answers it is drawn from. The
// requirement list is what the form decides a save by. Mutation: require the
// printed field only once the key is held, or drop printed_by, and this fails.
func TestAHalfSetUpFormDeclaresWhatTheBlockPrints(t *testing.T) {
	t.Parallel()
	for _, step := range outsideSteps {
		t.Run(string(step.kind), func(t *testing.T) {
			t.Parallel()
			for _, s := range []*surface{newSurfaceSeeded(t), halfSetUp(t, step)} {
				res := s.do(t, http.MethodGet, "/setup/integrations/"+string(step.kind), "", nil)
				var state setupapi.ToolState
				if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
					t.Fatal(err)
				}
				byField := map[string]setup.Requirement{}
				for _, r := range state.Requirements {
					byField[r.Field] = r
				}
				printed, minted := byField[step.printed], byField[step.minted]
				if !printed.Required || printed.PrintedBy != step.minted || printed.Present || state.Satisfied {
					t.Errorf("the printed field is %+v (satisfied %v), want required, printed by %s and unfinished",
						printed, state.Satisfied, step.minted)
				}
				if minted.Present && (minted.Public == nil || len(minted.Public.DrawnFrom) == 0) {
					t.Errorf("the minted key's block is %+v, want it to name what it is drawn from", minted.Public)
				}
			}
		})
	}
}

// newSurfaceSeeded is a surface over the plain company, nothing connected.
func newSurfaceSeeded(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.seed(t)
	return s
}

// THE ROUTE RUNS GOOGLE CLOUD'S OWN PROBE ON THE PROVIDER and answers which
// part failed and what to do: here Google's Security Token Service refuses the
// federation token. Mutation: answer the route's old refusal, and there is no
// verdict to read.
func TestTheGoogleCloudTestNamesThePartThatFailed(t *testing.T) {
	t.Parallel()
	s := halfSetUp(t, outsideSteps[0])
	google := httpxtest.InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "invalid_grant", "error_description": "The audience in ID Token does not match."}`))
	}))
	s.withProbe(t, integration.KindGoogleCloud, googlecloud.Probe(google))
	res := s.do(t, http.MethodPost, "/setup/integrations/google_cloud/verify",
		verifyBody("provider", gcpProvider, ""), nil)
	var got setup.Verdict
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || res.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", res.Code, res.Body)
	}
	if got.Outcome != setup.VerifyRefused || !strings.Contains(got.Summary, "Security Token Service") ||
		!slices.Contains(got.Facts, setup.Fact{Label: "Federation", Value: setup.PartFailed}) ||
		len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0].Action, "Cloud Shell block") {
		t.Errorf("answered %+v, want federation named as the part that failed, with its fix", got)
	}
}

// THE ROUTE RUNS AWS'S OWN PROBE ON THE STACK OUTPUT, and names the sign-in as
// the part AWS refused. Mutation: as for Google Cloud.
func TestTheAWSTestNamesThePartThatFailed(t *testing.T) {
	t.Parallel()
	s := halfSetUp(t, outsideSteps[1])
	amazon := httpxtest.InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Untrusted signing certificate."}`))
	}))
	s.withProbe(t, integration.KindAWS, aws.Probe(amazon))
	res := s.do(t, http.MethodPost, "/setup/integrations/aws/verify", verifyBody("connection", awsConnection, ""), nil)
	var got setup.Verdict
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || res.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", res.Code, res.Body)
	}
	if got.Outcome != setup.VerifyRefused || !strings.Contains(got.Summary, "refused Crewlet's sign-in") ||
		!slices.Contains(got.Facts, setup.Fact{Label: "Sign-in", Value: setup.PartFailed}) || len(got.Warnings) == 0 {
		t.Errorf("answered %+v, want the sign-in named as the part that failed, with its fix", got)
	}
}

// EVERY FIELD A BLOCK PRINTS NAMES A MINTED FIELD OF ITS OWN LIST, AND IS
// CHECKABLE, across every integration, so the form's save rule and its Test
// hold for any kind that declares one. Mutation: point printed_by at a field
// that is not minted, and this fails.
func TestEveryPrintedFieldNamesTheBlockThatPrintsIt(t *testing.T) {
	t.Parallel()
	s := newSurfaceSeeded(t)
	seen := 0
	for _, kind := range integration.Kinds {
		res := s.do(t, http.MethodGet, "/setup/integrations/"+string(kind), "", nil)
		var state setupapi.ToolState
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, r := range state.Requirements {
			if r.PrintedBy == "" {
				continue
			}
			seen++
			i := slices.IndexFunc(state.Requirements, func(o setup.Requirement) bool { return o.Field == r.PrintedBy })
			if i < 0 || !state.Requirements[i].Mintable || !r.Required || !r.Checkable || r.Kind == setup.KindSecret {
				t.Errorf("%s.%s is printed by %q, which is not a minted field of its list, or is not required "+
					"and checkable", kind, r.Field, r.PrintedBy)
			}
		}
	}
	if seen < len(outsideSteps) {
		t.Errorf("%d printed fields walked, want at least %d", seen, len(outsideSteps))
	}
}
