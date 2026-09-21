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

// EVERY ADDRESSABLE COLLECTION IS LISTABLE, in a stable order, so a client
// can discover what is there rather than carrying its own copy of the list.
//
// ROLES AND UNITS ARE STILL LISTED, and that is deliberate rather than
// leftover: a revision written before the org chart moved onto its own log
// still carries both inside it, and an operator repairing one has to be able
// to see what it holds. The listing is a READ; the write is what moved.
func TestEveryEntityCollectionListsWhatTheDocumentCarries(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, entityDoc, nil)

	for _, tc := range []struct {
		kind string
		want []string
	}{
		{configapi.EntityLLMProviders, []string{"yankee", "zulu"}},
		{configapi.EntityMCPServers, []string{"notion", "tracker"}},
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
	// a settings revision carries no units at all, and null renders as a
	// failure where an empty list renders as "there are none".
	for _, kind := range []string{configapi.EntityUnits, configapi.EntityRoles} {
		got, err := s.service().Entities(t.Context(), kind)
		if err != nil {
			t.Fatalf("Entities(%s): %v", kind, err)
		}
		if got == nil {
			t.Errorf("a settings revision answered null for %s rather than an "+
				"empty list", kind)
		}
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
	s.seed(t, entityDoc, nil)

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
// this surface exists beside PUT /config: an operator changing one server's
// URL must not send back — and take responsibility for — every provider,
// every integration and every other server in the company.
func TestAnEntityWriteLeavesTheRestOfTheDocumentAlone(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, entityDoc, nil)

	server := entityOf(t, s, configapi.EntityMCPServers, "tracker")
	server["url"] = "https://tracker.example.com"
	body, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/mcp-servers/tracker", string(body),
		map[string]string{"X-Summary": "move the tracker server"})
	// 201, like PUT /config: the write creates a REVISION, which is a new
	// resource whether it changed one entity or the whole document.
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT /config/mcp-servers/tracker = %d: %s", res.Code, res.Body.String())
	}

	if got := entityOf(t, s, configapi.EntityMCPServers, "tracker")["url"]; got != "https://tracker.example.com" {
		t.Errorf("the edit did not land: url = %v", got)
	}
	// The server nobody touched is still there, and so is its credential,
	// which a whole-document write could have lost.
	ids, err := s.service().Entities(t.Context(), configapi.EntityMCPServers)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "notion,tracker" {
		t.Errorf("the other servers did not survive the write: %v", ids)
	}
	if document := s.activeDocument(t); !strings.Contains(document, "notion-literal") {
		t.Errorf("the untouched server's credential was lost: %s", document)
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

// THE WHOLE DOCUMENT IS VALIDATED, not just the entity — and the entity the
// caller sent is beside the point when the rest of the company is broken.
//
// # Why the break is somewhere the caller never looked
//
// A per-entity surface exists so that an operator does not have to send back
// the rest of the document. That is also what makes this rule load bearing:
// they cannot see the rest, so nothing in what they sent tells them the write
// will leave the company unrunnable. It used to be shown with a SEAT naming a
// missing provider, which is the same rule one field along — but every
// cross-block reference this document still has runs through a seat, and seats
// are the chart's now. So the break is seeded instead, in the half a revision
// still holds: a delegate template naming a provider nobody configures.
func TestAnEntityWriteThatBreaksTheCompanyIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seedStored(t, entityDoc, refusedByThisBuild)

	// A PERFECTLY GOOD ENTITY, and the write is still refused: what the
	// validation is about is nowhere in this body.
	server := entityOf(t, s, configapi.EntityMCPServers, "tracker")
	server["url"] = "https://tracker.example.com"
	body, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/mcp-servers/tracker", string(body),
		map[string]string{"X-Summary": "move the tracker server"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PUT onto a broken company = %d, want 400: %s",
			res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "validation_error") {
		t.Errorf("the refusal does not say it failed validation: %s", res.Body.String())
	}
	// AND IT NAMES THE PART THE CALLER CANNOT SEE, which is the only way
	// the refusal is actionable at all.
	if !strings.Contains(res.Body.String(), "nonexistent") {
		t.Errorf("the refusal does not name what is actually broken: %s", res.Body.String())
	}
}

// AN ID NOBODY CARRIES IS NOT CREATED. A PUT naming an entity that is not
// there is far more often a typo than an intent to add one, and creating
// through this route would let a caller grow the company without ever seeing
// the document they changed.
func TestAnEntityWriteNeverCreates(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, entityDoc, nil)

	res := s.do(t, http.MethodPut, "/config/llm-providers/xray",
		`{"type":"anthropic","model":"claude-sonnet-5","api_keys":["${X}"]}`,
		map[string]string{"X-Summary": "add a provider sideways"})
	if res.Code != http.StatusNotFound {
		t.Fatalf("PUT to an unknown id = %d, want 404: %s", res.Code, res.Body.String())
	}
	ids, err := s.service().Entities(t.Context(), configapi.EntityLLMProviders)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(ids, ","), "xray") {
		t.Errorf("the refused write created a provider anyway: %v", ids)
	}
}

// AND IT STILL NEEDS AN AUDIT SUMMARY. The history is what an operator reads
// to find the change that broke something, and a per-entity write is the one
// most likely to be made in a hurry.
func TestAnEntityWriteNeedsASummary(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, entityDoc, nil)

	res := s.do(t, http.MethodPut, "/config/llm-providers/zulu",
		`{"type":"anthropic","model":"claude-sonnet-5"}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PUT with no summary = %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "summary_required") {
		t.Errorf("the refusal does not name what is missing: %s", res.Body.String())
	}
}

// GET AND PUT ARE THE VERBS ON AN ENTITY PATH, and the refusal of the rest has
// to be legible.
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
func TestAnEntityPathRefusesEveryVerbButGetAndPut(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	for _, kind := range configapi.EntityKinds() {
		for _, method := range []string{
			http.MethodDelete, http.MethodPost, http.MethodPatch,
		} {
			res := s.do(t, method, "/config/"+kind+"/ceo", "", nil)
			if res.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /config/%s/ceo = %d, want 405 — an entity path serves "+
					"GET and PUT, and any other answer leaves an operator "+
					"guessing whether the write happened", method, kind, res.Code)
				continue
			}
			// RFC 9110 §15.5.6 makes the Allow header a MUST on a 405, and
			// it is the whole value of answering 405 rather than 404: it
			// names what would have worked.
			allow := res.Header().Get("Allow")
			for _, verb := range []string{http.MethodGet, http.MethodPut} {
				if !strings.Contains(allow, verb) {
					t.Errorf("%s /config/%s/ceo: Allow = %q, which does not name "+
						"%s — a verb this path has", method, kind, allow, verb)
				}
			}
		}
	}
}

// entityDoc carries TWO of each collection this surface still writes, which
// is what the single-member fixture cannot exercise: a write has to change one
// member and leave its neighbour exactly as it was, and a fixture with one of
// each cannot tell "edited the right one" from "edited the only one".
//
// NO ROLES AND NO UNITS. They are the org chart's own domain and this door
// refuses to write either (see chartdoor.go), so an entity fixture carrying
// them would be exercising that refusal instead of the write.
const entityDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal", "${ROTATED}"]
    yankee:
      type: anthropic
      model: claude-haiku-4-5
      api_keys: ["${YANKEE_KEY}"]
mcp_servers:
  - name: tracker
    transport: http
    url: https://mcp.example.com
  - name: notion
    command: notion-mcp
    env: {TOKEN: notion-literal}
`

// A SEAT AND A UNIT ARE NOT WRITTEN AT THIS DOOR, and the refusal says where
// they are.
//
// # Why a refusal rather than a 404
//
// The route would answer 404 on its own: a settings revision carries no
// `roles:` and no `units:`, so the splice finds nothing under any id. That is
// a true statement and a useless one — a founder whose company plainly HAS a
// CEO reads "no roles called ceo" as the engine having lost their org chart.
//
// # Why it is refused at all, given that it could never succeed
//
// It could, on one document: a revision written BEFORE the chart moved still
// carries both halves, and splicing into that one succeeds and stores another
// revision carrying a chart — which every node then refuses to apply. The
// write would look like it worked and the failure would surface at the next
// restart, naming a revision rather than the request that made it.
func TestASeatOrAUnitIsNotWrittenAtThisDoor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, id, body string }{
		{configapi.EntityRoles, "ceo", `{"name":"CEO","handle":"ceo","llm":"zulu"}`},
		{configapi.EntityUnits, "platform", `{"name":"platform","id":"platform"}`},
	} {
		t.Run(tc.kind+"/"+tc.id, func(t *testing.T) {
			t.Parallel()
			// A PRE-SPLIT REVISION, which is the one document the splice
			// could actually have landed on: on a settings-only revision
			// the route finds nothing and the refusal proves less.
			s := newSurface(t, nil)
			s.seedStored(t, duplicateNamesDoc, func(map[string]any) {})
			before := s.activeDocument(t)

			path := "/config/" + tc.kind + "/" + tc.id
			res := s.do(t, http.MethodPut, path, tc.body,
				map[string]string{"X-Summary": "edit it here"})
			if res.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400: %s", path, res.Code, res.Body.String())
			}
			body := decode(t, res)
			if body["error"] != "chart_not_writable_here" {
				t.Fatalf("error = %v, want chart_not_writable_here: %v", body["error"], body)
			}
			// THE REFUSAL POINTS AT SOMETHING THAT WORKS. A door that
			// only says no leaves an operator with a company they cannot
			// edit — and one naming a route this build does not serve is
			// worse, because it sends them to a 404 with the engine's own
			// words behind it. What is true today is the boot seed.
			hint, _ := body["hint"].(string)
			if !strings.Contains(hint, "crewlet run -company") {
				t.Errorf("the refusal does not name anything that works: %v", body)
			}
			if strings.Contains(hint, "/chart") {
				t.Errorf("the refusal names a route this build does not serve: %v", body)
			}
			// AND NOTHING WAS WRITTEN, which is the whole point: a
			// refused write that stored a revision would be the same
			// chart, one indirection away.
			if after := s.activeDocument(t); after != before {
				t.Errorf("a refused chart write changed the active document:\n%s", after)
			}
		})
	}

	// AND THE READ IS UNTOUCHED. A pre-split revision's chart is still
	// served, because an operator repairing one has to be able to see it.
	t.Run("the read still answers", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seedStored(t, duplicateNamesDoc, func(map[string]any) {})
		if res := s.do(t, http.MethodGet, "/config/units/platform", "", nil); res.Code != http.StatusOK {
			t.Errorf("GET a unit of a pre-split revision = %d, want 200: %s",
				res.Code, res.Body)
		}
	})
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
			name: "an mcp server", kind: configapi.EntityMCPServers, id: "tracker",
			body: `{"name":"issues","transport":"http","url":"https://mcp.example.com"}`,
		},
		{
			// THE ONE THAT ARRIVES BY ACCIDENT: a body with no `name` in
			// it at all, which reads as a rename to the empty string
			// rather than as "leave the name alone".
			name: "an mcp server whose body drops its name",
			kind: configapi.EntityMCPServers, id: "tracker",
			body: `{"transport":"http","url":"https://mcp.example.com"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t, nil)
			s.seed(t, entityDoc, nil)
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
// refuse renames, not edits: an entity whose every other field changes while
// its identity is sent back unchanged is the ordinary case, and refusing it
// would make the surface useless for the thing it is most used for.
func TestAnEntityWriteAcceptsAChangeThatKeepsTheIdentity(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, entityDoc, nil)

	res := s.do(t, http.MethodPut, "/config/mcp-servers/tracker",
		`{"name":"tracker","transport":"http","url":"https://issues.example.com"}`,
		map[string]string{"X-Summary": "point the tracker server somewhere else"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT an edited-but-same-name server = %d, want 201: %s",
			res.Code, res.Body.String())
	}
	if got := entityOf(t, s, configapi.EntityMCPServers, "tracker")["url"]; got != "https://issues.example.com" {
		t.Errorf("the edit did not land: url = %v", got)
	}
}

// AN ENTITY PATH IS A RESOURCE, WHICH MEANS IT CAN BE READ.
//
// The four paths accepted a write and answered 405 to a GET, so the entity a
// caller was expected to send back had to be fetched from a different URI
// space — /query/config_entities — which answers a {kind, id, entity}
// envelope that PUT does not accept. The documented loop therefore ran the
// read through `jq '.entity'` to make one half fit the other.
//
// The read is now the entity itself, so GET | PUT round-trips.
func TestAnEntityReadRoundTripsStraightBackIntoTheWrite(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, entityDoc, nil)

	// AN ENTITY CARRYING A CREDENTIAL, because that is where the round
	// trip is hardest: the read masks it and the write has to put the
	// stored value back, or the loop replaces a working token with the
	// mask.
	res := s.do(t, http.MethodGet, "/config/mcp-servers/notion", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config/mcp-servers/notion = %d: %s", res.Code, res.Body.String())
	}
	var entity map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &entity); err != nil {
		t.Fatalf("the read is not the entity: %v", err)
	}
	if entity["name"] != "notion" {
		t.Fatalf("the read is wrapped in an envelope rather than being the "+
			"entity PUT takes: %s", res.Body.String())
	}
	if tag := res.Header().Get("ETag"); tag == "" {
		t.Error("an entity read carries no ETag, so a conditional write on it " +
			"cannot be built from the read that produced it")
	}

	// STRAIGHT BACK, unmodified and unwrapped, guarded by the tag the read
	// gave. Anything less than 201 means the two halves still disagree.
	back := s.do(t, http.MethodPut, "/config/mcp-servers/notion", res.Body.String(),
		map[string]string{
			"X-Summary": "round trip", "If-Match": res.Header().Get("ETag"),
		})
	if back.Code != http.StatusCreated {
		t.Fatalf("the entity a GET returned was refused by its own PUT = %d: %s",
			back.Code, back.Body.String())
	}
	// AND THE CREDENTIAL SURVIVED IT. A round trip that stored the mask
	// would answer 201 and break the server hours later, naming nothing.
	if document := s.activeDocument(t); !strings.Contains(document, "notion-literal") {
		t.Errorf("the round trip replaced the credential with its mask:\n%s", document)
	}
}

// AND IT IS REDACTED, because it is a slice of a document that is. Otherwise
// the new read is a way to fetch every credential in the company one entity
// at a time, past the masking the document read applies.
func TestAnEntityReadOverHTTPCarriesNoCredential(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodGet, "/config/llm-providers/zulu", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config/llm-providers/zulu = %d", res.Code)
	}
	if strings.Contains(res.Body.String(), "sk-literal") {
		t.Fatalf("a literal credential came back over HTTP: %s", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "${") {
		t.Errorf("the ${VAR} reference was masked away, so an editor cannot see "+
			"what it points at: %s", res.Body.String())
	}
}

// AN ID NOBODY CARRIES IS A 404 ON THE READ TOO, not an empty entity.
func TestReadingAnAbsentEntityIsNotFound(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodGet, "/config/roles/nobody", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("GET an absent seat = %d, want 404: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "no_such_entity") {
		t.Errorf("the refusal does not name what was missing: %s", res.Body.String())
	}
}

// AN ID NOBODY CARRIES IS A 404 EVEN WHEN THE BODY ALSO DISAGREES.
//
// The two refusals answer different questions, and the order decides which
// one a caller is told about. Judging the body's identity first calls a PUT
// to an absent id a RENAME and blames the body — when the entity addressed is
// simply not there and the URL is what went wrong. A caller who mistypes the
// path while sending a correct entity is the ordinary case, and it must be
// pointed at the path.
func TestAnAbsentEntityIsNotFoundBeforeItIsARename(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, kind, id, body string }{
		{
			// The mistyped URL: the body is a perfectly good server.
			name: "an mcp server", kind: configapi.EntityMCPServers, id: "nothing",
			body: `{"name":"tracker","transport":"http","url":"https://mcp.example.com"}`,
		},
		{
			name: "an llm provider", kind: configapi.EntityLLMProviders, id: "nothing",
			body: `{"type":"anthropic","model":"claude-sonnet-5","api_keys":["${K}"]}`,
		},
		{
			// And a body that cannot be read at all: the path is what went
			// wrong first, and a body is read at the place of the entity
			// it replaces, which a missing entity does not have.
			name: "an mcp server whose body has a typo",
			kind: configapi.EntityMCPServers, id: "nothing",
			body: `{"name":"tracker","transport":"http","url":"https://mcp.example.com","comand":"npx"}`,
		},
		{
			name: "an llm provider whose body has a typo",
			kind: configapi.EntityLLMProviders, id: "nothing",
			body: `{"type":"anthropic","modell":"claude-sonnet-5"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t, nil)
			s.seed(t, entityDoc, nil)

			path := "/config/" + tc.kind + "/" + tc.id
			res := s.do(t, http.MethodPut, path, tc.body,
				map[string]string{"X-Summary": "write to an id nothing carries"})
			if res.Code != http.StatusNotFound {
				t.Fatalf("PUT %s = %d, want 404 — the entity addressed is not "+
					"there, which is a fact about the path rather than the "+
					"body: %s", path, res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), "no_such_entity") {
				t.Errorf("the refusal blames the body rather than the path: %s",
					res.Body.String())
			}
		})
	}
}

// A TYPO IN AN ENTITY BODY IS REFUSED, not dropped.
//
// `json.Unmarshal` ignores what it does not recognise, so `"gaol"` answered
// 201 and stored a seat with no goal — the exact failure Tier B's strict
// document parser exists to prevent, on the surface most likely to be
// hand-edited in a hurry. Asserted per kind because each one decodes into its
// own type and a new kind added without the strict decoder is silent.
func TestAMistypedFieldInAnEntityBodyIsRefusedByName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind string
		id   string
		body string
		// at is where the typo sits in the WHOLE document, which is where
		// a refusal's problems are placed on every route.
		at string
	}{
		{configapi.EntityLLMProviders, "zulu", `{"type":"anthropic","modell":"claude-sonnet-5"}`, "providers.llm.zulu.modell"},
		{configapi.EntityLLMProviders, "yankee", `{"type":"anthropic","model":"m","api_kies":["${K}"]}`, "providers.llm.yankee.api_kies"},
		{configapi.EntityMCPServers, "tracker", `{"name":"tracker","transport":"http","url":"https://mcp.example.com","comand":"npx"}`,
			"mcp_servers[0].comand"},
		{configapi.EntityMCPServers, "notion", `{"name":"notion","command":"notion-mcp","enviroment":{}}`,
			"mcp_servers[1].enviroment"},
	} {
		t.Run(tc.kind+"/"+tc.id, func(t *testing.T) {
			t.Parallel()
			// entityDoc, because every kind has to address something
			// that EXISTS: a 404 for a missing entity would pass this
			// test without the body ever being decoded. And the SECOND
			// member of each collection is what pins the index in the
			// placed path — one of each cannot tell `[0]` from "the
			// one there is".
			s := newSurface(t, nil)
			s.seed(t, entityDoc, nil)

			res := s.do(t, http.MethodPut, "/config/"+tc.kind+"/"+tc.id, tc.body,
				map[string]string{"X-Summary": "with a typo in it"})
			if res.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s/%s with an unknown field = %d, want 400: %s",
					tc.kind, tc.id, res.Code, res.Body.String())
			}
			// AND IT NAMES THE FIELD. A caller who mistyped one key needs to
			// be told which one, not that "the body is invalid".
			if !strings.Contains(res.Body.String(), "invalid_body") {
				t.Errorf("the refusal is not reported as a body problem: %s", res.Body.String())
			}
			// AND PLACES IT, in the document the entity is spliced into,
			// as the unknown field it is: the same problem the same typo
			// in a whole document is, so a screen puts it beside the field.
			problems := problemsOf(t, res)
			if len(problems) != 1 || problems[0].Path != tc.at || problems[0].Kind != "unknown_field" {
				t.Errorf("problems = %+v, want one unknown_field at %s", problems, tc.at)
			} else if !strings.Contains(decode(t, res)["detail"].(string), problems[0].Message) {
				t.Errorf("the problem %q is not a line of the detail: %s", problems[0].Message, res.Body)
			}
		})
	}
}

// A UNIT OF A PRE-SPLIT REVISION IS ADDRESSED BY ITS KEY ON EVERY PATH THAT
// STILL TOUCHES IT.
//
// The listing and the lookup have to agree, and for one release they did not:
// one keyed on the display name and the other on the key. On any document
// whose units declare an id those are different values, so a client listing
// the chart of a revision it is repairing was handed ids that 404 when it
// asked for them.
//
// THE WRITE HALF IS GONE and the refusal is asserted in its place: the splice
// this used to exercise is what `chart_not_writable_here` now stops, and the
// splice's own agreement with the lookup is checked by the chart's writer
// rather than here.
func TestAUnitOfAnOldRevisionIsAddressedByItsKey(t *testing.T) {
	t.Parallel()
	const doc = `{"name":"Acme",
	  "providers":{"llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["${K}"]}}},
	  "units":[{"name":"Platform Engineering","id":"plat",
	    "roles":[{"name":"Engineer","handle":"eng","llm":"zulu"}]}]}`

	s := newSurface(t, nil)
	s.seed(t, doc, nil)

	// THE LISTING NAMES THE KEY, which is what a client then addresses.
	ids, err := s.service().Entities(t.Context(), configapi.EntityUnits)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	if len(ids) != 1 || ids[0] != "plat" {
		t.Fatalf("the listing names %v, want the unit's key", ids)
	}

	// THE LOOKUP ANSWERS UNDER IT, and not under the display name: a
	// fixture whose key and name were the same could not tell the two
	// apart, so this one's differ.
	if res := s.do(t, http.MethodGet, "/config/units/plat", "", nil); res.Code != http.StatusOK {
		t.Fatalf("GET by key = %d, want 200: %s", res.Code, res.Body)
	}
	byName := s.do(t, http.MethodGet, "/config/units/Platform%20Engineering", "", nil)
	if byName.Code != http.StatusNotFound {
		t.Errorf("GET by display name = %d, want 404 — nothing in the document "+
			"resolves a unit by its name", byName.Code)
	}

	// AND THE ENTITY IT ANSWERS WITH IS NOT WRITABLE HERE, under the key
	// it was just found by: the address resolves and the write is still
	// refused, which is what makes the refusal about the CHART rather than
	// about a unit this surface could not find.
	entity := s.do(t, http.MethodGet, "/config/units/plat", "", nil)
	put := s.do(t, http.MethodPut, "/config/units/plat", entity.Body.String(),
		map[string]string{"X-Summary": "round trip"})
	if put.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400: %s", put.Code, put.Body)
	}
	if got := decode(t, put)["error"]; got != "chart_not_writable_here" {
		t.Errorf("error = %v, want chart_not_writable_here", got)
	}
}
