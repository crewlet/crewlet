package configapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
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
      agent: codex
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
	if before := activeCompany(t, s); before.Providers.LLM["codex-review"].Agent == "" {
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

	if code, got := removeModel(t, s, "nope", "", "", nil); code != http.StatusNotFound {
		t.Errorf("an absent entry = %d %v, want 404", code, got)
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
