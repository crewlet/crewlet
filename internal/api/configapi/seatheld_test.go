package configapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// heldDoc is a company with three people's seats — two at the root, one in a
// unit — beside its agents.
const heldDoc = `
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
  - name: Jane Doe
    handle: jane
    kind: human
  - name: Sam Lee
    handle: sam
    kind: human
units:
  - name: Engineering
    id: engineering
    lead: cto
    roles:
      - name: CTO
        handle: cto
        llm: zulu
      - name: Ana Ruiz
        handle: ana
        kind: human
`

// withoutSeat is heldDoc with one seat's three lines taken out.
func withoutSeat(t *testing.T, name string) string {
	t.Helper()
	lines := strings.Split(heldDoc, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "- name: "+name {
			return strings.Join(append(lines[:i:i], lines[i+3:]...), "\n")
		}
	}
	t.Fatalf("heldDoc has no seat named %s", name)
	return ""
}

// heldSurface is a surface over heldDoc whose directory binds Jane (active)
// and Ana (suspended), and nobody to Sam's seat.
func heldSurface(t *testing.T) (*surface, string) {
	t.Helper()
	s := newSurface(t)
	s.held = map[string]configapi.SeatHolder{
		"jane": {Person: "p-jane", Login: "jane.doe", Stage: iam.StageActive},
		"ana":  {Person: "p-ana", Login: "ana.ruiz", Stage: iam.StageSuspended},
	}
	return s, s.seed(t, heldDoc)
}

// assertSeatHeld checks a refusal is the seat-holder one, names the seat and
// its holder, and left the active revision where it was.
func assertSeatHeld(t *testing.T, s *surface, before string, code int, body, seat, login string) {
	t.Helper()
	if code != http.StatusConflict || !strings.Contains(body, `"seat_held"`) {
		t.Fatalf("got %d, want 409 seat_held: %s", code, body)
	}
	var refusal map[string]any
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatal(err)
	}
	held, _ := refusal["held"].(map[string]any)
	holder, _ := held[seat].(map[string]any)
	if holder["login"] != login {
		t.Errorf("the refusal's held = %v, want %s held by %s", held, seat, login)
	}
	if active := s.activeID(t); active != before {
		t.Errorf("the active revision moved from %s to %s under a refused write",
			before, active)
	}
}

// activeID is the revision this surface's node is running.
func (s *surface) activeID(t *testing.T) string {
	t.Helper()
	revision, found, err := s.configs.Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active revision: %v (found=%v)", err, found)
	}
	return revision.ID
}

// REMOVING A SEAT SOMEBODY IS BOUND TO IS REFUSED NAMING THEM, on every write
// path — and a seat nobody holds is removed.
//
// A person's binding lives in the identity directory and their seat in the
// company document; a revision that removed the seat left them bound to
// nothing, refused on every request and found by an alarm a minute later. A
// SUSPENDED holder still holds their seat: suspending somebody is not giving
// their seat away.
//
// Mutation: drop the check from prepare, and every arm but the control lands.
func TestRemovingAHeldSeatIsRefusedNamingThePerson(t *testing.T) {
	t.Parallel()

	t.Run("a whole document", func(t *testing.T) {
		t.Parallel()
		s, before := heldSurface(t)
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Jane Doe"), summaryHeader)
		assertSeatHeld(t, s, before, res.Code, res.Body.String(), "jane", "jane.doe")
	})

	t.Run("a patch", func(t *testing.T) {
		t.Parallel()
		s, before := heldSurface(t)
		res := s.do(t, http.MethodPatch, "/config", `{"roles":[`+
			`{"name":"CEO","handle":"ceo","llm":"zulu"},`+
			`{"name":"Sam Lee","handle":"sam","kind":"human"}]}`, summaryHeader)
		assertSeatHeld(t, s, before, res.Code, res.Body.String(), "jane", "jane.doe")
	})

	t.Run("a revert", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t)
		earlier := s.seed(t, withoutSeat(t, "Jane Doe"))
		before := s.seed(t, heldDoc)
		s.held = map[string]configapi.SeatHolder{
			"jane": {Person: "p-jane", Login: "jane.doe", Stage: iam.StageActive},
		}
		res := s.do(t, http.MethodPost, "/config/revisions/"+earlier+"/revert", "", nil)
		assertSeatHeld(t, s, before, res.Code, res.Body.String(), "jane", "jane.doe")
	})

	t.Run("the unit a seat sits in", func(t *testing.T) {
		t.Parallel()
		s, before := heldSurface(t)
		unit := entityOf(t, s, configapi.EntityUnits, "engineering")
		unit["roles"] = []any{map[string]any{"name": "CTO", "handle": "cto", "llm": "zulu"}}
		body, err := json.Marshal(unit)
		if err != nil {
			t.Fatal(err)
		}
		res := s.do(t, http.MethodPut, "/config/units/engineering", string(body), summaryHeader)
		assertSeatHeld(t, s, before, res.Code, res.Body.String(), "ana", "ana.ruiz")
	})

	t.Run("the control: a seat nobody holds", func(t *testing.T) {
		t.Parallel()
		s, before := heldSurface(t)
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"), summaryHeader)
		if res.Code != http.StatusCreated {
			t.Fatalf("removing an unheld seat = %d, want 201: %s", res.Code, res.Body)
		}
		if s.activeID(t) == before {
			t.Error("the write landed and the active revision did not move")
		}
		if len(s.asked) != 1 || strings.Join(s.asked[0], ",") != "sam" {
			t.Errorf("the directory was asked about %v, want the one seat leaving", s.asked)
		}
	})
}

// TURNING A HELD PERSON'S SEAT INTO AN AGENT'S IS REFUSED TOO.
//
// The binding names a human seat; an agent seat is nothing a person can hold,
// so to the person bound to it this is a removal. The control is the same
// edit of a seat nobody holds.
func TestTurningAHeldHumanSeatIntoAnAgentIsRefused(t *testing.T) {
	t.Parallel()
	s, before := heldSurface(t)
	res := s.do(t, http.MethodPut, "/config/roles/jane",
		`{"name":"Jane Doe","handle":"jane","kind":"agent","llm":"zulu"}`, summaryHeader)
	assertSeatHeld(t, s, before, res.Code, res.Body.String(), "jane", "jane.doe")

	res = s.do(t, http.MethodPut, "/config/roles/sam",
		`{"name":"Sam Lee","handle":"sam","kind":"agent","llm":"zulu"}`, summaryHeader)
	if res.Code != http.StatusCreated {
		t.Errorf("turning an unheld seat into an agent = %d, want 201: %s",
			res.Code, res.Body)
	}
}

// A DIRECTORY THIS NODE CANNOT READ REFUSES A REMOVAL AND NOTHING ELSE.
//
// A removal may not proceed on evidence this node does not have: read as
// "nobody holds it", an outage is exactly the answer the write would act on.
// But a write that takes no person's seat away never asks, so an identity
// outage does not stop every edit of the company.
func TestAnUnreadableDirectoryRefusesARemovalAndNothingElse(t *testing.T) {
	t.Parallel()
	s, before := heldSurface(t)
	s.directoryDown = errors.New("the identity estate is not open")

	res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"), summaryHeader)
	if res.Code != http.StatusServiceUnavailable ||
		!strings.Contains(res.Body.String(), "identity_unavailable") ||
		res.Header().Get("Retry-After") == "" {
		t.Fatalf("a removal over an unreadable directory = %d (Retry-After %q), "+
			"want 503 identity_unavailable with a hint: %s", res.Code,
			res.Header().Get("Retry-After"), res.Body)
	}
	if s.activeID(t) != before {
		t.Error("a removal nobody could check landed")
	}

	asked := len(s.asked)
	ceo := entityOf(t, s, configapi.EntityRoles, "ceo")
	ceo["goal"] = "ship it"
	body, err := json.Marshal(ceo)
	if err != nil {
		t.Fatal(err)
	}
	res = s.do(t, http.MethodPut, "/config/roles/ceo", string(body), summaryHeader)
	if res.Code != http.StatusCreated {
		t.Errorf("an edit that removes nobody's seat = %d over an unreadable "+
			"directory, want 201: %s", res.Code, res.Body)
	}
	if len(s.asked) != asked {
		t.Errorf("an edit that removes nobody's seat asked the directory about %v",
			s.asked[asked:])
	}
}
