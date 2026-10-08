package configapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
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
	return withoutSeats(t, heldDoc, name)
}

// withoutSeats is doc with each named seat's three lines taken out.
func withoutSeats(t *testing.T, doc string, names ...string) string {
	t.Helper()
	for _, name := range names {
		lines := strings.Split(doc, "\n")
		i := slices.IndexFunc(lines, func(line string) bool {
			return strings.TrimSpace(line) == "- name: "+name
		})
		if i < 0 {
			t.Fatalf("the document has no seat named %s", name)
		}
		doc = strings.Join(append(lines[:i:i], lines[i+3:]...), "\n")
	}
	return doc
}

// heldSurface is a surface over heldDoc whose directory binds Jane (active)
// and Ana (suspended), and nobody to Sam's seat.
func heldSurface(t *testing.T) (*surface, string) {
	t.Helper()
	s := newSurface(t)
	s.held = map[string]configapi.SeatHolder{
		"jane": {Person: "p-jane", Kind: iam.KindPerson, Login: "jane.doe",
			Stage: iam.StageActive},
		"ana": {Person: "p-ana", Kind: iam.KindPerson, Login: "ana.ruiz",
			Stage: iam.StageSuspended},
	}
	return s, s.seed(t, heldDoc)
}

// assertSeatHeld checks a refusal is the seat-holder one, names the seat and
// the login of the principal holding it, and left the active revision where it
// was.
func assertSeatHeld(t *testing.T, s *surface, before string, code int, body, seat, login string) {
	t.Helper()
	if holder := heldEntry(t, s, before, code, body, seat); holder["login"] != login {
		t.Errorf("the refusal's held %s = %v, want it held by %s", seat, holder,
			login)
	}
}

// heldEntry checks a refusal is the seat-holder one and left the active
// revision where it was, and answers what it says holds seat.
func heldEntry(t *testing.T, s *surface, before string, code int, body,
	seat string) map[string]any {

	t.Helper()
	if code != http.StatusConflict || !strings.Contains(body, `"seat_held"`) {
		t.Fatalf("got %d, want 409 seat_held: %s", code, body)
	}
	var refusal map[string]any
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatal(err)
	}
	if active := s.activeID(t); active != before {
		t.Errorf("the active revision moved from %s to %s under a refused write",
			before, active)
	}
	held, _ := refusal["held"].(map[string]any)
	holder, ok := held[seat].(map[string]any)
	if !ok {
		t.Fatalf("the refusal's held = %v names nothing holding %s", held, seat)
	}
	return holder
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
// nothing, refused on every request and named only once somebody asked
// `/iam/check`. A SUSPENDED holder still holds their seat: suspending somebody
// is not giving their seat away. A SERVICE ACCOUNT bound to one holds it too,
// and the refusal says it is one — the remedy differs: a person is moved or
// removed, only a service account is unbound — which the hint names.
//
// Mutation: drop the check from prepare, and every arm but the control lands;
// drop the kind from the holder and the service account reads as a person.
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

	t.Run("a service account bound to it", func(t *testing.T) {
		t.Parallel()
		s, before := heldSurface(t)
		s.held["sam"] = configapi.SeatHolder{Person: "p-ci", Kind: iam.KindMachine,
			Login: "ci:release", Stage: iam.StageActive}
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"), summaryHeader)
		holder := heldEntry(t, s, before, res.Code, res.Body.String(), "sam")
		if holder["kind"] != string(iam.KindMachine) || holder["login"] != "ci:release" {
			t.Errorf("the refusal names %v, want the service account ci:release", holder)
		}
		body := res.Body.String()
		if !strings.Contains(body, "service account ci:release") ||
			!strings.Contains(body, "unbind a service account") {
			t.Errorf("the refusal's sentence and hint do not say a service "+
				"account holds it, and how to free it: %s", body)
		}
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

// claimsSurface is a surface over heldDoc whose directory is the PRODUCTION
// fold ([configapi.ClaimHolders]) over claims — one snapshot of the bindings
// and the open invitations, as a node reads them — recording every instant it
// was asked at.
func claimsSurface(t *testing.T, claims iamdomain.SeatClaims) (*surface, string,
	*[]time.Time) {

	t.Helper()
	var at []time.Time
	s := newSurfaceWith(t, func(o *configapi.Options) {
		o.Holders = configapi.ClaimHolders(func(_ context.Context, now time.Time) (
			iamdomain.SeatClaims, error) {

			at = append(at, now)
			return claims, nil
		})
	})
	return s, s.seed(t, heldDoc), &at
}

// samInvited is one open invitation onto Sam's seat, its address sealed as the
// directory holds it.
var samInvited = iamdomain.SeatInvitation{
	Invitation: "018f3a9c-4d2e-7000-8000-00000000a5a1", Seat: "sam",
	Sealed: "sealed-address-of-the-invitee", InvitedBy: "jane",
	CreatedAt: pinned.Add(-time.Hour), ExpiresAt: pinned.Add(6 * 24 * time.Hour),
}

// AN INVITED SEAT CANNOT BE TAKEN OUT OF THE COMPANY (ADR-0026).
//
// A person holds a human seat for as long as they exist, so every invitation
// names the seat its redemption binds and HOLDS it, as it holds its address.
// A write removing that seat — or turning it into an agent's — while the link
// is outstanding would leave a link whose redemption is refused with nothing to
// tell its holder why, so it is refused `409 seat_held` naming the invitation
// by its id, with when it lapses and frees the seat on its own — and NEVER its
// address: a `/config` writer may be a unit's lead holding no grant over the
// identity directory.
//
// DRIVEN THROUGH THE PRODUCTION FOLD over one snapshot of the claims, read at
// this surface's own clock. THE CONTROLS: a snapshot holding no open
// invitation — one redeemed, cancelled or lapsed is not in it, the directory
// judging "open" at the instant it is asked — takes nothing; and where a
// person and an invitation left by an older build both claim a seat, the
// PERSON is named, whose seat the write takes.
//
// AND THE FOLD ANSWERS ABOUT THE SEATS THE WRITE TAKES AWAY: two rows on one
// seat are the unknown arm for that seat, and a residue on a seat the write
// keeps refuses nothing.
//
// Mutation: drop the invitation arm from the fold and the removal lands; ask
// at any clock but the surface's and the instant is wrong; fold the whole
// snapshot rather than the seats asked and the residue on Jane's seat refuses
// the removal of Sam's.
func TestAnInvitedSeatCannotBeTakenOutOfTheCompany(t *testing.T) {
	t.Parallel()
	invited := iamdomain.SeatClaims{Invitations: []iamdomain.SeatInvitation{samInvited}}

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"a whole document removing it", http.MethodPut, "/config", ""},
		{"turning it into an agent's", http.MethodPut, "/config/roles/sam",
			`{"name":"Sam Lee","handle":"sam","kind":"agent","llm":"zulu"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, before, at := claimsSurface(t, invited)
			body := tc.body
			if body == "" {
				body = withoutSeat(t, "Sam Lee")
			}
			res := s.do(t, tc.method, tc.path, body, summaryHeader)
			holder := heldEntry(t, s, before, res.Code, res.Body.String(), "sam")
			want := map[string]any{"invitation": samInvited.Invitation,
				"expires_at": samInvited.ExpiresAt.Format(time.RFC3339)}
			if !reflect.DeepEqual(holder, want) {
				t.Errorf("the refusal names %v, want the invitation alone: %v",
					holder, want)
			}
			for _, secret := range []string{samInvited.Sealed, "@"} {
				if strings.Contains(res.Body.String(), secret) {
					t.Errorf("the refusal carries the invitation's address "+
						"(%q): %s", secret, res.Body)
				}
			}
			if !strings.Contains(res.Body.String(), "open invitation "+
				samInvited.Invitation) || !strings.Contains(res.Body.String(),
				"DELETE /iam/invitations/{id}") {
				t.Errorf("the refusal does not say an invitation holds the seat "+
					"and how to cancel it: %s", res.Body)
			}
			if len(*at) != 1 || !(*at)[0].Equal(pinned) {
				t.Errorf("the directory was asked at %v, want once at the "+
					"surface's own clock %v", *at, pinned)
			}
		})
	}

	t.Run("the control: no open invitation", func(t *testing.T) {
		t.Parallel()
		s, before, _ := claimsSurface(t, iamdomain.SeatClaims{})
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"), summaryHeader)
		if res.Code != http.StatusCreated || s.activeID(t) == before {
			t.Fatalf("removing a seat nothing holds = %d, want 201: %s",
				res.Code, res.Body)
		}
	})

	t.Run("the control: a person and an older invitation name the person",
		func(t *testing.T) {
			t.Parallel()
			s, before, _ := claimsSurface(t, iamdomain.SeatClaims{
				Bindings: []iamdomain.SeatBinding{{Person: "p-sam", Login: "sam.lee",
					Kind: iam.KindPerson, Seat: "sam", Stage: iam.StageActive}},
				Invitations: []iamdomain.SeatInvitation{samInvited},
			})
			res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"),
				summaryHeader)
			holder := heldEntry(t, s, before, res.Code, res.Body.String(), "sam")
			if holder["person"] != "p-sam" || holder["invitation"] != nil {
				t.Errorf("the refusal names %v, want the person who holds the "+
					"seat and not the invitation beside them", holder)
			}
		})

	t.Run("two rows on one seat are unknown", func(t *testing.T) {
		t.Parallel()
		s, before, _ := claimsSurface(t, iamdomain.SeatClaims{
			Bindings: []iamdomain.SeatBinding{
				{Person: "p-sam", Login: "sam.lee", Kind: iam.KindPerson, Seat: "sam"},
				{Person: "p-sid", Login: "sid.lee", Kind: iam.KindPerson, Seat: "sam"},
			},
		})
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"), summaryHeader)
		if res.Code != http.StatusServiceUnavailable ||
			!strings.Contains(res.Body.String(), "identity_unavailable") {
			t.Errorf("a seat this node cannot say the holder of = %d, want 503 "+
				"identity_unavailable: %s", res.Code, res.Body)
		}
		if s.activeID(t) != before {
			t.Error("a removal nobody could check landed")
		}
	})

	// THE UNKNOWN ARM IS THE SEAT'S, never the company's: the same residue on
	// a seat the write leaves alone decides nothing about the one it removes.
	t.Run("two rows on a seat the write keeps are not asked about", func(t *testing.T) {
		t.Parallel()
		s, before, _ := claimsSurface(t, iamdomain.SeatClaims{
			Bindings: []iamdomain.SeatBinding{
				{Person: "p-jane", Login: "jane.doe", Kind: iam.KindPerson, Seat: "jane"},
				{Person: "p-jill", Login: "jill.doe", Kind: iam.KindPerson, Seat: "jane"},
			},
		})
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Sam Lee"), summaryHeader)
		if res.Code != http.StatusCreated || s.activeID(t) == before {
			t.Errorf("removing a seat nothing holds beside a residue on another "+
				"= %d, want 201: %s", res.Code, res.Body)
		}
	})
}

// THE DASHBOARD READS WHAT A SEAT-HELD REFUSAL SENDS.
//
// `SeatHeldHolder` (`contract/config.ts`) is the dashboard's type for one entry
// of a `409 seat_held` refusal's `held`, and the org builder words its remedy
// from it: a person is moved or removed, a service account unbound, an
// invitation cancelled — so a key renamed on this side alone, `kind` above all,
// tells an administrator to unbind a person, a remedy the directory refuses.
//
// HELD BOTH WAYS over a refusal naming one holder of each kind, through the
// production fold: every member the interface declares is sent by some entry
// and every REQUIRED one by every entry, and every key an entry sends is one
// the interface declares.
//
// Mutation: rename a tag on [configapi.SeatHolder] and the old name is declared
// and never sent while the new one is sent and never declared; send the
// invitation's address and it is a key nothing declares.
func TestTheDashboardReadsWhatASeatHeldRefusalSends(t *testing.T) {
	t.Parallel()
	s, before, _ := claimsSurface(t, iamdomain.SeatClaims{
		Bindings: []iamdomain.SeatBinding{
			{Person: "p-jane", Login: "jane.doe", Kind: iam.KindPerson, Seat: "jane",
				Stage: iam.StageActive},
			{Person: "p-ci", Login: "ci:release", Kind: iam.KindMachine, Seat: "ana",
				Stage: iam.StageActive},
		},
		Invitations: []iamdomain.SeatInvitation{samInvited},
	})
	res := s.do(t, http.MethodPut, "/config",
		withoutSeats(t, heldDoc, "Jane Doe", "Sam Lee", "Ana Ruiz"), summaryHeader)
	var entries []map[string]any
	for _, seat := range []string{"jane", "ana", "sam"} {
		entries = append(entries, heldEntry(t, s, before, res.Code,
			res.Body.String(), seat))
	}

	members, err := clientsource.Interface(clientsource.Tree(t), "SeatHeldHolder")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the dashboard's "+
			"declaration, and skipping would certify nothing", err)
	}
	declared := map[string]bool{} // member → required
	for _, m := range members {
		declared[m.Name] = !m.Optional
	}
	if len(declared) < 6 {
		t.Fatalf("SeatHeldHolder declares %v, so this gate compares less than "+
			"a refusal carries", members)
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		sentBy := 0
		for _, entry := range entries {
			if _, ok := entry[name]; ok {
				sentBy++
			}
		}
		switch {
		case sentBy == 0:
			t.Errorf("SeatHeldHolder declares %q and no entry of the refusal "+
				"sends it — the org builder reads it as undefined", name)
		case declared[name] && sentBy != len(entries):
			t.Errorf("SeatHeldHolder declares %q REQUIRED and %d of %d entries "+
				"send it", name, sentBy, len(entries))
		}
	}
	for _, entry := range entries {
		for _, key := range slices.Sorted(maps.Keys(entry)) {
			if _, ok := declared[key]; !ok {
				t.Errorf("a seat_held entry sends %q and SeatHeldHolder does not "+
					"declare it, so the org builder cannot read it", key)
			}
		}
	}
}
