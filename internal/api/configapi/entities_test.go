package configapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// entityBody reads {kind, id, entity} out of a query-shaped answer.
func entityOf(t *testing.T, s *surface, kind, id string) map[string]any {
	t.Helper()
	entity, err := s.service().Entity(t.Context(), kind, id)
	if err != nil {
		t.Fatalf("Entity(%s, %s): %v", kind, id, err)
	}
	raw, err := json.Marshal(entity)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// EVERY ADDRESSABLE COLLECTION IS LISTABLE, and a seat inside a unit is a
// seat: an operator editing "the CTO" does not think about which list it
// happens to live in, and a surface that only saw root-level roles would
// make every real org chart's seats unreachable.
func TestEveryEntityCollectionListsWhatTheDocumentCarries(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	for _, tc := range []struct {
		kind string
		want []string
	}{
		{configapi.EntityRoles, []string{"ceo", "cto"}},
		{configapi.EntityLLMProviders, []string{"zulu"}},
	} {
		got, err := s.service().Entities(t.Context(), tc.kind)
		if err != nil {
			t.Fatalf("Entities(%s): %v", tc.kind, err)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Entities(%s) = %v, want %v", tc.kind, got, tc.want)
		}
	}

	// An EMPTY collection is a real answer and must not come back as null:
	// a company with no units is a company, and null renders as a failure.
	units, err := s.service().Entities(t.Context(), configapi.EntityUnits)
	if err != nil {
		t.Fatalf("Entities(units): %v", err)
	}
	if units == nil {
		t.Error("a company with no units answered null rather than an empty list")
	}
}

// A KIND NOBODY ADDRESSES IS NAMED, not answered empty. An empty list for a
// typo reads as "there are none of those", which sends an operator looking
// for the entity they just deleted.
func TestAnUnknownEntityKindSaysWhatTheKindsAre(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	_, err := s.service().Entities(t.Context(), "widgets")
	if err == nil {
		t.Fatal("an unknown kind was answered")
	}
	for _, kind := range configapi.EntityKinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("the refusal does not name %s: %v", kind, err)
		}
	}
}

// AN ENTITY READ IS REDACTED, because it is a slice of a document that is.
// Otherwise a per-entity read is a way to fetch every credential in the
// company one seat at a time, past the masking the document read applies.
func TestAnEntityReadCarriesNoCredential(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	provider := entityOf(t, s, configapi.EntityLLMProviders, "zulu")
	raw, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-literal") {
		t.Fatalf("a literal credential came back in an entity read: %s", raw)
	}
	// And a ${VAR} is NOT a credential — it names one, and it is the half
	// an operator edits, so masking it would make the editor useless.
	if !strings.Contains(string(raw), "${") {
		t.Errorf("the reference was masked away, so an editor cannot see what "+
			"it points at: %s", raw)
	}
}

// A WRITE CHANGES ONE ENTITY AND NOTHING ELSE, which is the whole reason
// this surface exists beside PUT /config: an operator editing one seat's goal
// must not send back — and take responsibility for — every other seat.
func TestAnEntityWriteLeavesTheRestOfTheDocumentAlone(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	role := entityOf(t, s, configapi.EntityRoles, "ceo")
	role["goal"] = "ship the rewrite"
	body, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/roles/ceo", string(body),
		map[string]string{"X-Summary": "give the CEO a goal"})
	// 201, like PUT /config: the write creates a REVISION, which is a new
	// resource whether it changed one entity or the whole document.
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT /config/roles/ceo = %d: %s", res.Code, res.Body.String())
	}

	if got := entityOf(t, s, configapi.EntityRoles, "ceo")["goal"]; got != "ship the rewrite" {
		t.Errorf("the edit did not land: goal = %v", got)
	}
	// The seat nobody touched is still there and unchanged, which a
	// whole-document write could have lost.
	ids, err := s.service().Entities(t.Context(), configapi.EntityRoles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "ceo,cto" {
		t.Errorf("the other seats did not survive the write: %v", ids)
	}
}

// A MASK COMES BACK AS THE VALUE IT HID. The read is redacted, so a caller
// who fetched an entity, changed one line and sent it back would otherwise
// replace that entity's credentials with the literal mask — silently, and
// discovered when the integration stops authenticating.
func TestAnEntityWriteRestoresWhatTheReadMasked(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	provider := entityOf(t, s, configapi.EntityLLMProviders, "zulu")
	provider["model"] = "claude-haiku-4-5"
	body, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/llm-providers/zulu", string(body),
		map[string]string{"X-Summary": "move zulu to haiku"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT /config/llm-providers/zulu = %d: %s", res.Code, res.Body.String())
	}

	// The document is what proves it: read it back unredacted through the
	// store the way a node applying the revision does.
	stored := s.activeDocument(t)
	if strings.Contains(stored, "__redacted__") {
		t.Fatalf("a mask was stored as a credential:\n%s", stored)
	}
	if !strings.Contains(stored, "sk-literal") {
		t.Fatalf("the untouched credential was lost by the write:\n%s", stored)
	}
	if !strings.Contains(stored, "claude-haiku-4-5") {
		t.Fatalf("the edit did not reach the stored document:\n%s", stored)
	}
}

// THE WHOLE DOCUMENT IS VALIDATED, not just the entity. A seat naming a
// provider that no longer exists is valid on its own terms and breaks the
// company — and a per-entity surface is exactly where that gets introduced,
// because the caller never sees the rest of the document.
func TestAnEntityWriteThatBreaksTheCompanyIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	role := entityOf(t, s, configapi.EntityRoles, "ceo")
	role["llm"] = "a-provider-that-does-not-exist"
	body, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/roles/ceo", string(body),
		map[string]string{"X-Summary": "point the CEO at nothing"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a dangling provider = %d, want 400: %s",
			res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "validation_error") {
		t.Errorf("the refusal does not say it failed validation: %s", res.Body.String())
	}
}

// AN ID NOBODY CARRIES IS NOT CREATED. A PUT naming an entity that is not
// there is far more often a typo than an intent to add one, and creating
// through this route would let a caller grow the company without ever seeing
// the document they changed.
func TestAnEntityWriteNeverCreates(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodPut, "/config/roles/nobody",
		`{"name":"Nobody","handle":"nobody"}`,
		map[string]string{"X-Summary": "add a seat sideways"})
	if res.Code != http.StatusNotFound {
		t.Fatalf("PUT to an unknown id = %d, want 404: %s", res.Code, res.Body.String())
	}
	ids, err := s.service().Entities(t.Context(), configapi.EntityRoles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(ids, ","), "nobody") {
		t.Errorf("the refused write created a seat anyway: %v", ids)
	}
}

// AND IT STILL NEEDS AN AUDIT SUMMARY. The history is what an operator reads
// to find the change that broke something, and a per-entity write is the one
// most likely to be made in a hurry.
func TestAnEntityWriteNeedsASummary(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodPut, "/config/roles/ceo", `{"name":"CEO"}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PUT with no summary = %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "summary_required") {
		t.Errorf("the refusal does not name what is missing: %s", res.Body.String())
	}
}
