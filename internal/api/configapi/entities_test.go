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

// PUT IS THE ONLY VERB ON AN ENTITY PATH, and the refusal has to be legible.
//
// The previous engine served DELETE here, so an operator carrying those
// scripts forward will send one. They get 405 with an Allow header naming PUT
// — a *routing* answer rather than a handler that 404s or, worse, one that
// quietly succeeds having done nothing.
//
// Removal is a full-document edit on purpose, for the same reason creation is
// and more so: deleting a seat strands its mailbox and its in-flight work, and
// deleting a provider silently repoints every role that named it. Both belong
// in a document somebody looked at. docs/guides/configure-via-api.md states
// this status code, which is why it is asserted rather than assumed.
func TestAnEntityPathRefusesEveryVerbButPut(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	for _, kind := range configapi.EntityKinds() {
		// GET is in the list because an operator has every reason to try
		// it: the entity paths look like resources, and reading one goes
		// through /query/config_entities instead. A 405 naming PUT is the
		// answer that sends them there.
		for _, method := range []string{
			http.MethodDelete, http.MethodPost, http.MethodPatch, http.MethodGet,
		} {
			res := s.do(t, method, "/config/"+kind+"/ceo", "", nil)
			if res.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /config/%s/ceo = %d, want 405 — an entity path serves "+
					"PUT alone, and any other answer leaves an operator guessing "+
					"whether the write happened", method, kind, res.Code)
				continue
			}
			if allow := res.Header().Get("Allow"); !strings.Contains(allow, http.MethodPut) {
				t.Errorf("%s /config/%s/ceo: Allow = %q, which does not name PUT — "+
					"the one verb this path has", method, kind, allow)
			}
		}
	}
}

// nestedDoc is companyDoc with the shapes the flat fixture cannot exercise:
// seats inside units, a unit inside a unit, and an MCP server. The entity
// surface's whole claim is that a seat is reachable by handle "wherever it
// lives", and a fixture with no units leaves that claim untested.
const nestedDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal"]
mcp_servers:
  - name: tracker
    transport: http
    url: https://mcp.example.com
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: engineering
    lead: cto
    roles:
      - name: CTO
        handle: cto
        llm: zulu
    children:
      - name: platform
        roles:
          - name: Staff Engineer
            handle: staff-eng
            llm: zulu
`

// A SEAT INSIDE A UNIT IS A SEAT, at any depth. An operator editing "the
// staff engineer" does not think about which list it happens to live in, and
// a surface that only reached root-level roles would make every real org
// chart's seats unaddressable.
func TestAnEntityWriteReachesASeatNestedInAUnit(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, nestedDoc, nil)

	ids, err := s.service().Entities(t.Context(), configapi.EntityRoles)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "ceo,cto,staff-eng" {
		t.Fatalf("roles = %v, want the root seat and both nested ones", ids)
	}

	role := entityOf(t, s, configapi.EntityRoles, "staff-eng")
	role["goal"] = "own the build"
	body, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/roles/staff-eng", string(body),
		map[string]string{"X-Summary": "give the staff engineer a goal"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT a twice-nested seat = %d: %s", res.Code, res.Body.String())
	}
	if got := entityOf(t, s, configapi.EntityRoles, "staff-eng")["goal"]; got != "own the build" {
		t.Errorf("the edit did not reach the nested seat: goal = %v", got)
	}
}

// A WRITE NEVER RENAMES. The path is the address, and a body carrying a
// different identity is a rename wearing a replacement's clothes.
//
// Refused rather than applied, because none of what points at the old
// identity moves with the splice: a seat's durable id is a UUIDv5 over
// (company name, handle), so a silent rename strands its diary, its
// onboarding marker and its counterparty profiles behind an id nothing
// derives any more — and leaves the URL naming a seat that is gone.
func TestAnEntityWriteNeverRenames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, id, body string
	}{
		{
			name: "a seat", kind: configapi.EntityRoles, id: "ceo",
			body: `{"name":"CEO","handle":"chief","llm":"zulu"}`,
		},
		{
			// THE ONE THAT ARRIVES BY ACCIDENT: no handle in the body at
			// all, just a new display name — which derives a new handle,
			// and renames the seat without the word ever being used.
			name: "a seat renamed by its display name alone",
			kind: configapi.EntityRoles, id: "ceo",
			body: `{"name":"Chief Executive","llm":"zulu"}`,
		},
		{
			name: "a unit", kind: configapi.EntityUnits, id: "engineering",
			body: `{"name":"eng","lead":"cto"}`,
		},
		{
			name: "an mcp server", kind: configapi.EntityMCPServers, id: "tracker",
			body: `{"name":"issues","transport":"http","url":"https://mcp.example.com"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t, nil)
			s.seed(t, nestedDoc, nil)
			before := s.activeDocument(t)

			path := "/config/" + tc.kind + "/" + tc.id
			res := s.do(t, http.MethodPut, path, tc.body,
				map[string]string{"X-Summary": "rename it sideways"})
			if res.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s renaming the entity = %d, want 400: %s",
					path, res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), "identity_mismatch") {
				t.Errorf("the refusal does not say it was a rename: %s", res.Body.String())
			}
			// The refusal names the id the caller has to send back, so it
			// is actionable without reading the docs.
			if !strings.Contains(res.Body.String(), tc.id) {
				t.Errorf("the refusal does not name %q: %s", tc.id, res.Body.String())
			}
			// AND NOTHING WAS WRITTEN. A refused write that still stored a
			// revision would be the same rename, one indirection away.
			if after := s.activeDocument(t); after != before {
				t.Errorf("a refused rename changed the active document:\n%s", after)
			}
			ids, err := s.service().Entities(t.Context(), tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(ids, ","), tc.id) {
				t.Errorf("%s/%s is gone after a refused rename: %v", tc.kind, tc.id, ids)
			}
		})
	}
}

// AND AN EDIT THAT KEEPS THE IDENTITY STILL LANDS. The guard above must
// refuse renames, not name changes: a seat whose display name changes while
// its handle is sent back unchanged is an ordinary edit, and refusing it
// would make the surface useless for the thing it is most used for.
func TestAnEntityWriteAcceptsANameChangeThatKeepsTheHandle(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, nestedDoc, nil)

	res := s.do(t, http.MethodPut, "/config/roles/ceo",
		`{"name":"Chief Executive","handle":"ceo","llm":"zulu"}`,
		map[string]string{"X-Summary": "spell the CEO's title out"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT a renamed-but-same-handle seat = %d, want 201: %s",
			res.Code, res.Body.String())
	}
	if got := entityOf(t, s, configapi.EntityRoles, "ceo")["name"]; got != "Chief Executive" {
		t.Errorf("the name change did not land: name = %v", got)
	}
}
