package configapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// leadDoc is a company with a lead in the middle of it. The Platform lead
// leads Platform and the Tooling team inside it; Engineering above them is
// the CTO's, and Data beside them and Design beside Engineering are other
// people's. The CEO sits at the root, which is nobody's subtree.
const leadDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal"]
mcp_servers:
  - name: github
    command: github-mcp
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Engineering
    id: engineering
    lead: cto
    project: ENG
    roles:
      - name: CTO
        handle: cto
        llm: zulu
    children:
      - name: Platform
        id: platform
        lead: platform-lead
        purpose: keep the lights on
        roles:
          - name: Platform Lead
            handle: platform-lead
            kind: human
          - name: Staff Engineer
            handle: staff-eng
            llm: zulu
            mcp_env:
              github:
                GITHUB_TOKEN: ghp-literal-token
          - name: SRE
            handle: sre
            llm: zulu
        children:
          - name: Tooling
            id: tooling
            purpose: build tools
            roles:
              - name: Toolsmith
                handle: toolsmith
                llm: zulu
      - name: Data
        id: data
        lead: data-lead
        roles:
          - name: Data Lead
            handle: data-lead
            llm: zulu
  - name: Design
    id: design
    lead: designer
    project: DSN
    roles:
      - name: Designer
        handle: designer
        llm: zulu
`

// platformLead is the person bound to the Platform lead's seat, holding no
// grant at all: whatever they may write, they may write as its lead.
func platformLead() iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "pat.lead", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: "platform-lead",
		ReauthAt: time.Now().Add(time.Hour)}
}

// leadSurface is the surface over leadDoc.
func leadSurface(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.seed(t, leadDoc)
	return s
}

// doAs is one request as p.
func doAs(t *testing.T, s *surface, p iam.Principal, method, path, body string,
	headers map[string]string) *httptest.ResponseRecorder {

	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req = req.WithContext(iam.WithPrincipal(req.Context(), p))
	res := httptest.NewRecorder()
	s.mux.ServeHTTP(res, req)
	return res
}

// putEntityAs reads one seat or unit as the company's own grant would, lets
// the case edit it, and writes it back as p.
func putEntityAs(t *testing.T, s *surface, p iam.Principal, kind, id string,
	change func(map[string]any), query string) *httptest.ResponseRecorder {

	t.Helper()
	entity := entityOf(t, s, kind, id)
	change(entity)
	body, err := json.Marshal(entity)
	if err != nil {
		t.Fatal(err)
	}
	return doAs(t, s, p, http.MethodPut, "/config/"+kind+"/"+id+query, string(body),
		map[string]string{"X-Summary": "a lead's edit"})
}

// refusedParts is a 403's refused list as kind/id/side/place/why/reason.
func refusedParts(t *testing.T, res *httptest.ResponseRecorder) []string {
	t.Helper()
	if res.Code != http.StatusForbidden {
		t.Fatalf("answered %d, want 403: %s", res.Code, res.Body.String())
	}
	body := decode(t, res)
	if body["error"] != string(httpjson.CodeUnauthorized) {
		t.Fatalf("refused as %v, want unauthorized: %v", body["error"], body)
	}
	if grants, _ := body[authz.DetailGrants].([]any); !slices.Equal(grants,
		[]any{string(iam.GrantConfigWrite)}) {
		t.Errorf("grants = %v, want the company's grant that would admit all of it",
			body[authz.DetailGrants])
	}
	list, _ := body["refused"].([]any)
	var out []string
	for _, item := range list {
		r, _ := item.(map[string]any)
		out = append(out, strings.Join([]string{str(r["kind"]), str(r["id"]),
			str(r["side"]), str(r["place"]), str(r["why"]), str(r["reason"])}, "/"))
	}
	if len(out) == 0 {
		t.Fatalf("a 403 named no refused part: %v", body)
	}
	return out
}

func str(v any) string { s, _ := v.(string); return s }

// rolesOf is a unit entity's seat list, for an edit to change.
func rolesOf(unit map[string]any) []any { list, _ := unit["roles"].([]any); return list }

// childOf is a unit entity's child with this key.
func childOf(t *testing.T, unit map[string]any, key string) map[string]any {
	t.Helper()
	children, _ := unit["children"].([]any)
	for _, c := range children {
		if child, _ := c.(map[string]any); child["id"] == key {
			return child
		}
	}
	t.Fatalf("no child %q in %v", key, unit["id"])
	return nil
}

// putDocumentAs sends leadDoc back whole through PUT /config as p, with the
// case's edit made to the document.
func putDocumentAs(t *testing.T, s *surface, p iam.Principal,
	change func(map[string]any)) *httptest.ResponseRecorder {

	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(companyJSON(t, leadDoc), &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doAs(t, s, p, http.MethodPut, "/config", string(body),
		map[string]string{"X-Summary": "a lead's edit"})
}

// topUnit is a whole document's top-level unit with this key.
func topUnit(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	units, _ := doc["units"].([]any)
	for _, u := range units {
		if unit, _ := u.(map[string]any); unit["id"] == key {
			return unit
		}
	}
	t.Fatalf("no top-level unit %q", key)
	return nil
}

// takeSeat removes the seat with this handle from a list, answering both.
func takeSeat(t *testing.T, list []any, handle string) ([]any, any) {
	t.Helper()
	for i, item := range list {
		if seat, _ := item.(map[string]any); seat["handle"] == handle {
			return slices.Delete(slices.Clone(list), i, i+1), item
		}
	}
	t.Fatalf("no seat %q", handle)
	return nil, nil
}

// A LEAD WRITES INSIDE THEIR SUBTREE, AND ONLY THERE.
//
// Every case is one write by the Platform lead, who holds no grant. What they
// may do is everything about the seats and units inside Platform — its own
// fields, a seat's, a seat added, moved between two of their teams, a team
// removed — and what they may not is anything that reaches outside it on
// either side of the write: handing Platform to somebody else, removing or
// moving the unit they lead, moving a seat out, making a seat manage somebody
// outside, touching a root seat, changing a key another system finds a seat
// or a team by — a project, even one nothing in the company names any more,
// a channel or a contact id — changing a credential — an `mcp_env` key with
// nothing under it among them, since the key alone starts that tool server
// for the seat, and a credential emptied or removed — or naming a `${VAR}`
// anywhere — a contact id or an address that names one is resolved from the
// engine's own environment and recited to whoever looks the seat up.
//
// A case with no kind is a whole-document write, its edit handed the whole
// document: a seat or a unit outside their subtree is refused at its own
// address before anything is built (TestAWriteIsDecidedBeforeItsIDIsLookedUp),
// so what reaches outside from a write that also reaches inside is asked
// there.
//
// Mutation: decide every place against the document being replaced alone and
// the cases that only the proposed document refuses — clearing the unit's
// lead, a seat moved out — are admitted; drop the key check and the project,
// the channel and the contact id are admitted; list only credential fields and the contact id and the
// address naming a variable are admitted; compare a credential field's
// strings alone and the empty server blocks are admitted; list only what the
// proposed document holds and the emptied and removed credentials are.
func TestALeadWritesInsideTheirSubtreeAndOnlyThere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		kind, id   string // both empty for a whole-document write
		change     func(t *testing.T, entity map[string]any)
		admitted   bool
		refusedHas string // kind/id/side/place/why/reason, for a refusal
	}{
		{"a seat's goal", configapi.EntityRoles, "sre",
			func(_ *testing.T, e map[string]any) { e["goal"] = "keep it up" }, true, ""},
		{"a sub-team's purpose", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) { e["purpose"] = "build better tools" }, true, ""},
		{"their own team's purpose", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) { e["purpose"] = "ship the platform" }, true, ""},
		{"a seat added to their team", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) {
				e["roles"] = append(rolesOf(e), map[string]any{
					"name": "Intern", "handle": "intern", "llm": "zulu"})
			}, true, ""},
		{"a sub-team removed", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) { delete(e, "children") }, true, ""},
		{"a seat moved between two of their teams", configapi.EntityUnits, "platform",
			func(t *testing.T, e map[string]any) {
				var sre any
				e["roles"], sre = takeSeat(t, rolesOf(e), "sre")
				tooling := childOf(t, e, "tooling")
				tooling["roles"] = append(rolesOf(tooling), sre)
			}, true, ""},
		{"a seat's masked credential sent back as it was read", configapi.EntityRoles,
			"staff-eng", func(_ *testing.T, e map[string]any) { e["goal"] = "ship" }, true, ""},

		{"their own team handed to an outsider", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) { e["lead"] = "data-lead" }, false,
			"unit/platform/after/platform/self/not_lead"},
		{"their own team's lead cleared", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) { delete(e, "lead") }, false,
			"unit/platform/after/platform/self/not_lead"},
		{"a sub-team led by an outsider", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) { e["lead"] = "designer" }, false,
			"unit/tooling/after/design/lead/not_lead"},
		{"their own team removed", "", "",
			func(t *testing.T, doc map[string]any) {
				e := topUnit(t, doc, "engineering")
				children, _ := e["children"].([]any)
				e["children"] = []any{children[1]} // Data stays
			}, false, "unit/platform/before/engineering/place/not_lead"},
		{"a seat moved out of their team", "", "",
			func(t *testing.T, doc map[string]any) {
				e := topUnit(t, doc, "engineering")
				platform, data := childOf(t, e, "platform"), childOf(t, e, "data")
				var sre any
				platform["roles"], sre = takeSeat(t, rolesOf(platform), "sre")
				data["roles"] = append(rolesOf(data), sre)
			}, false, "seat/sre/after/data/place/not_lead"},
		{"a seat made to manage somebody outside", configapi.EntityRoles, "sre",
			func(_ *testing.T, e map[string]any) { e["manages"] = []any{"designer"} }, false,
			"seat/sre/after/design/manages/not_lead"},
		{"a seat made to manage the CEO", configapi.EntityRoles, "sre",
			func(_ *testing.T, e map[string]any) { e["manages"] = []any{"ceo"} }, false,
			"seat/sre/after//manages/not_lead"},
		{"another team's project declared", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) { e["project"] = "DSN" }, false,
			"unit/tooling///key/no_grant"},
		{"a project nothing in the company names", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) { e["project"] = "SALES" }, false,
			"unit/tooling///key/no_grant"},
		{"their own team's channel", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) { e["channel"] = "platform" }, false,
			"unit/platform///key/no_grant"},
		{"a literal contact id on their own seat", configapi.EntityRoles, "platform-lead",
			func(_ *testing.T, e map[string]any) {
				e["contact"] = map[string]any{"slack_user_id": "U0PLATFORM"}
			}, false, "seat/platform-lead///key/no_grant"},
		{"a seat at the root", "", "",
			func(_ *testing.T, doc map[string]any) {
				roles, _ := doc["roles"].([]any)
				ceo, _ := roles[0].(map[string]any)
				ceo["goal"] = "grow"
			}, false, "seat/ceo/before//place/not_lead"},
		{"a credential changed inside their team", configapi.EntityRoles, "staff-eng",
			func(_ *testing.T, e map[string]any) {
				e["mcp_env"] = map[string]any{"github": map[string]any{
					"GITHUB_TOKEN": "${CEO_GITHUB_TOKEN}"}}
			}, false, "seat/staff-eng///credential/no_grant"},
		{"a seat's credential emptied", configapi.EntityRoles, "staff-eng",
			func(_ *testing.T, e map[string]any) { e["mcp_env"] = map[string]any{} }, false,
			"seat/staff-eng///credential/no_grant"},
		{"a seat's credential removed", configapi.EntityRoles, "staff-eng",
			func(_ *testing.T, e map[string]any) { delete(e, "mcp_env") }, false,
			"seat/staff-eng///credential/no_grant"},
		{"a tool server attached to a seat by an empty block", configapi.EntityRoles, "sre",
			func(_ *testing.T, e map[string]any) {
				e["mcp_env"] = map[string]any{"github": map[string]any{}}
			}, false, "seat/sre///credential/no_grant"},
		{"a tool server attached to a team by an empty block", configapi.EntityUnits,
			"tooling", func(_ *testing.T, e map[string]any) {
				e["mcp_env"] = map[string]any{"github": map[string]any{"GITHUB_TOKEN": ""}}
			}, false, "unit/tooling///credential/no_grant"},
		{"a contact id naming a variable on their own seat", configapi.EntityRoles,
			"platform-lead", func(_ *testing.T, e map[string]any) {
				e["contact"] = map[string]any{"slack_user_id": "${CREWLET_KEYRING}"}
			}, false, "seat/platform-lead///credential/no_grant"},
		{"an address naming a variable", configapi.EntityRoles, "sre",
			func(_ *testing.T, e map[string]any) { e["email"] = "${SOME_SECRET}" }, false,
			"seat/sre///credential/no_grant"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := leadSurface(t)
			edit := func(e map[string]any) { c.change(t, e) }
			var res *httptest.ResponseRecorder
			if c.kind == "" {
				res = putDocumentAs(t, s, platformLead(), edit)
			} else {
				res = putEntityAs(t, s, platformLead(), c.kind, c.id, edit, "")
			}
			if c.admitted {
				if res.Code != http.StatusCreated {
					t.Fatalf("a lead's write inside their subtree answered %d: %s",
						res.Code, res.Body.String())
				}
				return
			}
			if parts := refusedParts(t, res); !slices.Contains(parts, c.refusedHas) {
				t.Errorf("refused %v, want it to name %s", parts, c.refusedHas)
			}
		})
	}
}

// A LEAD CANNOT TAKE OVER A REFERENCE BY ADDING WHAT IT NAMES.
//
// A `lead:` may name a seat nobody has added yet, and Design's here does.
// Adding a seat under that handle to Platform changes nothing about Design,
// yet in the proposed document that seat would lead Design and the Platform
// lead would be above every member of it — so the addition reaches Design,
// before the write, and is refused there. The same addition under a name a
// unit INSIDE their subtree states is theirs: Tooling's dangling lead is.
//
// Mutation: drop the referrers from an addition and the ghost seat lands,
// leaving the Platform lead leading Design.
func TestALeadCannotTakeOverAReferenceByAddingWhatItNames(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(leadDoc, "    lead: designer\n", "    lead: ghost\n", 1)
	doc = strings.Replace(doc, "            purpose: build tools\n",
		"            purpose: build tools\n            lead: tools-lead\n", 1)
	if !strings.Contains(doc, "lead: ghost") || !strings.Contains(doc, "lead: tools-lead") {
		t.Fatal("the fixture did not take the dangling leads")
	}
	add := func(handle string) func(map[string]any) {
		return func(e map[string]any) {
			e["roles"] = append(rolesOf(e), map[string]any{
				"name": handle, "handle": handle, "llm": "zulu"})
		}
	}
	s := newSurface(t)
	s.seed(t, doc)
	res := putEntityAs(t, s, platformLead(), configapi.EntityUnits, "platform", add("ghost"), "")
	if parts := refusedParts(t, res); !slices.Contains(parts,
		"seat/ghost/before/design/named/not_lead") {
		t.Errorf("refused %v, want Design's lead named", parts)
	}
	if res := putEntityAs(t, s, platformLead(), configapi.EntityUnits, "platform",
		add("tools-lead"), ""); res.Code != http.StatusCreated {
		t.Errorf("a seat added under a name their own sub-team states = %d: %s",
			res.Code, res.Body.String())
	}
}

// A LEAD'S WHOLE-DOCUMENT WRITES ARE JUDGED THE SAME WAY, and so is a revert.
//
// PUT, PATCH and a revert reach the same admission an entity write does: a
// change inside the subtree lands and one that also touches another team is
// refused. A revert is judged on its WHOLE diff — from the revision active now
// to the one reverted to — so reverting to a revision an administrator wrote
// is refused when it would undo anything outside the lead's subtree.
func TestALeadsWholeDocumentWritesAreJudgedTheSameWay(t *testing.T) {
	t.Parallel()
	inside := strings.Replace(leadDoc, "build tools", "build better tools", 1)
	outside := strings.Replace(inside, "    project: DSN\n",
		"    project: DSN\n    purpose: draw\n", 1)

	t.Run("PUT", func(t *testing.T) {
		t.Parallel()
		s := leadSurface(t)
		if res := doAs(t, s, platformLead(), http.MethodPut, "/config",
			string(companyJSON(t, inside)), map[string]string{"X-Summary": "tools"}); res.Code != http.StatusCreated {
			t.Fatalf("a whole document changing only their subtree = %d: %s",
				res.Code, res.Body.String())
		}
		res := doAs(t, s, platformLead(), http.MethodPut, "/config",
			string(companyJSON(t, outside)), map[string]string{"X-Summary": "design"})
		if parts := refusedParts(t, res); !slices.Contains(parts,
			"unit/design/before/design/self/not_lead") {
			t.Errorf("refused %v, want it to name Design", parts)
		}
	})
	t.Run("PATCH", func(t *testing.T) {
		t.Parallel()
		s := leadSurface(t)
		units := func(doc string) string {
			var company map[string]any
			if err := json.Unmarshal(companyJSON(t, doc), &company); err != nil {
				t.Fatal(err)
			}
			patch, err := json.Marshal(map[string]any{"units": company["units"]})
			if err != nil {
				t.Fatal(err)
			}
			return string(patch)
		}
		headers := map[string]string{"X-Summary": "tools",
			"Content-Type": "application/merge-patch+json"}
		if res := doAs(t, s, platformLead(), http.MethodPatch, "/config",
			units(inside), headers); res.Code != http.StatusCreated {
			t.Fatalf("a patch changing only their subtree = %d: %s",
				res.Code, res.Body.String())
		}
		if parts := refusedParts(t, doAs(t, s, platformLead(), http.MethodPatch,
			"/config", units(outside), headers)); !slices.Contains(parts,
			"unit/design/before/design/self/not_lead") {
			t.Errorf("refused %v, want it to name Design", parts)
		}
	})
	t.Run("revert", func(t *testing.T) {
		t.Parallel()
		s := leadSurface(t)
		base := activeRevision(t, s)
		// AN ADMINISTRATOR CHANGES BOTH TEAMS; the lead reverting all of it
		// would undo Design's edit too.
		if res := s.do(t, http.MethodPut, "/config", string(companyJSON(t, outside)),
			map[string]string{"X-Summary": "both"}); res.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", res.Code, res.Body.String())
		}
		if parts := refusedParts(t, doAs(t, s, platformLead(), http.MethodPost,
			"/config/revisions/"+base+"/revert", "", nil)); !slices.Contains(parts,
			"unit/design/before/design/self/not_lead") {
			t.Errorf("refused %v, want the revert's edit of Design named", parts)
		}
		// AND A REVERT WHOSE WHOLE DIFF IS INSIDE THEIR SUBTREE LANDS.
		mine := activeRevision(t, s)
		if res := s.do(t, http.MethodPut, "/config", string(companyJSON(t,
			strings.Replace(outside, "build better tools", "build the best tools", 1))),
			map[string]string{"X-Summary": "tools again"}); res.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", res.Code, res.Body.String())
		}
		if res := doAs(t, s, platformLead(), http.MethodPost,
			"/config/revisions/"+mine+"/revert", "", nil); res.Code != http.StatusCreated {
			t.Errorf("a revert inside their subtree = %d: %s", res.Code, res.Body.String())
		}
	})
}

// activeRevision is the id of the revision active now.
func activeRevision(t *testing.T, s *surface) string {
	t.Helper()
	revision, found, err := s.configs.Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active revision: %v (found=%v)", err, found)
	}
	return revision.ID
}

// A SETTING IS NEVER A LEAD'S, and the refusal names its key — on a dry run
// as on a write, so a lead learns before saving what would be refused.
func TestALeadsSettingChangeIsRefusedNamingTheKey(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	before := activeRevision(t, s)
	for _, query := range []string{"?dry_run=true", ""} {
		res := doAs(t, s, platformLead(), http.MethodPatch, "/config"+query,
			`{"mission": "make tools"}`, map[string]string{"X-Summary": "mission"})
		if parts := refusedParts(t, res); !slices.Equal(parts,
			[]string{"setting/mission////no_grant"}) {
			t.Errorf("PATCH%s refused %v, want the mission named alone", query, parts)
		}
	}
	// AND A DRY RUN OF WHAT THEY MAY DO ANSWERS AS ONE: valid, nothing stored.
	res := putEntityAs(t, s, platformLead(), configapi.EntityRoles, "sre",
		func(e map[string]any) { e["goal"] = "keep it up" }, "?dry_run=true")
	if res.Code != http.StatusOK || decode(t, res)["valid"] != true {
		t.Errorf("a lead's dry run inside their subtree = %d: %s", res.Code, res.Body.String())
	}
	if after := activeRevision(t, s); after != before {
		t.Errorf("a refused write or a dry run moved the active revision to %s", after)
	}
}

// A WRITE THAT CHANGES NOTHING IS THE COMPANY GRANT'S.
//
// Storing the document unchanged makes a new revision and re-activates it on
// every node — `POST /config/reload`'s gesture — so a caller without
// `config:write` is refused it however it arrives: a patch of a key that is
// already unset, by a person who leads nothing, and an entity sent back as it
// was read, by a lead. The revision does not move. The control is the company's
// grant, whose same patch lands.
//
// Mutation: admit a diff with no part refused, as before, and both writes
// land as new revisions.
func TestAWriteThatChangesNothingIsTheCompanyGrants(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	before := activeRevision(t, s)
	colleague := platformLead()
	colleague.Seat = "toolsmith"
	headers := map[string]string{"X-Summary": "nothing"}
	if parts := refusedParts(t, doAs(t, s, colleague, http.MethodPatch, "/config",
		`{"vision": null}`, headers)); !slices.Equal(parts,
		[]string{"document////unchanged/no_grant"}) {
		t.Errorf("a no-op patch refused %v, want the unchanged document named", parts)
	}
	if parts := refusedParts(t, putEntityAs(t, s, platformLead(), configapi.EntityRoles,
		"sre", func(map[string]any) {}, "")); !slices.Equal(parts,
		[]string{"document////unchanged/no_grant"}) {
		t.Errorf("a lead's unchanged seat refused %v, want the unchanged document named",
			parts)
	}
	if after := activeRevision(t, s); after != before {
		t.Errorf("a refused no-op moved the active revision to %s", after)
	}
	admin := iam.Principal{ID: uuid.New(), Login: "ops.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantConfigWrite},
		ReauthAt: time.Now().Add(time.Hour)}
	if res := doAs(t, s, admin, http.MethodPatch, "/config", `{"vision": null}`,
		headers); res.Code != http.StatusCreated {
		t.Errorf("config:write re-publishing the document = %d: %s", res.Code, res.Body.String())
	}
}

// THE COMPANY'S GRANT IS THE ADMIN PATH: whoever holds config:write changes
// anything, a root seat and a setting included, leading nothing at all.
func TestConfigWriteIsTheAdminPath(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	admin := iam.Principal{ID: uuid.New(), Login: "ops.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantConfigWrite},
		ReauthAt: time.Now().Add(time.Hour)}
	if res := putEntityAs(t, s, admin, configapi.EntityRoles, "ceo",
		func(e map[string]any) { e["goal"] = "grow" }, ""); res.Code != http.StatusCreated {
		t.Errorf("config:write editing a root seat = %d: %s", res.Code, res.Body.String())
	}
	if res := doAs(t, s, admin, http.MethodPatch, "/config", `{"mission": "grow"}`,
		map[string]string{"X-Summary": "mission"}); res.Code != http.StatusCreated {
		t.Errorf("config:write editing a setting = %d: %s", res.Code, res.Body.String())
	}
}

// THE ROUTE REFUSES SOMEBODY WHO COULD LEAD NOTHING BEFORE THE BODY IS READ.
//
// A person bound to no seat is no unit's lead, so a body they send is never
// parsed: an unreadable one is refused 403 rather than 400. The control is a
// person bound to a seat, whose same body is read — and refused as unreadable
// — because whether they lead what it changes is only known once it is. An
// agent never passes, whatever it holds: a seat rewriting the org it runs
// inside is a model choosing its own team.
func TestTheRouteRefusesSomebodyWhoCouldLeadNothing(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	unbound := platformLead()
	unbound.Seat = ""
	headers := map[string]string{"X-Summary": "x"}
	if res := doAs(t, s, unbound, http.MethodPut, "/config/roles/sre", "{not json",
		headers); res.Code != http.StatusForbidden {
		t.Errorf("somebody bound to no seat = %d, want 403 before the body: %s",
			res.Code, res.Body.String())
	}
	if res := doAs(t, s, platformLead(), http.MethodPut, "/config/roles/sre", "{not json",
		headers); res.Code != http.StatusBadRequest {
		t.Errorf("a person bound to a seat = %d, want the body read and refused: %s",
			res.Code, res.Body.String())
	}
	agent := platformLead()
	agent.Kind, agent.Grants = iam.KindSeat, iam.AllGrants
	if res := doAs(t, s, agent, http.MethodPut, "/config/roles/sre", "{}",
		headers); res.Code != http.StatusForbidden ||
		decode(t, res)[authz.DetailReason] != string(authz.ReasonSeatRefused) {
		t.Errorf("an agent holding every grant = %d %s, want seat_refused",
			res.Code, res.Body.String())
	}
}

// A LEAD READS WHAT THEY MAY WRITE, AND NOTHING ELSE.
//
// One seat or one unit of their subtree, masked as every read of the document
// is — and not a seat or a unit outside it, nor the whole document, which is
// `config:read`'s.
func TestALeadReadsOnlyTheirSubtree(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	for path, want := range map[string]int{
		"/config/roles/sre":          http.StatusOK,
		"/config/units/platform":     http.StatusOK,
		"/config/units/tooling":      http.StatusOK,
		"/config/roles/designer":     http.StatusForbidden,
		"/config/units/engineering":  http.StatusForbidden,
		"/config/roles/ceo":          http.StatusForbidden,
		"/config":                    http.StatusForbidden,
		"/config/mcp-servers/github": http.StatusForbidden,
	} {
		if res := doAs(t, s, platformLead(), http.MethodGet, path, "", nil); res.Code != want {
			t.Errorf("GET %s = %d, want %d: %s", path, res.Code, want, res.Body.String())
		}
	}
	res := doAs(t, s, platformLead(), http.MethodGet, "/config/roles/staff-eng", "", nil)
	if strings.Contains(res.Body.String(), "ghp-literal-token") {
		t.Errorf("a lead's read carried a credential in the clear: %s", res.Body.String())
	}
}

// A READ IS DECIDED BEFORE ITS ID IS LOOKED UP.
//
// A person bound to a seat who leads nothing is refused a seat in another
// team, a seat at the root, a unit, and an id the company does not have, in
// the same bytes: an id the revision does not hold is decided at the root, and
// the root is refused as another team's unit is. The control is the company's
// read grant, which is told the id is not there.
//
// Mutation: look the id up before deciding, or give the root a refusal reason
// of its own, and the missing id answers differently from the seat in Design.
func TestAReadIsDecidedBeforeItsIDIsLookedUp(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	colleague := platformLead()
	colleague.Seat = "toolsmith"
	var first string
	for _, path := range []string{"/config/roles/designer", "/config/roles/ceo",
		"/config/roles/nosuch", "/config/units/design", "/config/units/nosuch"} {
		res := doAs(t, s, colleague, http.MethodGet, path, "", nil)
		if res.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403: %s", path, res.Code, res.Body.String())
			continue
		}
		if first == "" {
			first = res.Body.String()
		} else if res.Body.String() != first {
			t.Errorf("GET %s answered %s, unlike %s", path, res.Body.String(), first)
		}
	}
	admin := iam.Principal{ID: uuid.New(), Login: "ops.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantConfigRead},
		ReauthAt: time.Now().Add(time.Hour)}
	if res := doAs(t, s, admin, http.MethodGet, "/config/roles/nosuch", "", nil); res.Code != http.StatusNotFound {
		t.Errorf("config:read reading a missing seat = %d, want 404: %s",
			res.Code, res.Body.String())
	}
}

// A WRITE IS DECIDED BEFORE ITS ID IS LOOKED UP, as a read is.
//
// The Platform lead writing a seat or a unit outside their subtree — in
// another team, at the root, the team above their own — or one the company
// does not have is refused in the same bytes whatever the body: decided where
// the id sits before anything is built, a missing id at the root. Built
// first, a body this kind cannot read answered 400 naming where the seat sits
// in the document, a body naming another handle 400 naming both, a well-formed
// edit admission's 403 naming the seat's unit, and a missing id 404. The
// controls are the company's grant, which is told the id is not there, and an
// address inside their subtree, whose body is read and refused as unreadable.
//
// Mutation: decide the address after the draft is built, and every body to an
// existing id answers apart from the missing one.
func TestAWriteIsDecidedBeforeItsIDIsLookedUp(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	headers := map[string]string{"X-Summary": "probe"}
	bodies := map[string][]string{
		configapi.EntityRoles: {`{"bogus": true}`, `{"name": "Renamed", "goal": "grow"}`,
			`{"name": "Renamed", "handle": "other"}`},
		configapi.EntityUnits: {`{"bogus": true}`, `{"name": "Renamed", "purpose": "grow"}`,
			`{"name": "Renamed", "id": "other"}`},
	}
	ids := map[string][]string{
		configapi.EntityRoles: {"designer", "ceo", "cto", "nosuch"},
		configapi.EntityUnits: {"design", "engineering", "data", "nosuch"},
	}
	var first string
	for kind, list := range ids {
		for _, id := range list {
			for _, body := range bodies[kind] {
				path := "/config/" + kind + "/" + id
				res := doAs(t, s, platformLead(), http.MethodPut, path, body, headers)
				if res.Code != http.StatusForbidden {
					t.Errorf("PUT %s %s = %d, want 403: %s", path, body, res.Code,
						res.Body.String())
					continue
				}
				if first == "" {
					first = res.Body.String()
				} else if res.Body.String() != first {
					t.Errorf("PUT %s %s answered %s, unlike %s", path, body,
						res.Body.String(), first)
				}
			}
		}
	}
	if res := doAs(t, s, platformLead(), http.MethodPut, "/config/roles/sre",
		`{"bogus": true}`, headers); res.Code != http.StatusBadRequest {
		t.Errorf("PUT of a seat in their own team with an unreadable body = %d, "+
			"want the body read and refused: %s", res.Code, res.Body.String())
	}
	admin := iam.Principal{ID: uuid.New(), Login: "ops.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantConfigWrite},
		ReauthAt: time.Now().Add(time.Hour)}
	if res := doAs(t, s, admin, http.MethodPut, "/config/roles/nosuch",
		`{"name": "Nosuch", "llm": "zulu"}`, headers); res.Code != http.StatusNotFound {
		t.Errorf("config:write writing a missing seat = %d, want 404: %s",
			res.Code, res.Body.String())
	}
}

// A LEAD'S WRITE STILL ASKS FOR A RECENT PROOF, as every write of the company
// document does.
func TestALeadsWriteAsksForARecentProof(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	stale := platformLead()
	stale.ReauthAt = time.Now().Add(-time.Minute)
	res := putEntityAs(t, s, stale, configapi.EntityRoles, "sre",
		func(e map[string]any) { e["goal"] = "keep it up" }, "")
	if res.Code != http.StatusForbidden ||
		decode(t, res)["error"] != string(httpjson.CodeStepUpRequired) {
		t.Errorf("a stale lead's write = %d %s, want step_up_required",
			res.Code, res.Body.String())
	}
}
