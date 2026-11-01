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
// send, a card for every shape of answer it reads, a preview exactly when it
// proposes, and every exposure row names one surface and a tool of the kind it
// says. Mutation: name a write tool on a read row and this fails.
func TestTheCatalogueHoldsTogether(t *testing.T) {
	t.Parallel()
	if err := catalog.Check(); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range catalog.Tools() {
		names[tool.Name] = true
	}
	for _, want := range []string{"search_work", "list_tasks", "get_task", "get_task_thread",
		"get_task_cost", "get_my_work", "list_projects", "get_workload", "list_decisions",
		"list_agents", "get_agent", "get_org_chart", "list_turns", "list_executors",
		"get_spend", "get_spend_series", "list_budgets", "search_knowledge", "list_spaces", "get_page",
		"get_company_feed", "list_artifacts", "get_artifact", "list_integrations",
		"get_fleet_health", "list_config_changes", "create_task", "update_task",
		"comment_on_task", "answer_decision", "set_my_priorities", "mark_inbox",
		"pause_agent", "resume_agent", "steer_turn", "answer_coding_run", "create_page",
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
		if tool.Kind == catalog.KindNavigate {
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
	req, err := mustTool(t, "get_task").Request(map[string]any{"item": "../../secrets"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/worklet/..%2F..%2Fsecrets" {
		t.Errorf("a traversal key was sent as %q", req.Path)
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

// ONLY WHAT THE PERSON CAN TAKE IS OFFERED: a read whose app they do not hold,
// an administrator's read to anybody else, and a write whose act tool the
// viewer's acts do not list are left out, in a stable order. Mutation: offer
// every read and the person without Worklet is handed list_tasks.
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
	for _, want := range []string{"search_knowledge", "get_page", "list_agents", "create_page",
		"propose_page_change", "open_in_app"} {
		if !slices.Contains(offered, want) {
			t.Errorf("a knowledge person is not offered %s", want)
		}
	}
	for _, refused := range []string{"list_tasks", "list_integrations", "create_task",
		"edit_page", "get_company_principles", "update_company_principles"} {
		if slices.Contains(offered, refused) {
			t.Errorf("a knowledge person with write_page alone is offered %s", refused)
		}
	}
	admin := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}}
	all := names(catalog.Offered(admin, nil))
	if !slices.Contains(all, "list_integrations") || !slices.Contains(all, "list_tasks") {
		t.Errorf("an administrator is offered %v", all)
	}
	// THE PRINCIPLES' WRITE IS NO OPERATOR TOOL, so acts never lists it: an
	// administrator is offered it by the route's own class, with no acts.
	for _, want := range []string{"get_company_principles", "update_company_principles"} {
		if !slices.Contains(all, want) {
			t.Errorf("an administrator is not offered %s", want)
		}
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
		"knowledge.search.set", "draft.set", "org.draft.apply_ops", "budgets.ceiling.prefill"}
	if !slices.Equal(got, want) {
		t.Errorf("the page actions are %v, want %v", got, want)
	}
	args := catalog.Args{"action": "work.filter.apply", "args": map[string]any{"status": "todo"}}
	if _, err := catalog.PageActionFor(args, &wire.PageContext{
		Accepts: []string{"spend.range.set"}}); !errors.Is(err, catalog.ErrNotAccepted) {
		t.Errorf("an action the screen did not mount = %v, want ErrNotAccepted", err)
	}
	if _, err := catalog.PageActionFor(args, &wire.PageContext{
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

// A STEP SHOWS THE REQUEST IT MADE, ITS METHOD ONCE: every read's step carries
// the method apart and the path it was sent to, wildcards filled in and no
// query, so the drawer's "search_knowledge · GET /query/knowledge" names the
// question asked rather than the pattern "/query/{what}", and never doubles
// the method. Mutation: show the matched route pattern and the two named cases
// fail; show the whole request URL and the query check fails.
func TestAStepShowsTheRequestItMade(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead {
			continue
		}
		req, err := tool.Request(assistanttest.ExampleArgs(t, tool.Parameters))
		if err != nil {
			t.Errorf("%s: %v", tool.Name, err)
			continue
		}
		shown, ok := tool.StepRequest(req)
		if !ok || shown.Method != req.Method || shown.Route != req.Path ||
			strings.ContainsAny(shown.Route, "?{") {
			t.Errorf("%s's step shows %+v for %s %s", tool.Name, shown, req.Method, req.Path)
		}
	}
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_task", map[string]any{"item": "ENG-1"}, "/worklet/ENG-1"},
		{"search_knowledge", map[string]any{"q": "the mission"}, "/query/knowledge"},
	} {
		tool := mustTool(t, c.tool)
		req, err := tool.Request(c.args)
		if err != nil {
			t.Fatal(err)
		}
		if shown, _ := tool.StepRequest(req); shown == nil || shown.Route != c.want {
			t.Errorf("%s's step shows %+v, want the route %s", c.tool, shown, c.want)
		}
	}
	if _, ok := mustTool(t, "get_task").StepRequest(exec.Request{Method: "GET",
		Path: "/pages/x"}); ok {
		t.Error("a request get_task could not have built is shown as one it made")
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
