package configapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// fleetDoc is a company with two Opus sign-ins in one fleet, a Sonnet entry,
// a seat whose executor runs on the fleet and Ask Crewlet on default-fleet.
const fleetDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    claude-max: {model: opus}
    claude-team: {model: opus, effort: medium}
    sonnet: {model: sonnet}
  llm_fleets:
    claude-opus: {name: Claude Opus, members: [claude-max, claude-team]}
roles:
  - name: Engineer
    handle: engineer
    llm: fleet:claude-opus
    llm_review: sonnet
assistant:
  model: fleet:default-fleet
`

// stored is the active document's providers block as stored, as a tree.
func storedProviders(t *testing.T, s *surface) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(s.activeDocument(t)), &doc); err != nil {
		t.Fatal(err)
	}
	providers, _ := doc["providers"].(map[string]any)
	return providers
}

// default-fleet is read as every other fleet while synthesised, a write that
// leaves it as synthesised stores nothing, and GET /config serves it only once
// a write changed it.
func TestDefaultFleetIsWrittenOnlyOnceChanged(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, fleetDoc)

	res := s.do(t, http.MethodGet, "/config/llm-fleets/default-fleet", "", nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"name":"default-fleet"`) {
		t.Fatalf("GET default-fleet = %d %s", res.Code, res.Body)
	}
	tag := res.Header().Get("ETag")
	ids, err := s.service().Entities(t.Context(), "llm-fleets")
	if err != nil || !slices.Equal(ids, []string{"claude-opus", config.DefaultFleetKey}) {
		t.Fatalf("Entities(llm-fleets) = %v, %v", ids, err)
	}
	if fleets, _ := storedProviders(t, s)["llm_fleets"].(map[string]any); fleets[config.DefaultFleetKey] != nil {
		t.Fatal("the stored document holds default-fleet before any write")
	}
	if got := s.do(t, http.MethodGet, "/config", "", nil).Body.String(); strings.Contains(got, config.DefaultFleetKey+`":{`) {
		t.Fatalf("GET /config serves the synthesised default-fleet: %s", got)
	}

	put := func(body string) int {
		return s.do(t, http.MethodPut, "/config/llm-fleets/default-fleet", body,
			map[string]string{"If-Match": tag, "X-Summary": "edit default-fleet"}).Code
	}
	if code := put(`{"name": "default-fleet", "strategy": "balanced"}`); code != http.StatusOK {
		t.Fatalf("PUT of the synthesised default-fleet unchanged = %d, want 200 unchanged", code)
	}
	if fleets, _ := storedProviders(t, s)["llm_fleets"].(map[string]any); fleets[config.DefaultFleetKey] != nil {
		t.Fatal("an unchanged write stored default-fleet")
	}
	if code := put(`{"name": "default-fleet", "members": ["sonnet"]}`); code != http.StatusCreated {
		t.Fatalf("PUT of default-fleet with a member = %d, want 201", code)
	}
	fleets, _ := storedProviders(t, s)["llm_fleets"].(map[string]any)
	written, _ := fleets[config.DefaultFleetKey].(map[string]any)
	if members, _ := json.Marshal(written["members"]); string(members) != `["sonnet"]` {
		t.Fatalf("stored default-fleet = %v, want its member", written)
	}
	if fleets["claude-opus"] == nil {
		t.Fatal("writing default-fleet lost the other fleet")
	}
}

// default-fleet is never deleted, and keeps its name.
func TestDefaultFleetCannotBeDeletedOrRenamed(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, fleetDoc)
	res := s.do(t, http.MethodDelete, "/config/llm-fleets/default-fleet", `{"repoint": []}`,
		map[string]string{"X-Summary": "remove default-fleet"})
	body := decode(t, res)
	if res.Code != http.StatusConflict || body["error"] != "not_removable" ||
		body["hint"] != "The default fleet can't be deleted." {
		t.Fatalf("DELETE default-fleet = %d %v, want 409 not_removable", res.Code, body)
	}
	res = s.do(t, http.MethodPut, "/config/llm-fleets/default-fleet", `{"name": "Everyone"}`,
		map[string]string{"X-Summary": "rename default-fleet"})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(),
		`"said":"The default fleet can't be renamed."`) {
		t.Fatalf("renaming default-fleet = %d %s, want 400 in the dashboard's words", res.Code, res.Body)
	}
}

// A fleet is created with If-None-Match: *, replaced by PUT, and its key is
// the address: a body is validated with the whole company.
func TestAFleetIsCreatedAndReplacedByKey(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, fleetDoc)
	create := map[string]string{"If-None-Match": "*", "X-Summary": "create Claude Sonnet"}
	res := s.do(t, http.MethodPut, "/config/llm-fleets/claude-sonnet",
		`{"name": "Claude Sonnet", "strategy": "sequential", "members": ["sonnet"]}`, create)
	if res.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.Code, res.Body)
	}
	if res = s.do(t, http.MethodPut, "/config/llm-fleets/claude-sonnet", `{"name": "Other"}`, create); res.Code != http.StatusPreconditionFailed {
		t.Fatalf("create of a taken key = %d, want 412 entity_exists", res.Code)
	}
	res = s.do(t, http.MethodPut, "/config/llm-fleets/claude-opus",
		`{"name": "Claude Opus", "members": ["claude-max", "sonnet"]}`, map[string]string{"X-Summary": "add sonnet"})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(),
		`"said":"Runs Claude Sonnet; this fleet runs Claude Opus."`) {
		t.Fatalf("a member of another class = %d %s, want 400 naming the class", res.Code, res.Body)
	}
	if res = s.do(t, http.MethodPut, "/config/llm-fleets/nope", `{"name": "Nope"}`,
		map[string]string{"X-Summary": "edit"}); res.Code != http.StatusNotFound {
		t.Fatalf("PUT of a fleet nobody has = %d, want 404", res.Code)
	}
	after := activeCompany(t, s)
	if f, _ := after.Providers.Fleet("claude-sonnet"); f.EffectiveStrategy() != config.FleetSequential {
		t.Fatalf("created fleet = %+v", f)
	}
}

// A fleet's references are every seat job and helper set to it, and its
// removal answers each by identity in one revision.
func TestAFleetsRemovalRepointsEverySeatSetToIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, fleetDoc)
	res := s.do(t, http.MethodGet, "/config/llm-fleets/claude-opus/references", "", nil)
	refs := decode(t, res)["references"]
	if got, _ := json.Marshal(refs); res.Code != http.StatusOK ||
		string(got) != `[{"at":{"job":"executor","seat":"engineer"},"chain":["fleet:claude-opus"],"segments":["roles",0,"llm"]}]` {
		t.Fatalf("references = %d %s", res.Code, got)
	}
	del := func(body string) (int, map[string]any) {
		res := s.do(t, http.MethodDelete, "/config/llm-fleets/claude-opus", body,
			map[string]string{"X-Summary": "remove Claude Opus"})
		return res.Code, decode(t, res)
	}
	if code, body := del(""); code != http.StatusUnprocessableEntity || body["references"] == nil {
		t.Fatalf("removal leaving the seat unanswered = %d %v, want 422 naming it", code, body)
	}
	code, body := del(`{"repoint": [{"at": {"seat": "engineer", "job": "executor"}, "to": "fleet:default-fleet"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("removal = %d %v", code, body)
	}
	after := activeCompany(t, s)
	if _, ok := after.Providers.LLMFleets["claude-opus"]; ok {
		t.Fatal("the fleet is still there")
	}
	for role := range after.EachRole() {
		if !slices.Equal(role.LLM.Default, config.ProviderKeys{"fleet:default-fleet"}) {
			t.Fatalf("engineer's llm = %v, want the fleet it moved to", role.LLM.Default)
		}
	}
	if _, ok := after.Providers.LLM["claude-max"]; !ok {
		t.Fatal("removing a fleet removed its members' entries")
	}
}

// Removing an entry a fleet lists is refused naming the fleet, and a repoint
// at the fleet drops it from the members.
func TestRemovingAFleetMemberAnswersTheFleet(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, fleetDoc)
	code, body := removeModel(t, s, "claude-team", "?dry_run=true", "", nil)
	refs, _ := json.Marshal(body["references"])
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(refs), `"at":{"fleet":"claude-opus"}`) {
		t.Fatalf("removing a member = %d %s, want 422 naming the fleet", code, refs)
	}
	code, body = removeModel(t, s, "claude-team", "",
		`{"repoint": [{"at": {"fleet": "claude-opus"}, "to": null}]}`,
		map[string]string{"X-Summary": "remove claude-team"})
	if code != http.StatusCreated {
		t.Fatalf("removal = %d %v", code, body)
	}
	if f, _ := activeCompany(t, s).Providers.Fleet("claude-opus"); !slices.Equal(f.Members, []string{"claude-max"}) {
		t.Fatalf("members = %v, want claude-team dropped", f.Members)
	}
}
