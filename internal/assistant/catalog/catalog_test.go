package catalog_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE CATALOGUE HOLDS TOGETHER: every tool declares a schema, a route it may
// send and a preview exactly when it proposes, every exposure row names one
// surface and a tool of the kind it says, and no curated tool reads: every read
// is read_api's. Mutation: name a write tool on a read row and this fails.
func TestTheCatalogueHoldsTogether(t *testing.T) {
	t.Parallel()
	if err := catalog.Check(); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range catalog.Tools() {
		names[tool.Name] = true
		if tool.Kind == catalog.KindRead {
			t.Errorf("%s is a curated read; every read is %s's", tool.Name, catalog.ReadAPITool)
		}
	}
	for _, want := range []string{"create_task", "update_task",
		"comment_on_task", "answer_decision", "set_my_priorities", "mark_inbox",
		"pause_agent", "resume_agent", "steer_turn", "answer_executor", "create_page",
		"edit_page", "share_artifact", "unshare_artifact", "post_chat_message",
		"reply_in_chat_thread", "send_direct_message", "edit_chat_message",
		"delete_chat_message", "update_chat_room", "set_chat_room_members",
		"leave_chat_room", "propose_page_change", "open_in_app"} {
		if !names[want] {
			t.Errorf("the catalogue has no %s", want)
		}
	}
}

// identityNames walks a JSON Schema's every property name, at any depth.
func identityNames(schema map[string]any, path string, found func(string)) {
	props, _ := schema["properties"].(map[string]any)
	for name, p := range props {
		if slices.Contains(catalog.IdentityArguments, name) {
			found(path + "." + name)
		}
		if sub, ok := p.(map[string]any); ok {
			identityNames(sub, path+"."+name, found)
			if items, ok := sub["items"].(map[string]any); ok {
				identityNames(items, path+"."+name+"[]", found)
			}
		}
	}
}

// NO ASSISTANT TOOL NAMES WHO ACTS.
//
// Identity is the grant a call is sent under and nothing else, so no schema a
// model is handed declares an argument naming the company, a principal, an
// actor, a login, a credential or an operation, at any depth; and a call that
// sends one anyway, or any argument its schema does not declare, is refused
// before anything is built. Mutation: add `login` to a tool's schema, or let
// Check pass an undeclared argument, and this fails.
func TestNoAssistantToolNamesWhoActs(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		identityNames(tool.Parameters, tool.Name, func(path string) {
			t.Errorf("%s declares an identity argument", path)
		})
		if ap, _ := tool.Parameters["additionalProperties"].(bool); ap {
			t.Errorf("%s admits arguments its schema does not declare", tool.Name)
		}
		args := assistanttest.ExampleArgs(t, tool.Parameters)
		for _, extra := range append(slices.Clone(catalog.IdentityArguments), "undeclared") {
			bad := maps.Clone(args)
			bad[extra] = "x"
			if err := tool.Check(bad); !errors.Is(err, catalog.ErrInvalidArguments) {
				t.Errorf("%s took the argument %q: %v", tool.Name, extra, err)
			}
		}
	}
	if err := mustTool(t, "propose_page_change").Check(map[string]any{
		"action": "work.filter.apply", "args": map[string]any{"login": "x"}}); err == nil {
		t.Error("a page action's payload named a login and was taken")
	}
}

func mustTool(t *testing.T, name string) catalog.Tool {
	t.Helper()
	tool, ok := catalog.Lookup(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	return tool
}

// EVERY TOOL SENDS ONLY ITS OWN ROUTES, with each path value one escaped
// segment: a call's request is checked against the tool's Routes after it is
// built, so a value carrying a slash can never reach another route. Mutation:
// drop the escape in at() and the traversal case reaches /worklet/../secrets.
func TestEveryToolSendsOnlyItsOwnRoutes(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		// A LINK AND THE ENGINE'S OWN DOCUMENTATION SEND NOTHING.
		if tool.Kind == catalog.KindNavigate || tool.Kind == catalog.KindDocs {
			continue
		}
		req, err := tool.Request(assistanttest.ExampleArgs(t, tool.Parameters))
		if err != nil {
			t.Errorf("%s: %v", tool.Name, err)
			continue
		}
		if tool.Kind == catalog.KindRead && req.Method != "GET" {
			t.Errorf("the read %s sends %s", tool.Name, req.Method)
		}
		if req.IdempotencyKey != "" {
			t.Errorf("%s mints its own operation key", tool.Name)
		}
	}
	_, read, _ := generic(t, admin)
	req, err := read.Request(map[string]any{"route": "GET /worklet/{id}",
		"path": map[string]any{"id": "../../secrets"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/worklet/..%2F..%2Fsecrets" {
		t.Errorf("a traversal key was sent as %q", req.Path)
	}
	if _, ok := read.RouteOf(req); !ok {
		t.Errorf("the read's own request %s is not one of its routes", req.Path)
	}
}

// A WRITE'S TIER IS DERIVED AND NEVER LOWERED: no row states a tier below what
// the authority table and the act tool's own hint derive (internal/agent/builtin
// holds the hint to the act tool's annotation), a human-only verb is R3, and a
// share, which reaches other people, is R3 whatever its row derives.
// Mutation: state update_task as R2 and this fails.
func TestATiersDerivationIsNeverLowered(t *testing.T) {
	t.Parallel()
	rank := func(tier catalog.Tier) int { return slices.Index(catalog.Tiers, tier) }
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindWrite {
			continue
		}
		derived := catalog.DeriveTier(authz.Action(tool.Act), tool.Destructive)
		if rank(tool.Tier) < rank(derived) {
			t.Errorf("%s states %s and derives %s", tool.Name, tool.Tier, derived)
		}
		if only, _ := authz.HumanOnlyOf(authz.Action(tool.Act)); only &&
			tool.Tier != catalog.TierConsequential {
			t.Errorf("%s executes as the human-only %s and is %s", tool.Name, tool.Act, tool.Tier)
		}
		if class, _ := authz.AssistOf(authz.Action(tool.Act)); class != authz.AssistApprove {
			t.Errorf("%s executes as %s, which the authority table classes %q, not approve",
				tool.Name, tool.Act, class)
		}
	}
	for _, name := range []string{"share_artifact", "unshare_artifact", "answer_decision"} {
		if tier := mustTool(t, name).Tier; tier != catalog.TierConsequential {
			t.Errorf("%s is %s, want R3", name, tier)
		}
	}
}

// ONLY WHAT THE PERSON CAN TAKE IS OFFERED: a write whose act tool the
// viewer's acts do not list, and an administrator's write to anybody else, are
// left out, in a stable order. Mutation: offer every write and the person with
// write_page alone is handed create_task.
func TestOnlyWhatThePersonCanTakeIsOffered(t *testing.T) {
	t.Parallel()
	names := func(tools []catalog.Tool) []string {
		var out []string
		for _, tool := range tools {
			out = append(out, tool.Name)
		}
		return out
	}
	writer := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantDoclet}}
	offered := names(catalog.Offered(writer, []string{"write_page"}))
	for _, want := range []string{"create_page", "propose_page_change", "open_in_app"} {
		if !slices.Contains(offered, want) {
			t.Errorf("a knowledge person is not offered %s", want)
		}
	}
	for _, refused := range []string{"create_task", "edit_page", "update_company_principles",
		"set_token_budget"} {
		if slices.Contains(offered, refused) {
			t.Errorf("a knowledge person with write_page alone is offered %s", refused)
		}
	}
	admin := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}}
	all := names(catalog.Offered(admin, nil))
	// THE PRINCIPLES' WRITE IS NO OPERATOR TOOL, so acts never lists it: an
	// administrator is offered it by the route's own class, with no acts.
	if !slices.Contains(all, "update_company_principles") {
		t.Errorf("an administrator is not offered update_company_principles: %v", all)
	}
	if again := names(catalog.Offered(admin, nil)); !slices.Equal(all, again) {
		t.Error("two offers to one person differ in order")
	}
}

// THE PAGE ACTIONS ARE THE V1 SET, and a call naming one is held to what the
// screen mounted now. Mutation: accept an action the turn's page context did
// not list and this fails.
func TestAPageActionIsOnlyOneTheScreenAccepts(t *testing.T) {
	t.Parallel()
	var got []string
	for _, a := range catalog.PageActions {
		got = append(got, a.ID)
		if a.Scope == "" || !a.Confirm.Valid() {
			t.Errorf("%s has scope %q and confirm %q", a.ID, a.Scope, a.Confirm)
		}
	}
	want := []string{"home.range.set", "home.feed.set", "work.filter.apply",
		"work.filters.clear", "work.arrange.set", "project.lens.set", "item.activity.set",
		"spend.range.set", "spend.group.set", "live.filter.apply", "turns.filter.apply",
		"knowledge.search.set", "draft.set", "org.draft.apply_ops", "budgets.ceiling.prefill",
		catalog.ReadScreen}
	if !slices.Equal(got, want) {
		t.Errorf("the page actions are %v, want %v", got, want)
	}
	// READ_SCREEN NAMES ITS OWN ACTION, held to the screen's accepts as any other.
	readScreen := mustTool(t, catalog.ReadScreenTool)
	if _, err := catalog.PageActionFor(readScreen, catalog.Args{}, &wire.PageContext{
		Accepts: []string{"spend.range.set"}}); !errors.Is(err, catalog.ErrNotAccepted) {
		t.Errorf("a screen read the page did not offer = %v, want ErrNotAccepted", err)
	}
	if a, err := catalog.PageActionFor(readScreen, catalog.Args{}, &wire.PageContext{
		Accepts: []string{catalog.ReadScreen},
		ActionArgs: map[string]*wire.ActionArgs{catalog.ReadScreen: {
			Type: wire.ArgTypes{wire.ArgObject}, AdditionalProperties: new(false)}},
	}); err != nil || a.Confirm != wire.ConfirmRead {
		t.Errorf("an offered screen read = %+v, %v; want confirm read", a, err)
	}
	args := catalog.Args{"action": "work.filter.apply", "args": map[string]any{"status": "todo"}}
	if _, err := catalog.PageActionFor(catalog.Tool{}, args, &wire.PageContext{
		Accepts: []string{"spend.range.set"}}); !errors.Is(err, catalog.ErrNotAccepted) {
		t.Errorf("an action the screen did not mount = %v, want ErrNotAccepted", err)
	}
	if _, err := catalog.PageActionFor(catalog.Tool{}, args, &wire.PageContext{
		Accepts:    []string{"work.filter.apply"},
		ActionArgs: map[string]*wire.ActionArgs{"work.filter.apply": {Type: wire.ArgTypes{wire.ArgObject}}},
	}); err != nil {
		t.Errorf("an accepted action = %v", err)
	}
	req, err := mustTool(t, "propose_page_change").Request(map[string]any{
		"action": "work.filter.apply", "args": map[string]any{"status": "blocked"}})
	if err != nil || req.Method != catalog.PageMethod ||
		req.Path != catalog.PageActionPrefix+"work.filter.apply" ||
		string(req.Body) != `{"status":"blocked"}` {
		t.Errorf("a page action's request = %+v, %v", req, err)
	}
}

// AN OPEN IN APP LINK IS THE ENGINE'S, with the record's own read as its
// permission probe, and a screen with no record has none. Mutation: build the
// probe from a different parameter than the link and the page case fails.
func TestAnOpenInAppLinkProbesTheRecordsOwnRead(t *testing.T) {
	t.Parallel()
	id := "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	link, probe, err := catalog.Open(catalog.Args{"screen": "page", "id": id})
	if err != nil || link.Href != "#/doclet/pages/"+id || probe == nil ||
		probe.Path != "/doclet/pages/"+id {
		t.Errorf("a page link = %+v, probe %+v, %v", link, probe, err)
	}
	if _, probe, err := catalog.Open(catalog.Args{"screen": "budgets"}); err != nil || probe != nil {
		t.Errorf("a screen link probed %+v, %v", probe, err)
	}
	if _, _, err := catalog.Open(catalog.Args{"screen": "item", "id": "a/b"}); !errors.Is(err,
		catalog.ErrInvalidArguments) {
		t.Errorf("a link with a path in its id = %v", err)
	}
}

// EVERY TABLE FITS THE DRAWER: it names the few columns the drawer draws, the
// first of them the text a row is known by (never a figure or a date), and
// leaves the rest to the downloads and the model.
func TestEveryTableDrawsAFewColumnsNameFirst(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		for variant, spec := range tool.Cards {
			table := spec.Table
			if table == nil {
				continue
			}
			if len(table.Brief) == 0 || len(table.Brief) > cards.MaxBrief {
				t.Errorf("%s %q draws %d columns in the drawer, want 1 to %d", tool.Name,
					variant, len(table.Brief), cards.MaxBrief)
				continue
			}
			for _, c := range table.Columns {
				if c.Key == table.Brief[0] && c.Type != wire.ColumnText &&
					c.Type != wire.ColumnLabel && c.Type != wire.ColumnDatetime {
					t.Errorf("%s %q leads with %q, a %s column, rather than a name",
						tool.Name, variant, c.Key, c.Type)
				}
			}
		}
	}
}

// EVERY STEP READS AS A SENTENCE, never as the tool's model-facing name: a read
// by what it reads, a write as the change being prepared, the task it touches
// named where the call names one.
func TestEveryStepIsASentence(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		label := tool.StepLabel(map[string]any{}, cards.Options{})
		if label == "" || label == tool.Name || label[:1] != strings.ToUpper(label[:1]) {
			t.Errorf("%s steps read %q, want a sentence", tool.Name, label)
		}
	}
	update := assistanttest.Tool(t, "update_task")
	if got := update.StepLabel(map[string]any{"item": "FAIR-2"},
		cards.Options{}); got != "Preparing a change to FAIR-2" {
		t.Errorf("an update of FAIR-2 reads %q", got)
	}
	page := assistanttest.Tool(t, "propose_page_change")
	if got := page.StepLabel(map[string]any{}, cards.Options{}); got != "Preparing a change to this screen" {
		t.Errorf("a page change reads %q", got)
	}
}

// A STEP SHOWS THE REQUEST IT MADE, ITS METHOD ONCE: a read's step carries the
// method apart and the path it was sent to, wildcards filled in and no query,
// so "GET /query/knowledge" names the question asked rather than the pattern.
// Mutation: show the matched route pattern and the named cases fail.
func TestAStepShowsTheRequestItMade(t *testing.T) {
	t.Parallel()
	_, read, _ := generic(t, admin)
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"route": "GET /worklet/{id}", "path": map[string]any{"id": "ENG-1"}},
			"/worklet/ENG-1"},
		{map[string]any{"route": "GET /query/{what}", "path": map[string]any{"what": "knowledge"},
			"query": map[string]any{"q": "the mission"}}, "/query/knowledge"},
		{map[string]any{"route": "GET /config"}, "/config"},
	} {
		req, err := read.Request(c.args)
		if err != nil {
			t.Fatal(err)
		}
		shown, ok := read.StepRequest(req)
		if !ok || shown.Method != "GET" || shown.Route != c.want ||
			strings.ContainsAny(shown.Route, "?{") {
			t.Errorf("%v's step shows %+v, want GET %s", c.args, shown, c.want)
		}
	}
	if _, ok := read.StepRequest(exec.Request{Method: "GET", Path: "/pages/x"}); ok {
		t.Error("a request read_api could not have built is shown as one it made")
	}
}

// EVERY FAULT OF A CALL IN ONE REFUSAL, for every assistant tool (ADR-0059): two
// undeclared arguments are both named, where the check named the first.
//
// Mutation: answer only the first problem and the second is never named.
func TestEveryAssistantToolNamesEveryFaultAtOnce(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		bad := maps.Clone(assistanttest.ExampleArgs(t, tool.Parameters))
		bad["undeclared_one"], bad["undeclared_two"] = "x", "y"
		err := tool.Check(bad)
		if err == nil || !strings.Contains(err.Error(), "undeclared_one") ||
			!strings.Contains(err.Error(), "undeclared_two") {
			t.Errorf("%s did not name both faults: %v", tool.Name, err)
		}
	}
}

// TestAStepSaysItsScopeOnce: a step names what its call was about once, so a
// tool whose label already says its scope is not told it again after a
// comma ("Searching the web for “x”, “x”"). Mutation: append the scope to
// every label again.
func TestAStepSaysItsScopeOnce(t *testing.T) {
	t.Parallel()
	search := catalog.SearchWebTool()
	args := map[string]any{"query": "Crewlet release notes"}
	if got := search.StepLabel(args, cards.Options{}); got != "Searching the web for “Crewlet release notes”" {
		t.Errorf("a web search's step reads %q", got)
	}
	for _, tool := range append(catalog.Tools(), catalog.FetchWebPageTool(), search) {
		if tool.Scope == nil {
			continue
		}
		a := assistanttest.ExampleArgs(t, tool.Parameters)
		scope := tool.ScopeOf(catalog.Args(a), cards.Options{})
		if label := tool.StepLabel(a, cards.Options{}); scope != "" &&
			strings.Count(label, scope) > 1 {
			t.Errorf("%s's step says its scope twice: %q", tool.Name, label)
		}
	}
}
