package workapi_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY ANSWER THIS SURFACE GIVES IS ITS OWN STATUS, because each sends a
// caller somewhere different: present a credential, ask for authority, wait,
// look for something else, read it again, fix the request.
func TestEachOutcomeIsItsOwnStatus(t *testing.T) {
	t.Parallel()
	create := map[string]any{"title": "rotate the key", "project": "ENG"}
	for _, c := range []struct {
		name   string
		chart  authz.Chart
		who    caller
		method string
		target string
		body   any
		setup  func(*rig)
		want   int
	}{
		{"nobody presented a credential", chart{}, anonymous,
			http.MethodPost, "/work/items", create, nil, http.StatusUnauthorized},
		{"the node could not tell who is asking", chart{}, unknown,
			http.MethodPost, "/work/items", create, nil, http.StatusServiceUnavailable},
		{"the table refused at the route", chart{},
			as(person("ana", iam.GrantStateRead)), http.MethodPost, "/work/items",
			create, nil, http.StatusForbidden},
		// A ROW-DECIDED VERB on a chart that could not answer: the route
		// admitted a reader and the tool's own ask could not decide.
		{"the chart could not answer", chart{err: errors.New("behind the chart log")},
			as(colleague("ana")), http.MethodDelete, "/work/items/ENG-1", nil, nil,
			http.StatusServiceUnavailable},
		{"the item does not exist", chart{}, as(admin("ana")),
			http.MethodPatch, "/work/items/ENG-404",
			map[string]any{"status": "done"}, nil, http.StatusNotFound},
		{"somebody moved it first", chart{}, as(admin("ana")),
			http.MethodPatch, "/work/items/ENG-1", map[string]any{"status": "done"},
			func(r *rig) { r.writes.err = tracker.ErrStaleVersion }, http.StatusConflict},
		{"the domain refused the request", chart{}, as(admin("ana")),
			http.MethodPost, "/work/items", map[string]any{"project": "ENG"}, nil,
			http.StatusUnprocessableEntity},
		{"applied", chart{}, as(admin("ana")), http.MethodPost, "/work/items",
			create, nil, http.StatusOK},
		{"pending", chart{}, as(admin("ana")), http.MethodPost, "/work/items",
			create, func(r *rig) {
				r.writes.result = statelog.Result{Outcome: statelog.OutcomePending,
					Position: statelog.Position{Stream: "S", Generation: 1, Seq: 9}}
			}, http.StatusAccepted},
		{"unknown", chart{}, as(admin("ana")), http.MethodPost, "/work/items",
			create, func(r *rig) {
				r.writes.result = statelog.Result{Outcome: statelog.OutcomeUnknown}
			}, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, c.chart)
			if c.setup != nil {
				c.setup(r)
			}
			got := r.do(c.who, c.method, c.target, c.body)
			if got.status != c.want {
				t.Errorf("answered %d, want %d: %v", got.status, c.want, got.body)
			}
			if got.status == http.StatusServiceUnavailable &&
				got.header.Get("Retry-After") == "" {
				t.Error("a 503 carried no Retry-After, which reads as a node " +
					"that is down for good")
			}
		})
	}
}

// A WRITE THIS NODE CANNOT ACCOUNT FOR HANDS BACK THE KEY THAT MAKES A RETRY
// SAFE, and the retry under it IS the same operation.
//
// The ledger recognises a second arrival by its operation id, and every write
// a request makes derives its id from the request's key. So the key the 503
// hands back, sent again as Idempotency-Key, must produce the same operation
// — and a request without one must not, or two presses of one button would
// collapse into one item.
func TestARetryUnderTheSameKeyIsTheSameOperation(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.writes.result = statelog.Result{Outcome: statelog.OutcomeUnknown}
	first := r.do(as(admin("ana")), http.MethodPost, "/work/items",
		map[string]any{"title": "rotate the key", "project": "ENG"})
	key, _ := first.body["op_id"].(string)
	if first.status != http.StatusServiceUnavailable || key == "" {
		t.Fatalf("an unknown write answered %d with op_id %q", first.status, key)
	}
	retry := r.do(as(admin("ana")), http.MethodPost, "/work/items",
		map[string]any{"title": "rotate the key", "project": "ENG"},
		workapi.IdempotencyHeader, key)
	if got, _ := retry.body["op_id"].(string); got != key {
		t.Errorf("the retry answered op_id %q, want the key it was sent %q", got, key)
	}
	if len(r.writes.opIDs) != 2 || r.writes.opIDs[0] != r.writes.opIDs[1] {
		t.Errorf("the retry was a different operation: %v", r.writes.opIDs)
	}
	if r.writes.created[0].ID != r.writes.created[1].ID {
		t.Errorf("the retry filed a different item: %s and %s",
			r.writes.created[0].ID, r.writes.created[1].ID)
	}
	// THE CONTROL: two requests with no key are two operations.
	r.do(as(admin("ana")), http.MethodPost, "/work/items",
		map[string]any{"title": "rotate the key", "project": "ENG"})
	if r.writes.opIDs[2] == r.writes.opIDs[0] {
		t.Error("a request with no key reused another's operation")
	}
}

// A PERSON'S WRITE IS RECORDED AS THEIR SEAT, as a human.
//
// Which is what lets the tracker leave them out of the wake their own change
// sends — see TestAPersonsWriteSuppressesTheirOwnWake in internal/agent/builtin
// for the routing half.
func TestAWriteIsAuthoredByThePrincipal(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.do(as(admin("ana")), http.MethodPatch, "/work/items/ENG-1",
		map[string]any{"status": "in_progress"})
	if len(r.writes.actors) == 0 {
		t.Fatal("nothing was written")
	}
	actor := r.writes.actors[len(r.writes.actors)-1]
	if actor.Handle != "ana" || actor.Kind != tracker.AuthorHuman {
		t.Errorf("the write is authored as %q (%s), want the seat as a human",
			actor.Handle, actor.Kind)
	}
}

// THE PATH NAMES THE OBJECT, and a body naming a different one is refused.
func TestABodyContradictingThePathIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	got := r.do(as(admin("ana")), http.MethodPatch, "/work/items/ENG-1",
		map[string]any{"item": "ENG-2", "status": "done"})
	if got.status != http.StatusBadRequest || len(r.writes.opIDs) != 0 {
		t.Errorf("a body naming another item answered %d and wrote %v",
			got.status, r.writes.opIDs)
	}
	// AND A FACET ROUTE TAKES ONLY ITS FACET.
	got = r.do(as(admin("ana")), http.MethodPost, "/work/items/ENG-1/depend",
		map[string]any{"status": "done"})
	if got.status != http.StatusBadRequest {
		t.Errorf("the depend route took a status: %d", got.status)
	}
}

// A BODY NOTHING HERE COULD ACCEPT IS NOT READ INTO MEMORY.
func TestAnOversizedBodyIsRefusedBeforeItIsRead(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	huge := `{"title":"` + strings.Repeat("x", workapi.MaxBodyBytes) + `"}`
	if got := r.do(as(admin("ana")), http.MethodPost, "/work/items", huge); got.status != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body answered %d", got.status)
	}
}

// ---- the person routes -------------------------------------------------- //

// YOUR OWN INBOX GOES THROUGH THE TOOL, SOMEBODY ELSE'S THROUGH THE SAME
// WRITER, AND ONLY AN ADMINISTRATOR REACHES THE SECOND.
//
// mark_inbox and set_pins take no handle — a model that could name whose
// inbox to mark could mark anybody's — so the tools are not widened. What the
// route adds is the administrator's path onto a departed person's record,
// decided on the record the path names.
func TestSomebodyElsesInboxIsTheAdministratorsAlone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		who    iam.Principal
		handle string
		want   int
		agent  bool
	}{
		{"your own", colleague("ana"), "ana", http.StatusOK, false},
		{"a colleague's", colleague("ana"), "bo", http.StatusForbidden, false},
		{"an administrator on somebody's", admin("ana"), "bo", http.StatusOK, false},
	} {
		for _, route := range []string{"inbox", "pins"} {
			t.Run(c.name+"/"+route, func(t *testing.T) {
				t.Parallel()
				r := newRig(t, chart{})
				got := r.do(as(c.who), http.MethodPut,
					"/work/people/"+c.handle+"/"+route, map[string]any{})
				if got.status != c.want {
					t.Fatalf("answered %d, want %d: %v", got.status, c.want, got.body)
				}
				if c.want != http.StatusOK {
					return
				}
				written := r.writes.inbox
				if route == "pins" {
					written = r.writes.pins
				}
				if len(written) != 1 || written[0] != c.handle {
					t.Errorf("wrote the record of %v, want %q", written, c.handle)
				}
				if a := r.writes.authz[0]; !a.Authorized || a.Agent != c.agent {
					t.Errorf("the writer was handed %+v", a)
				}
			})
		}
	}
}

// ---- the gestures no tool makes ---------------------------------------- //

// A CARD IS PLACED AMONG ITS OWN PROJECT'S CARDS, between the neighbours
// named.
func TestARankMovePlacesAnItemBetweenItsNeighbours(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	got := r.do(as(colleague("ana")), http.MethodPost, "/work/items/ENG-1/rank",
		map[string]any{"after": "ENG-2"})
	if got.status != http.StatusOK || len(r.writes.moved) != 1 ||
		r.writes.moved[0] != [2]tracker.Rank{"a5", ""} {
		t.Fatalf("answered %d and moved %v", got.status, r.writes.moved)
	}
	if got := r.do(as(colleague("ana")), http.MethodPost, "/work/items/ENG-1/rank",
		map[string]any{"before": "OPS-1"}); got.status != http.StatusUnprocessableEntity {
		t.Errorf("a card placed on another project's board answered %d", got.status)
	}
	if got := r.do(as(person("ana", iam.GrantStateRead)), http.MethodPost,
		"/work/items/ENG-1/rank", map[string]any{"after": "ENG-2"}); got.status != http.StatusForbidden {
		t.Errorf("a reader moved a card: %d", got.status)
	}
}

// A REMARK ON A WORK ITEM IS REWRITTEN BY ITS AUTHOR.
//
// Two questions, two answers: the table admits the author and an
// administrator, and the writer — which the fake stands in for — refuses
// everybody but the author. What this surface must do is ask the first with
// the STORED author, so a colleague who did not write it never reaches the
// writer at all.
func TestAWorkCommentIsEditedByItsAuthor(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.reader.addComment("t-1", tracker.Comment{ID: "c-1", Task: "t-1",
		Author: "ana", AuthorKind: tracker.AuthorHuman, Body: "first"})
	if got := r.do(as(colleague("bo")), http.MethodPatch,
		"/work/items/ENG-1/comments/c-1", map[string]any{"body": "second"}); got.status != http.StatusForbidden || len(r.writes.edited) != 0 {
		t.Fatalf("somebody else's edit answered %d and wrote %v",
			got.status, r.writes.edited)
	}
	if got := r.do(as(colleague("ana")), http.MethodPatch,
		"/work/items/ENG-1/comments/c-1", map[string]any{"body": "second"}); got.status != http.StatusOK {
		t.Fatalf("the author's edit answered %d: %v", got.status, got.body)
	}
	if len(r.writes.edited) != 1 || r.writes.edited[0] != "c-1|second" {
		t.Errorf("the writer was asked %v", r.writes.edited)
	}
	// AND THE WRITER'S OWN REFUSAL IS AN AUTHORITY ANSWER, not a domain one.
	r.writes.err = tracker.ErrNotAuthor
	if got := r.do(as(admin("root")), http.MethodPatch,
		"/work/items/ENG-1/comments/c-1", map[string]any{"body": "third"}); got.status != http.StatusForbidden {
		t.Errorf("an administrator rewriting a remark answered %d", got.status)
	}
}

// A PURGE IS CONFIRMED BY THE ITEM'S KEY, CHECKED, AND FILED UNDER ITS OWN
// PROJECT — and it is a person's gesture.
//
// The route this replaced echoed the confirmation back unread and took the
// project as a parameter: a confirmation naming another item confirmed
// nothing, and a purge filed under the wrong project blocked writes to a
// project it was not about.
func TestAPurgeIsConfirmedAgainstTheItemItNames(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	operator := admin("root")
	for _, c := range []struct {
		name   string
		target string
		want   int
	}{
		{"no confirmation", "/work/items/t-1/purge?reason=why", http.StatusBadRequest},
		{"no reason", "/work/items/t-1/purge?confirm=ENG-1", http.StatusBadRequest},
		{"another item's key", "/work/items/t-1/purge?confirm=ENG-2&reason=why",
			http.StatusUnprocessableEntity},
	} {
		if got := r.do(as(operator), http.MethodPost, c.target, nil); got.status != c.want {
			t.Errorf("%s: answered %d, want %d", c.name, got.status, c.want)
		}
	}
	if len(r.writes.purged) != 0 {
		t.Fatalf("a refused purge destroyed %v", r.writes.purged)
	}
	got := r.do(as(operator), http.MethodPost,
		"/work/items/t-1/purge?confirm=eng-1&reason=an+erasure+request", nil,
		workapi.IdempotencyHeader, "k-1")
	if got.status != http.StatusOK {
		t.Fatalf("the purge answered %d: %v", got.status, got.body)
	}
	if len(r.writes.purged) != 1 || r.writes.purged[0] != "t-1|ENG|an erasure request" {
		t.Errorf("the writer was asked %v", r.writes.purged)
	}
	if r.writes.opIDs[0] != "purge-t-1-k-1" {
		t.Errorf("the purge was published as %q — the caller's key is what a "+
			"retry reuses", r.writes.opIDs[0])
	}
	// AND AN AGENT MAY NOT, whatever it holds.
	agent := admin("sre")
	agent.Kind = iam.KindSeat
	if got := r.do(as(agent), http.MethodPost,
		"/work/items/t-1/purge?confirm=ENG-1&reason=why", nil); got.status != http.StatusForbidden {
		t.Errorf("a seat purged an item: %d", got.status)
	}
}

// THE FOUR DESTRUCTIVE PAGE VERBS ARE THE CONTAINER LEAD'S, decided on the
// page's own container.
func TestThePageTrashIsTheContainersLeads(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		method, target string
		did            string
	}{
		{http.MethodDelete, "/pages/p-1", "trash p-1"},
		{http.MethodPost, "/pages/p-1/restore", "restore p-1"},
	} {
		r := newRig(t, chart{})
		if got := r.do(as(colleague("ana")), c.method, c.target, nil); got.status != http.StatusForbidden {
			t.Errorf("%s %s by a colleague answered %d", c.method, c.target, got.status)
		}
		if got := r.do(as(colleague("cto")), c.method, c.target, nil); got.status != http.StatusOK {
			t.Errorf("%s %s by the container's lead answered %d: %v",
				c.method, c.target, got.status, got.body)
		}
		if !slices.Equal(r.kb.did, []string{c.did}) {
			t.Errorf("%s %s did %v", c.method, c.target, r.kb.did)
		}
	}
	// A RENAME IS THE LEAD'S TOO.
	r := newRig(t, chart{})
	if got := r.do(as(colleague("ana")), http.MethodPost, "/pages/p-1/rename",
		map[string]any{"title": "Runbook v2"}); got.status != http.StatusForbidden {
		t.Errorf("a colleague renamed a page: %d", got.status)
	}
	// AND A PURGE IS THE FLEET'S, confirmed by the page's title.
	if got := r.do(as(admin("root")), http.MethodPost,
		"/pages/p-1/purge?confirm=runbook&reason=why", nil); got.status != http.StatusOK ||
		!slices.Contains(r.kb.did, "purge p-1 why") {
		t.Errorf("the purge answered %d and did %v", got.status, r.kb.did)
	}
}

// A TOOL SKILL'S PAGE TAKES config:write ON EVERY ROUTE THAT CHANGES IT.
//
// The skills container's pages are injected into every seat's turn, so taking
// one out of circulation, putting it back, renaming it or destroying it
// changes the prompt the company runs under exactly as editing its body does.
// `save_page` asks for the grant through the tool; these four reach the store
// without one, so without asking here a caller refused a skill's body could
// still delete the skill. Asked on top of each verb's own rule: `root` holds
// fleet:operate and knowledge:write — every destructive rule's admin path —
// and is refused until it also holds config:write.
func TestEveryRouteThatChangesAToolSkillNeedsConfigWrite(t *testing.T) {
	t.Parallel()
	operator := person("root", iam.GrantStateRead, iam.GrantKnowledgeWrite,
		iam.GrantFleetOperate)
	for _, c := range []struct {
		method, target string
		body           any
		did            string
	}{
		{http.MethodDelete, "/pages/p-1", nil, "trash p-1"},
		{http.MethodPost, "/pages/p-1/restore", nil, "restore p-1"},
		{http.MethodPost, "/pages/p-1/rename", map[string]any{"title": "Runbook v2"},
			"rename p-1 Runbook v2"},
		{http.MethodPost, "/pages/p-1/purge?confirm=runbook&reason=why", nil,
			"purge p-1 why"},
		{http.MethodPut, "/pages/p-1", map[string]any{"body": "new steps"}, "save p-1"},
	} {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			t.Parallel()
			r := &rig{t: t, mux: http.NewServeMux(), reader: newReader(),
				writes: &writes{}, kb: newKB()}
			r.kb.page.Page.Container = "TS"
			opts := r.options(chart{})
			opts.Pages.SkillsContainer = func() string { return "ts" }
			svc, err := workapi.New(opts)
			if err != nil || svc == nil {
				t.Fatalf("New: %v (%v)", err, svc)
			}
			if err := svc.Routes(r.mux); err != nil {
				t.Fatalf("Routes: %v", err)
			}
			headers := []string{"If-Match", "2"}

			got := r.do(as(operator), c.method, c.target, c.body, headers...)
			if got.status != http.StatusForbidden {
				t.Errorf("without config:write it answered %d: %v", got.status, got.body)
			}
			if len(r.kb.did) != 0 {
				t.Fatalf("a refused caller reached the store: %v", r.kb.did)
			}

			withConfig := operator
			withConfig.Grants = append(slices.Clone(operator.Grants), iam.GrantConfigWrite)
			got = r.do(as(withConfig), c.method, c.target, c.body, headers...)
			if got.status != http.StatusOK {
				t.Errorf("with config:write it answered %d: %v", got.status, got.body)
			}
			if !slices.Equal(r.kb.did, []string{c.did}) {
				t.Errorf("the store did %v, want %q", r.kb.did, c.did)
			}
		})
	}
}

// A REMARK IS TAKEN DOWN BY ITS AUTHOR, OR BY A MODERATOR — and the store is
// told which.
func TestARemarkIsTakenDownByItsAuthorOrAModerator(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		who      iam.Principal
		want     int
		moderate bool
	}{
		{"its author", colleague("ana"), http.StatusOK, false},
		{"a colleague", colleague("bo"), http.StatusForbidden, false},
		{"a moderator", admin("root"), http.StatusOK, true},
	} {
		r := newRig(t, chart{})
		got := r.do(as(c.who), http.MethodDelete, "/pages/p-1/comments/c-1", nil)
		if got.status != c.want {
			t.Errorf("%s: answered %d, want %d", c.name, got.status, c.want)
			continue
		}
		if c.want == http.StatusOK && !slices.Equal(r.kb.moderat, []bool{c.moderate}) {
			t.Errorf("%s: the store was told moderate=%v", c.name, r.kb.moderat)
		}
	}
}

// ---- the surface as mounted --------------------------------------------- //

// EVERY ROUTE THIS SURFACE MOUNTS IS GUARDED, and it mounts exactly these.
//
// Guarded is what a route IS on this engine; the exemption list is the whole
// of what is not. A write surface landing on it would file work with no
// author — and the list below is the one docs/reference/api-endpoints.md
// documents, so a route added without it fails here first.
func TestEveryWriteRouteIsGuardedAndDocumented(t *testing.T) {
	t.Parallel()
	seen := &patternMux{}
	svc, err := workapi.New(newRig(t, chart{}).options(chart{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Routes(seen); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	want := []string{
		"DELETE /pages/{id}",
		"DELETE /pages/{id}/comments/{cid}",
		"DELETE /work/items/{key}",
		"PATCH /pages/{id}/comments/{cid}",
		"PATCH /work/items/{key}",
		"PATCH /work/items/{key}/comments/{cid}",
		"POST /pages",
		"POST /pages/{id}/comments",
		"POST /pages/{id}/purge",
		"POST /pages/{id}/rename",
		"POST /pages/{id}/restore",
		"POST /work/items",
		"POST /work/items/{key}/comments",
		"POST /work/items/{key}/depend",
		"POST /work/items/{key}/purge",
		"POST /work/items/{key}/rank",
		"POST /work/items/{key}/relate",
		"POST /work/items/{key}/restore",
		"POST /work/projects/{key}/tags",
		"POST /work/views",
		"PUT /pages/{id}",
		"PUT /work/catalogue",
		"PUT /work/people/{handle}/inbox",
		"PUT /work/people/{handle}/pins",
		"PUT /work/people/{handle}/priorities",
		"PUT /work/projects/{key}",
	}
	got := slices.Sorted(slices.Values(seen.patterns))
	if !slices.Equal(got, want) {
		t.Errorf("the surface mounts\n%v\nwant\n%v", got, want)
	}
	for _, pattern := range got {
		_, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{key}", "ENG-1", "{id}", "p-1",
			"{cid}", "c-1", "{handle}", "ana").Replace(path)
		if auth.Unguarded(path) {
			t.Errorf("%s is exempt from the guard, so a write surface is "+
				"reachable with no credential", pattern)
		}
	}
}

// patternMux records what was mounted on it and serves nothing.
type patternMux struct{ patterns []string }

func (m *patternMux) Handle(pattern string, _ http.Handler) {
	m.patterns = append(m.patterns, pattern)
}
