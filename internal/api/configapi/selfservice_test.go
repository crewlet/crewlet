package configapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
)

// selfDoc is companyDoc with two people in it: Ana, bound to her key by its
// literal id, and Bo, bound through a `${VAR}` the node's own chain resolves.
const selfDoc = companyDoc + `  - name: Ana Diaz
    handle: ana
    kind: human
    email: ana@example.com
    availability: CET business hours
    contact:
      slack_user_id: U0ANA
      crewlet_operator_id: ana-key
  - name: Bo Lang
    handle: bo
    kind: human
    availability: Afternoons
    contact:
      slack_user_id: U0BO
      crewlet_operator_id: ${BO_KEY}
`

// asMember is the principal a member key named id carries.
func asMember(id string) auth.Principal {
	return auth.Principal{ID: id, Role: config.RoleMember, Reach: auth.ReachMember}
}

// doAs is [surface.do] as caller.
func (s *surface) doAs(t *testing.T, caller auth.Principal, method, path, body string,
	headers map[string]string) *httptest.ResponseRecorder {

	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), caller))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	s.mux.ServeHTTP(res, req)
	return res
}

// selfSurface is a surface holding selfDoc, whose chain resolves Bo's binding.
func selfSurface(t *testing.T, mutate func(*configapi.Options)) *surface {
	t.Helper()
	s := newSurfaceWith(t, func(o *configapi.Options) {
		o.Env = func(name string) (string, bool) {
			if name == "BO_KEY" {
				return "bo-key", true
			}
			return "", false
		}
		if mutate != nil {
			mutate(o)
		}
	})
	s.seed(t, selfDoc, nil)
	return s
}

// seat is one seat of the active revision.
func (s *surface) seat(t *testing.T, handle string) *config.Role {
	t.Helper()
	entity, err := s.svc.Entity(t.Context(), configapi.EntityRoles, handle)
	if err != nil {
		t.Fatalf("read %s: %v", handle, err)
	}
	role, ok := entity.(*config.Role)
	if !ok {
		t.Fatalf("roles/%s is a %T", handle, entity)
	}
	return role
}

var summarised = map[string]string{"X-Summary": "my availability"}

// A PERSON SETS THEIR OWN AVAILABILITY, and nothing else changes.
//
// The line a colleague reads in a roster before they wait on somebody is the
// one thing on the company document its subject knows best, and a member key
// could not write it at all: every route here was an admin's. A patch naming
// it alone, on the seat the key is linked to, is a revision like any other —
// stored, activated and attributed to the key that made it.
func TestAPersonSetsTheirOwnAvailability(t *testing.T) {
	t.Parallel()
	s := selfSurface(t, nil)
	before := s.seat(t, "ana")

	res := s.doAs(t, asMember("ana-key"), http.MethodPatch, "/config/roles/ana",
		`{"availability": "Mornings only; replies within a day"}`, summarised)
	if res.Code != http.StatusCreated {
		t.Fatalf("a person's own availability = %d, want 201: %s", res.Code, res.Body)
	}
	after := s.seat(t, "ana")
	if after.Availability != "Mornings only; replies within a day" {
		t.Errorf("availability = %q, want the line she wrote", after.Availability)
	}
	if after.Email != before.Email || after.Contact == nil ||
		after.Contact.CrewletOperatorID != "ana-key" || after.Contact.SlackUserID != "U0ANA" {
		t.Errorf("the write moved more than availability: %+v", after)
	}
	active, _, err := s.configs.Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.CreatedBy != "ana-key" {
		t.Errorf("the revision is attributed to %q, want the key that wrote it", active.CreatedBy)
	}

	// THROUGH A ${VAR} BINDING TOO, resolved by the node's own chain: Bo's
	// key is a sealed secret, and read from the process environment his
	// own seat would not be his.
	res = s.doAs(t, asMember("bo-key"), http.MethodPatch, "/config/roles/bo",
		`{"availability": "On leave until the 20th"}`, summarised)
	if res.Code != http.StatusCreated {
		t.Fatalf("a person bound through a ${VAR} = %d, want 201: %s", res.Code, res.Body)
	}
	if got := s.seat(t, "bo").Availability; got != "On leave until the 20th" {
		t.Errorf("Bo's availability = %q", got)
	}
}

// A PUT IS HELD TO THE SAME RULE, judged on what the write changes: the seat
// sent back whole with only its availability moved is admitted, and the same
// seat with anything else moved is not.
func TestAPersonsOwnPutIsJudgedOnWhatItChanges(t *testing.T) {
	t.Parallel()
	s := selfSurface(t, nil)
	seat := s.seat(t, "ana")
	seat.Availability = "Tuesdays and Thursdays"
	body, err := json.Marshal(seat)
	if err != nil {
		t.Fatal(err)
	}
	res := s.doAs(t, asMember("ana-key"), http.MethodPut, "/config/roles/ana", string(body), summarised)
	if res.Code != http.StatusCreated {
		t.Fatalf("her seat back with a new availability = %d, want 201: %s", res.Code, res.Body)
	}
	seat.Email = "ana@elsewhere.example.com"
	body, err = json.Marshal(seat)
	if err != nil {
		t.Fatal(err)
	}
	res = s.doAs(t, asMember("ana-key"), http.MethodPut, "/config/roles/ana", string(body), summarised)
	if res.Code != http.StatusForbidden {
		t.Fatalf("her seat back with a new email = %d, want 403: %s", res.Code, res.Body)
	}
}

// A MEMBER'S WRITE IS HELD TO THEIR OWN SEAT'S AVAILABILITY, and everything
// past it is refused `forbidden` with nothing stored.
//
// Contact ids, the email and the operator binding are how the engine
// attributes a person and reaches them: a key that could rewrite its own
// binding could bind itself to somebody else's seat.
func TestAPersonsOwnWriteIsHeldToTheirAvailability(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		caller auth.Principal
		path   string
		body   string
		// says is what the refusal must name, so each case is refused by
		// the rule it is about rather than by whichever fires first.
		says string
	}{
		"somebody else's seat": {asMember("ana-key"), "/config/roles/bo",
			`{"availability": "Never"}`, `linked to \"ana\", and this write is to \"bo\"`},
		"an agent's seat": {asMember("ana-key"), "/config/roles/ceo",
			`{"availability": "Always"}`, `this write is to \"ceo\"`},
		"her own email": {asMember("ana-key"), "/config/roles/ana",
			`{"email": "ana@elsewhere.example.com"}`, "more of"},
		"her own binding": {asMember("ana-key"), "/config/roles/ana",
			`{"contact": {"slack_user_id": "U0ANA", "crewlet_operator_id": "bo-key"}}`, "more of"},
		"her availability and her goal": {asMember("ana-key"), "/config/roles/ana",
			`{"availability": "Mornings", "goal": "Run the company"}`, "more of"},
		"a key no seat links": {asMember("stranger-key"), "/config/roles/ana",
			`{"availability": "Mornings"}`, "no seat names the key"},
		// A COLLECTION THAT IS NOT A SEAT, under a key named like her own
		// handle, so it is the collection that refuses it rather than the
		// name.
		"another collection": {asMember("ana-key"), "/config/llm-providers/ana",
			`{"model": "claude-opus-5"}`, "changes nothing in llm-providers"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := selfSurface(t, nil)
			was, _, err := s.configs.Active(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			res := s.doAs(t, c.caller, http.MethodPatch, c.path, c.body, summarised)
			if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), `"forbidden"`) {
				t.Fatalf("PATCH %s = %d, want 403 forbidden: %s", c.path, res.Code, res.Body)
			}
			if !strings.Contains(res.Body.String(), c.says) {
				t.Errorf("the refusal does not say %q: %s", c.says, res.Body)
			}
			now, _, err := s.configs.Active(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if now.ID != was.ID {
				t.Errorf("a refused write activated revision %s", now.ID)
			}
		})
	}
}

// AN ADMIN IS HELD TO NONE OF IT: the rule is a member's, and an admin's write
// is the document's to judge.
func TestAnAdminsSeatWriteIsNotAPersonsOwn(t *testing.T) {
	t.Parallel()
	s := selfSurface(t, nil)
	res := s.doAs(t, asAdmin("ops"), http.MethodPatch, "/config/roles/ana",
		`{"email": "ana@elsewhere.example.com", "availability": "Mornings"}`, summarised)
	if res.Code != http.StatusCreated {
		t.Fatalf("an admin's patch of somebody's seat = %d, want 201: %s", res.Code, res.Body)
	}
	if got := s.seat(t, "ana").Email; got != "ana@elsewhere.example.com" {
		t.Errorf("email = %q after an admin's patch", got)
	}
}

// A MANAGED DOCUMENT REFUSES A PERSON'S OWN WRITE TOO, as config_managed: the
// system that writes the company replaces the active revision at its next
// reconcile, so an availability set here would last until then and nothing
// would say so.
func TestAManagedCompanyRefusesAPersonsOwnWrite(t *testing.T) {
	t.Parallel()
	s := selfSurface(t, func(o *configapi.Options) {
		o.Bootstrap.API.Auth.CompanyWriters = []string{"gitops"}
	})
	res := s.doAs(t, asMember("ana-key"), http.MethodPatch, "/config/roles/ana",
		`{"availability": "Mornings"}`, summarised)
	if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "config_managed") {
		t.Fatalf("a person's own write on a managed company = %d, want 403 config_managed: %s",
			res.Code, res.Body)
	}
}

// AN ENTITY PATCH CHANGES WHAT IT NAMES AND NOTHING ELSE, and is refused as a
// PUT body would be: a misspelt key, a rename, an empty patch and a create.
func TestAnEntityPatchChangesOnlyWhatItNames(t *testing.T) {
	t.Parallel()
	s := selfSurface(t, nil)
	res := s.do(t, http.MethodPatch, "/config/roles/cto", `{"goal": "Ship the platform"}`, summarised)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH roles/cto = %d, want 201: %s", res.Code, res.Body)
	}
	cto := s.seat(t, "cto")
	if cto.Goal != "Ship the platform" {
		t.Errorf("goal = %q, want the patched one", cto.Goal)
	}
	if cto.Name != "CTO" || cto.LLM.IsZero() {
		t.Errorf("the patch moved what it did not name: %+v", cto)
	}

	for name, c := range map[string]struct {
		body    string
		headers map[string]string
		want    string
	}{
		"a misspelt key":    {`{"gaol": "x"}`, summarised, "gaol"},
		"a rename":          {`{"handle": "chief"}`, summarised, "identity_mismatch"},
		"an empty patch":    {`null`, summarised, "invalid_patch"},
		"a create":          {`{"goal": "x"}`, map[string]string{"X-Summary": "add", "If-None-Match": "*"}, "patch_creates_nothing"},
		"no summary at all": {`{"goal": "x"}`, nil, "summary_required"},
	} {
		res := s.do(t, http.MethodPatch, "/config/roles/cto", c.body, c.headers)
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), c.want) {
			t.Errorf("%s: PATCH = %d, want 400 naming %q: %s", name, res.Code, c.want, res.Body)
		}
	}
}
