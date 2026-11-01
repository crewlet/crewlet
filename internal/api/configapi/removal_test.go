package configapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
)

// removalDoc is a company whose models are named every way a removal has to
// answer: pm names none (its jobs run on `default`), qa names codex-review for
// its reviewing flat and sonnet as its executor, backend names codex-review
// inside its `llm` mapping, and the assistant chats on aux with deep work on
// sonnet.
const removalDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm_order: [default, sonnet, codex-review, aux]
  llm:
    default:
      model: claude-opus-5-5
    sonnet:
      model: sonnet
    codex-review:
      harness: codex
      model: gpt-6.1-sol
    aux:
      model: haiku
      overrides: {web_tools: denied}
roles:
  - name: PM
    handle: pm
  - name: QA
    handle: qa
    llm: sonnet
    llm_review: [codex-review, aux]
  - name: Backend
    handle: backend
    llm:
      default: sonnet
      subagent: codex-review
assistant:
  enabled: true
  llm: [aux]
  deep_work:
    llm: sonnet
`

func removeModel(t *testing.T, s *surface, key, query, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	res := s.do(t, http.MethodDelete, "/config/llm-providers/"+key+query, body, headers)
	return res.Code, decode(t, res)
}

// activeCompany is the fleet's company, decoded.
func activeCompany(t *testing.T, s *surface) *config.Company {
	t.Helper()
	c, err := config.DecodeCompany([]byte(s.activeDocument(t)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A REMOVAL WITH A DEPENDANT UNANSWERED IS REFUSED NAMING EVERY ONE.
//
// Removed alone, an entry leaves every field that names it naming nothing,
// which validation refuses with no way for a page to draw what has to move.
// The refusal is the list: each reference by seat and job, located at the
// field it is written in, with the chain it writes.
func TestARemovalNamesEveryDependantItLeavesUnanswered(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)

	code, body := removeModel(t, s, "codex-review", "?dry_run=true", "", nil)
	if code != http.StatusUnprocessableEntity || body["error"] != "validation_error" {
		t.Fatalf("dry run with no repoint = %d %v, want 422 validation_error", code, body)
	}
	refs, _ := body["references"].([]any)
	if len(refs) != 2 {
		t.Fatalf("references = %v, want qa's reviewing and backend's delegated tasks", refs)
	}
	first, _ := refs[0].(map[string]any)
	if at, _ := first["at"].(map[string]any); at["seat"] != "qa" || at["job"] != "review" {
		t.Errorf("first reference = %v, want qa's review by identity", first)
	}
	if segs, _ := json.Marshal(first["segments"]); string(segs) != `["roles",1,"llm_review"]` {
		t.Errorf("segments = %s, want the written field", segs)
	}
	if chain, _ := json.Marshal(first["chain"]); string(chain) != `["codex-review","aux"]` {
		t.Errorf("chain = %s, want the whole written chain", chain)
	}
	problems, _ := body["problems"].([]any)
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want one per reference", problems)
	}
	p, _ := problems[0].(map[string]any)
	if p["kind"] != "conflict" || p["seat"] != "qa" ||
		!strings.Contains(p["message"].(string), "qa's reviewing work still names codex-review") {
		t.Errorf("problem = %v", p)
	}
	if segs, _ := json.Marshal(p["segments"]); string(segs) != `["roles",1,"llm_review"]` {
		t.Errorf("problem segments = %s", segs)
	}
	// NOTHING MOVED.
	if _, ok := activeCompany(t, s).Providers.LLM["codex-review"]; !ok {
		t.Error("a refused removal removed the entry")
	}
}

// A REMOVAL IS DECIDED ON THE ENTRY'S OWN TAG, as an edit of it is: the one
// its GET answers. The document's tag names the whole revision and is
// refused, so a client that sends it (as the dashboard once did, for every
// model) learns at once rather than removing on a guess.
func TestARemovalIsDecidedOnTheEntrysOwnTag(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	document := s.do(t, http.MethodGet, "/config", "", nil).Header().Get("ETag")
	entry := s.do(t, http.MethodGet, "/config/llm-providers/codex-review", "", nil).Header().Get("ETag")
	if document == "" || entry == "" || document == entry {
		t.Fatalf("tags: document %q, entry %q, want two different tags", document, entry)
	}
	code, body := removeModel(t, s, "codex-review", "?dry_run=true", "",
		map[string]string{"If-Match": document})
	if code != http.StatusPreconditionFailed || body["error"] != "version_moved" {
		t.Errorf("removal on the document's tag = %d %v, want 412 version_moved", code, body)
	}
	code, body = removeModel(t, s, "codex-review", "?dry_run=true", "",
		map[string]string{"If-Match": entry})
	if code != http.StatusUnprocessableEntity || body["error"] != "validation_error" {
		t.Errorf("removal on the entry's tag = %d %v, want 422 naming its dependants", code, body)
	}
}

// A REMOVAL REPOINTS BY IDENTITY, IN ONE REVISION.
//
// `to` replaces the entry in its chain where it stood, null removes it, and a
// chain left empty clears the field; the entry leaves the map and the order.
func TestARemovalRepointsEveryDependantInOneRevision(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	body := `{"repoint": [
		{"at": {"seat": "qa", "job": "review"}, "to": "sonnet"},
		{"at": {"seat": "backend", "job": "subagent"}, "to": null}]}`

	code, checked := removeModel(t, s, "codex-review", "?dry_run=true", body, nil)
	if code != http.StatusOK || checked["valid"] != true {
		t.Fatalf("dry run = %d %v", code, checked)
	}
	if before := activeCompany(t, s); before.Providers.LLM["codex-review"].Harness == "" {
		t.Fatal("a dry run removed the entry")
	}

	code, applied := removeModel(t, s, "codex-review", "", body,
		map[string]string{"X-Summary": "remove codex-review"})
	if code != http.StatusCreated || applied["revision_id"] == "" || applied["epoch"] == nil {
		t.Fatalf("removal = %d %v, want 201 with the revision and its epoch", code, applied)
	}
	after := activeCompany(t, s)
	if _, ok := after.Providers.LLM["codex-review"]; ok {
		t.Error("the entry is still there")
	}
	if slices.Contains(after.Providers.LLMOrder, "codex-review") {
		t.Errorf("llm_order = %v still names it", after.Providers.LLMOrder)
	}
	for role := range after.EachRole() {
		switch role.IdentityKey() {
		case "qa":
			if !slices.Equal(role.LLMReview, config.ProviderKeys{"sonnet", "aux"}) {
				t.Errorf("qa's review = %v, want sonnet where codex-review stood", role.LLMReview)
			}
		case "backend":
			if len(role.LLM.Subagent) != 0 || !slices.Equal(role.LLM.Default, config.ProviderKeys{"sonnet"}) {
				t.Errorf("backend's llm = %+v, want the subagent field cleared and the rest kept", role.LLM)
			}
		}
	}
}

// A REMOVAL THAT MOVES A JOB NOBODY WROTE SAYS SO.
//
// pm names no model, so every job of pm's runs on `default`; removing it
// moves them to the first entry in the order, and the dry run names each move
// as a `resolution_moved` warning rather than leaving it to be found later.
func TestARemovalSaysWhereAnUnwrittenJobMoves(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)

	code, body := removeModel(t, s, "default", "?dry_run=true", "", nil)
	if code != http.StatusOK {
		t.Fatalf("dry run = %d %v", code, body)
	}
	warnings, _ := body["warnings"].([]any)
	var moved []string
	for _, w := range warnings {
		row, _ := w.(map[string]any)
		if row["kind"] != configapi.WarningResolutionMoved {
			continue
		}
		moved = append(moved, row["seat"].(string)+"/"+row["ref"].(string)+":"+
			row["from"].(string)+">"+row["to"].(string))
		if row["introduced"] != true {
			t.Errorf("%v is not marked introduced", row)
		}
		if row["seat"] == "pm" && row["ref"] == "auxiliary" &&
			row["message"] != "pm's memory and summaries will run on sonnet" {
			t.Errorf("message = %q", row["message"])
		}
	}
	slices.Sort(moved)
	want := []string{"pm/auxiliary:default>sonnet", "pm/executor:default>sonnet",
		"pm/judge:default>sonnet", "pm/review:default>sonnet", "pm/subagent:default>sonnet"}
	if !slices.Equal(moved, want) {
		t.Errorf("moves = %v, want %v (qa and backend write their own)", moved, want)
	}
}

// THE ASSISTANT'S CHAT IS NEVER LEFT EMPTY BY ACCIDENT.
//
// While the assistant is on, a removal that leaves its chat with no model is
// refused at `assistant.llm` unless the write turns the assistant off itself,
// and its deep work left unset removes the block.
func TestARemovalTurnsTheAssistantOffOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	clear := `{"repoint": [{"at": {"seat": "qa", "job": "review"}, "to": null},
		{"at": {"assistant": "chat"}, "to": null}]}`

	code, body := removeModel(t, s, "aux", "?dry_run=true", clear, nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an emptied chat = %d %v, want 422", code, body)
	}
	problems, _ := body["problems"].([]any)
	p, _ := problems[0].(map[string]any)
	if segs, _ := json.Marshal(p["segments"]); string(segs) != `["assistant","llm"]` {
		t.Errorf("problem = %v, want it at assistant.llm", p)
	}

	off := strings.Replace(clear, `]}`, `], "assistant_off": true}`, 1)
	code, body = removeModel(t, s, "aux", "", off, map[string]string{"X-Summary": "remove aux"})
	if code != http.StatusCreated {
		t.Fatalf("with assistant_off = %d %v", code, body)
	}
	if activeCompany(t, s).Assistant.On() {
		t.Error("the assistant is still on")
	}

	code, body = removeModel(t, s, "sonnet", "?dry_run=true", `{"repoint": [
		{"at": {"seat": "qa", "job": "executor"}, "to": "default"},
		{"at": {"seat": "backend", "job": "executor"}, "to": "default"},
		{"at": {"assistant": "deep_work"}, "to": null}]}`, nil)
	if code != http.StatusOK {
		t.Errorf("removing deep work's model with its block = %d %v", code, body)
	}
}

// A REPOINT THAT ANSWERS NOTHING, OR MOVES A JOB WHERE IT CANNOT RUN, IS
// REFUSED AT ITS FIELD.
func TestARemovalRefusesARepointTheCompanyCannotRun(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)

	code, body := removeModel(t, s, "codex-review", "?dry_run=true", `{"repoint": [
		{"at": {"seat": "qa", "job": "review"}, "to": null},
		{"at": {"seat": "backend", "job": "subagent"}, "to": null},
		{"at": {"seat": "pm", "job": "executor"}, "to": "aux"}]}`, nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("a repoint answering nothing = %d %v", code, body)
	}
	if problems, _ := body["problems"].([]any); len(problems) != 1 {
		t.Errorf("problems = %v, want the one repoint answering nothing", problems)
	} else if segs, _ := json.Marshal(problems[0].(map[string]any)["segments"]); string(segs) !=
		`["repoint",2,"at"]` {
		t.Errorf("the repoint answering nothing is at %s, want the row in the body", segs)
	}

	// A FIELD MOVED ONTO THE ENTRY THIS WRITE REMOVES is refused at the field.
	code, body = removeModel(t, s, "codex-review", "?dry_run=true", `{"repoint": [
		{"at": {"seat": "qa", "job": "review"}, "to": "codex-review"},
		{"at": {"seat": "backend", "job": "subagent"}, "to": null}]}`, nil)
	problems, _ := body["problems"].([]any)
	if code != http.StatusUnprocessableEntity || len(problems) != 1 ||
		problems[0].(map[string]any)["kind"] != "invalid" ||
		!strings.Contains(problems[0].(map[string]any)["message"].(string), "the entry this write removes") {
		t.Errorf("a repoint onto the removed entry = %d %v, want 422 at the field", code, body)
	}

	// CODEX CANNOT RUN AN EXECUTOR: the whole document is validated.
	code, body = removeModel(t, s, "sonnet", "?dry_run=true", `{"repoint": [
		{"at": {"seat": "qa", "job": "executor"}, "to": "codex-review"},
		{"at": {"seat": "backend", "job": "executor"}, "to": "default"},
		{"at": {"assistant": "deep_work"}, "to": null}]}`, nil)
	if code != http.StatusUnprocessableEntity || body["error"] != "validation_error" {
		t.Fatalf("an executor moved to codex = %d %v, want 422 validation_error", code, body)
	}
	if problems, _ := body["problems"].([]any); len(problems) == 0 {
		t.Errorf("the refusal carries no field-mapped problem: %v", body)
	}

	for _, bad := range []string{`{"repoint": [{"at": {"seat": "qa"}, "to": "x"}]}`,
		`{"repoint": [], "extra": 1}`, `{"repoint": [{"at": {"assistant": "voice"}}]}`} {
		if code, body := removeModel(t, s, "codex-review", "?dry_run=true", bad, nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", bad, code, body)
		}
	}
}

// A REMOVAL IS HELD TO WHAT ITS CALLER READ, NEEDS A SUMMARY, AND 404s AN
// ENTRY THAT IS NOT THERE.
func TestARemovalIsAWriteLikeAnyOther(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	body := `{"repoint": [{"at": {"seat": "qa", "job": "review"}, "to": null},
		{"at": {"seat": "backend", "job": "subagent"}, "to": null}]}`

	// SAID AS NOTHING TO REMOVE: the entity routes' 404 tells a PUT to send
	// If-None-Match: * to add what it named, which no removal wants.
	if code, got := removeModel(t, s, "nope", "", "", nil); code != http.StatusNotFound ||
		!strings.Contains(got["hint"].(string), "nothing to remove") ||
		strings.Contains(got["hint"].(string), "If-None-Match") {
		t.Errorf("an absent entry = %d %v, want 404 saying there is nothing to remove", code, got)
	}
	if code, got := removeModel(t, s, "codex-review", "", body,
		map[string]string{"If-Match": `"llm-providers-0000"`}); code != http.StatusPreconditionFailed {
		t.Errorf("a stale If-Match = %d %v, want 412", code, got)
	}
	if code, got := removeModel(t, s, "codex-review", "", body, nil); code != http.StatusBadRequest ||
		got["error"] != "summary_required" {
		t.Errorf("no summary = %d %v, want 400 summary_required", code, got)
	}
	tag := s.do(t, http.MethodGet, "/config/llm-providers/codex-review", "", nil).Header().Get("ETag")
	if code, got := removeModel(t, s, "codex-review", "", body,
		map[string]string{"If-Match": tag, "X-Summary": "remove codex-review"}); code != http.StatusCreated {
		t.Errorf("with the entry's own tag = %d %v", code, got)
	}
}

// A REMOVAL TAKES ITS SUMMARY IN THE BODY, as every config write does.
//
// The assistant's proposed change carries its history line as `_summary` in
// the body, the one channel its request digest binds, and sends no headers of
// its own: a removal that read only X-Summary refused that key as an unknown
// field, so the assistant could never remove a model.
func TestARemovalTakesItsSummaryInTheBody(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	body := `{"_summary": "remove codex-review", "repoint": [
		{"at": {"seat": "qa", "job": "review"}, "to": null},
		{"at": {"seat": "backend", "job": "subagent"}, "to": null}]}`
	if code, got := removeModel(t, s, "codex-review", "?dry_run=true", body, nil); code != http.StatusOK ||
		got["valid"] != true {
		t.Fatalf("a dry run with _summary = %d %v", code, got)
	}
	code, applied := removeModel(t, s, "codex-review", "", body, nil)
	if code != http.StatusCreated {
		t.Fatalf("a removal summarised in its body = %d %v, want 201", code, applied)
	}
	active, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != applied["revision_id"] || active.Summary != "remove codex-review" {
		t.Errorf("the revision is %s summarised %q, want the body's summary", active.ID, active.Summary)
	}
}

// A REMOVAL THAT ANSWERS ONE REFERENCE TWICE IS REFUSED.
//
// Two repoints at one field would move it to whichever came last, a choice
// the caller never saw made.
func TestARemovalRefusesAFieldAnsweredTwice(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	body := `{"repoint": [
		{"at": {"seat": "qa", "job": "review"}, "to": "sonnet"},
		{"at": {"seat": "backend", "job": "subagent"}, "to": null},
		{"at": {"seat": "qa", "job": "review"}, "to": null}]}`
	code, got := removeModel(t, s, "codex-review", "", body,
		map[string]string{"X-Summary": "remove codex-review"})
	if code != http.StatusBadRequest || got["error"] != "invalid_body" ||
		!strings.Contains(got["detail"].(string), "repoint[2].at names the same field as repoint[0]") {
		t.Fatalf("a field answered twice = %d %v, want 400 invalid_body naming both", code, got)
	}
	if _, ok := activeCompany(t, s).Providers.LLM["codex-review"]; !ok {
		t.Error("a refused removal removed the entry")
	}
}

// A JOB THAT FOLLOWS ITS SEAT'S EXECUTOR MOVES WITH THE EXECUTOR'S REPOINT,
// AND NO WARNING SAYS IT MOVED ON ITS OWN.
//
// qa runs on sonnet, and every job qa names no model for runs on qa's
// executor chain: repointing the executor moves them, as the caller wrote.
// `resolution_moved` is for a job no field decided, which the company's
// fallback ran.
func TestARemovalDoesNotWarnAboutAJobThatFollowsItsExecutor(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, removalDoc)
	code, body := removeModel(t, s, "sonnet", "?dry_run=true", `{"repoint": [
		{"at": {"seat": "qa", "job": "executor"}, "to": "aux"},
		{"at": {"seat": "backend", "job": "executor"}, "to": "aux"},
		{"at": {"assistant": "deep_work"}, "to": null}]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("dry run = %d %v", code, body)
	}
	warnings, _ := body["warnings"].([]any)
	for _, w := range warnings {
		if row, _ := w.(map[string]any); row["kind"] == configapi.WarningResolutionMoved {
			t.Errorf("a job following its repointed executor is warned about: %v", row)
		}
	}
}

// REMOVING THE LAST MODEL SAYS THE JOB HAS NONE, never that it "runs on
// nothing".
func TestARemovalOfTheLastModelSaysTheJobHasNone(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    only:
      model: sonnet
roles:
  - name: PM
    handle: pm
`)
	code, body := removeModel(t, s, "only", "?dry_run=true", "", nil)
	if code != http.StatusOK {
		t.Fatalf("dry run = %d %v", code, body)
	}
	warnings, _ := body["warnings"].([]any)
	said := 0
	for _, w := range warnings {
		row, _ := w.(map[string]any)
		if row["kind"] != configapi.WarningResolutionMoved {
			continue
		}
		said++
		if row["to"] != "" || !strings.HasPrefix(row["message"].(string), "pm's ") ||
			!strings.HasSuffix(row["message"].(string), "will have no model to run on: the company configures none") {
			t.Errorf("the last model's removal says %v", row)
		}
	}
	if said == 0 {
		t.Errorf("warnings = %v, want each of pm's jobs said to have no model", warnings)
	}
}

// A REMOVAL IS A CONFIG WRITE, mounted as one: `config.write` (admin, a recent
// proof), checked by `?dry_run=true` and summarised. A removal mounted under
// a weaker verb would let any reader of the configuration delete a model and
// every field naming it.
func TestARemovalIsMountedAsAConfigWrite(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	mux := &policyRecorder{ServeMux: http.NewServeMux(), policies: map[string]authz.Policy{}}
	if err := s.service().Routes(mux); err != nil {
		t.Fatal(err)
	}
	policy, ok := mux.policies["DELETE /config/"+configapi.EntityLLMProviders+"/{id}"]
	if !ok {
		t.Fatal("the removal route is not mounted")
	}
	if policy.Action != authz.ActionConfigWrite || !policy.DryRun || !policy.Summary {
		t.Errorf("the removal is mounted with %+v, want config.write, checked and summarised", policy)
	}
}
