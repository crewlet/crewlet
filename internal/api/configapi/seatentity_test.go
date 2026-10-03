package configapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// orgDoc is a company whose seats sit at the root, in a unit and in a unit's
// child, and whose units are keyed apart from their names.
const orgDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Engineering
    id: engineering
    lead: cto
    roles:
      - name: CTO
        handle: cto
        llm: zulu
    children:
      - name: Platform
        id: platform-team
        roles:
          - name: Staff Engineer
            handle: staff-eng
            llm: zulu
`

// A SEAT EDIT GOES THROUGH THE ROLES ENTITY ROUTE, at any depth.
//
// A seat is part of the company document, and a merge patch over `roles`
// replaces the list whole — so editing one seat is its own route, addressed by
// the seat's handle wherever in the tree it sits. An operator editing "the
// staff engineer" does not think about which list it lives in.
//
// The control is the read before the write: the goal is not already there.
func TestASeatEditGoesThroughTheRolesEntityRoute(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, orgDoc)

	ids, err := s.service().Entities(t.Context(), configapi.EntityRoles)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "ceo,cto,staff-eng" {
		t.Fatalf("roles = %v, want the root seat and both nested ones", ids)
	}

	role := entityOf(t, s, configapi.EntityRoles, "staff-eng")
	if role["goal"] == "own the build" {
		t.Fatal("the control: the fixture already carries the goal")
	}
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
	// AND EVERY OTHER SEAT IS STILL THERE, which is what a merge patch over
	// the list would not have left.
	after, err := s.service().Entities(t.Context(), configapi.EntityRoles)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(after, ","); got != "ceo,cto,staff-eng" {
		t.Errorf("roles after the edit = %v", after)
	}
}

// A UNIT IS ADDRESSED BY ITS KEY, never its name.
//
// A unit's key — its `id` — is what a seat's `unit:`, a `manages:` entry and a
// `lead:` resolve it by, and the name is prose a founder edits. Addressed by
// its name, a unit renamed for display would move its address, and two units
// sharing a name (two "Platform" teams under two departments) could not both
// be reached.
func TestAUnitIsAddressedByItsKey(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, orgDoc)

	if res := s.do(t, http.MethodGet, "/config/units/platform-team", "", nil); res.Code != http.StatusOK {
		t.Fatalf("GET the unit at its key = %d: %s", res.Code, res.Body.String())
	}
	if res := s.do(t, http.MethodGet, "/config/units/Platform", "", nil); res.Code != http.StatusNotFound {
		t.Errorf("GET the unit at its name = %d, want 404: %s", res.Code, res.Body.String())
	}

	// A WRITE AT THE KEY THAT RENAMES THE UNIT FOR DISPLAY LANDS, and one
	// that changes the key is a rename this route refuses.
	unit := entityOf(t, s, configapi.EntityUnits, "platform-team")
	unit["name"] = "Platform Engineering"
	body, err := json.Marshal(unit)
	if err != nil {
		t.Fatal(err)
	}
	if res := s.do(t, http.MethodPut, "/config/units/platform-team", string(body),
		map[string]string{"X-Summary": "spell the team out"}); res.Code != http.StatusCreated {
		t.Fatalf("PUT a unit's display name = %d: %s", res.Code, res.Body.String())
	}
	if got := entityOf(t, s, configapi.EntityUnits, "platform-team")["name"]; got != "Platform Engineering" {
		t.Errorf("the unit's name = %v, want the edit", got)
	}
	unit["id"] = "platform"
	body, err = json.Marshal(unit)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config/units/platform-team", string(body),
		map[string]string{"X-Summary": "rekey the team"})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "identity_mismatch") {
		t.Errorf("PUT a unit under another key = %d, want 400 identity_mismatch: %s",
			res.Code, res.Body.String())
	}
}

// EDITING A SEAT'S NAME IS NOT A RENAME.
//
// Every stored seat carries its handle, and a body that leaves the handle out
// keeps the one the path names rather than deriving another from the new name
// — so the edit this route is most often sent, correcting a title, never moves
// a seat's identity.
//
// The control is a body that DECLARES another handle: that is a rename, and it
// is refused naming the handle to send back.
func TestEditingASeatsNameIsNotARename(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, orgDoc)

	res := s.do(t, http.MethodPut, "/config/roles/ceo",
		`{"name":"Chief Executive","llm":"zulu"}`,
		map[string]string{"X-Summary": "spell the CEO's title out"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT a seat's new name with no handle = %d, want 201: %s",
			res.Code, res.Body.String())
	}
	role := entityOf(t, s, configapi.EntityRoles, "ceo")
	if role["name"] != "Chief Executive" || role["handle"] != "ceo" {
		t.Errorf("the seat reads name %v handle %v, want the new name under ceo",
			role["name"], role["handle"])
	}

	res = s.do(t, http.MethodPut, "/config/roles/ceo",
		`{"name":"Chief Executive","handle":"chief","llm":"zulu"}`,
		map[string]string{"X-Summary": "rename the CEO's seat"})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "identity_mismatch") ||
		!strings.Contains(res.Body.String(), "ceo") {
		t.Errorf("PUT a seat under another handle = %d, want 400 identity_mismatch "+
			"naming ceo: %s", res.Code, res.Body.String())
	}
}
