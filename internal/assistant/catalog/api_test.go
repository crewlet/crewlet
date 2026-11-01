package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// apiRoutes is a route table as an App hands it over: guarded routes with
// their verbs, a question route and the act route that decide inside, and one
// route whose verb is on the human-only list. Every POST says what it does, as
// the App's own walk holds them to.
var apiRoutes = []catalog.Route{
	{Pattern: "GET /worklet/{id}"},
	{Pattern: "GET /query/{what}"},
	{Pattern: catalog.ActRoute, Effect: authz.EffectCalls},
	{Pattern: "GET /config", Action: authz.ActionConfigRead},
	{Pattern: "PATCH /config", Action: authz.ActionOrgWrite},
	{Pattern: "GET /config/units/{id}", Action: authz.ActionOrgRead},
	{Pattern: "PUT /config/units/{id}", Action: authz.ActionOrgWrite,
		Absent: authz.AddedElsewhere(unitsElsewhere)},
	{Pattern: "GET /config/mcp-servers/{id}", Action: authz.ActionConfigRead},
	{Pattern: "PUT /config/mcp-servers/{id}", Action: authz.ActionConfigWrite,
		Absent: authz.CreatesWhenAbsent()},
	{Pattern: "GET /worklet/files/{project}/{path...}", Action: authz.ActionFileRead},
	{Pattern: "PUT /worklet/files/{project}/{path...}", Action: authz.ActionFileWrite,
		Absent: authz.CreatesWhenAbsent()},
	{Pattern: "GET /worklet/projects/{key}", Action: authz.ActionProjectRead},
	{Pattern: "PUT /worklet/projects/{key}", Action: authz.ActionProjectWrite},
	{Pattern: "DELETE /worklet/items/{key}", Action: authz.ActionWorkRead},
	{Pattern: "POST /worklet/items", Action: authz.ActionWorkCreate, Effect: authz.EffectCreates},
	{Pattern: "POST /iam/invitations", Action: authz.ActionSignInWrite,
		Effect: authz.EffectCreates},
}

// unitsElsewhere is where the API says a unit is added.
const unitsElsewhere = "a member of units is added by replacing the unit it sits in"

// generic are the three generic tools over apiRoutes for p, whose act route
// writes are acts.
func generic(t *testing.T, p iam.Principal, acts ...string) (docs, read, change catalog.Tool) {
	t.Helper()
	table, err := catalog.NewRouteTable(apiRoutes)
	if err != nil {
		t.Fatal(err)
	}
	tools := catalog.GenericTools(table, p, acts)
	return tools[0], tools[1], tools[2]
}

// admin is an administrator at their keyboard.
var admin = iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive,
	Grants: []iam.Grant{iam.GrantAdmin}}

// A GENERIC CALL NAMES A ROUTE OF THE TABLE, NEVER A URL.
//
// The request is built from the route the call names, each wildcard's value
// one escaped segment (a `{name...}` tail segment by segment), and refused
// before anything is sent when the route is not the table's, is of the other
// tool's kind, misses a value, names a value for no wildcard, holds a dot
// segment, or names an act tool the person's acts do not list. A route whose
// verb is on the human-only list is not in the table at all. A HEAD is its
// GET's route. Mutation: build the path by concatenation, or keep a never
// verb in the table, and a case here goes through.
func TestAGenericCallNamesARouteOfTheTable(t *testing.T) {
	t.Parallel()
	_, read, change := generic(t, admin, "merge_work_item")
	for _, c := range []struct {
		tool       catalog.Tool
		args       map[string]any
		method     string
		path, rawQ string
	}{
		{read, map[string]any{"route": "GET /worklet/{id}",
			"path": map[string]any{"id": "ENG/42"}}, "GET", "/worklet/ENG%2F42", ""},
		{read, map[string]any{"route": "GET /worklet/files/{project}/{path...}",
			"path": map[string]any{"project": "ENG", "path": "notes/q 3.md"}},
			"GET", "/worklet/files/ENG/notes/q%203.md", ""},
		{read, map[string]any{"route": "HEAD /worklet/{id}",
			"path": map[string]any{"id": "ENG-42"}}, "HEAD", "/worklet/ENG-42", ""},
		{read, map[string]any{"route": "GET /query/{what}", "path": map[string]any{"what": "budgets"},
			"query": map[string]any{"limit": "5", "kinds": []any{"a", "b"}}},
			"GET", "/query/budgets", "kinds=a&kinds=b&limit=5"},
		{change, map[string]any{"route": catalog.ActRoute,
			"path": map[string]any{"tool": "merge_work_item"}, "body": map[string]any{"args": map[string]any{}}},
			"POST", "/operator/act/merge_work_item", ""},
	} {
		req, err := c.tool.Request(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		u, _ := req.URL()
		if req.Method != c.method || u.EscapedPath() != c.path || u.RawQuery != c.rawQ {
			t.Errorf("%v built %s %s?%s, want %s %s?%s", c.args, req.Method, u.EscapedPath(),
				u.RawQuery, c.method, c.path, c.rawQ)
		}
	}
	for name, c := range map[string]struct {
		tool catalog.Tool
		args map[string]any
	}{
		"a URL":            {read, map[string]any{"route": "GET https://example.com/x"}},
		"a route not held": {read, map[string]any{"route": "GET /secrets"}},
		"a write as a read": {read, map[string]any{"route": "PATCH /config",
			"body": map[string]any{}}},
		"a read as a write": {change, map[string]any{"route": "GET /config"}},
		"a missing value":   {read, map[string]any{"route": "GET /worklet/{id}"}},
		"a value for nothing": {read, map[string]any{"route": "GET /config",
			"path": map[string]any{"id": "x"}}},
		"a dot segment": {read, map[string]any{"route": "GET /worklet/files/{project}/{path...}",
			"path": map[string]any{"project": "ENG", "path": "../secrets"}}},
		"a dot value": {read, map[string]any{"route": "GET /worklet/{id}",
			"path": map[string]any{"id": ".."}}},
		"an act tool not the person's": {change, map[string]any{"route": catalog.ActRoute,
			"path": map[string]any{"tool": "write_work_catalogue"}, "body": map[string]any{}}},
		"a human-only route": {change, map[string]any{"route": "POST /iam/invitations",
			"body": map[string]any{}}},
		"a read with a body": {read, map[string]any{"route": "GET /config",
			"body": map[string]any{"x": 1}}},
	} {
		if req, err := c.tool.Request(c.args); err == nil {
			t.Errorf("%s was built: %s %s", name, req.Method, req.Path)
		} else if !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s was refused with %v, not as invalid arguments", name, err)
		}
	}
}

// resource is a turn grant answering GET path with doc under etag, and 404
// for anything else.
func resource(path, etag string, doc any) proposal.Read {
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		if r.Method != http.MethodGet || r.Path != path {
			return exec.Response{Status: http.StatusNotFound,
				Body: []byte(`{"error":"not_found"}`)}, nil
		}
		body, _ := json.Marshal(doc)
		h := http.Header{}
		if etag != "" {
			h.Set("ETag", etag)
		}
		return exec.Response{Status: http.StatusOK, Header: h, Body: body}, nil
	}
}

// rows are a preview's changes by field.
func rows(p proposal.Preview) map[string]wire.Change {
	out := map[string]wire.Change{}
	for _, c := range p.Changes {
		out[c.Field] = c
	}
	return out
}

// A GENERIC CHANGE IS PREVIEWED FROM THE RESOURCE AS IT IS NOW.
//
// A PATCH is merged onto the representation its path reads (RFC 7396, `null`
// deleting), and only the fields that move are rows; a list of records is
// matched by the identity each carries, so one added seat is one added
// member and the seat beside it is no row; the request and its body, redacted,
// are the card's technical details for an administrator, never a row. A strong
// entity tag binds the change and becomes its
// If-Match; a representation with no tag but a version field binds the change
// to that field, which the approval re-reads. A DELETE shows what it removes,
// a POST what it creates, and a change that moves nothing is refused. The
// verb's step-up is on the card. Mutation: diff the body instead of the
// merged document, match lists by position, or drop the revision, and a case
// here fails.
func TestAGenericChangeIsPreviewedFromTheResource(t *testing.T) {
	t.Parallel()
	_, _, change := generic(t, admin)
	document := map[string]any{"name": "Acme", "mission": "Make every invoice add up.",
		"turn_engine": map[string]any{"onboarding_max_tool_rounds": 16, "temperature": 0.2},
		"roles":       []any{map[string]any{"handle": "ceo", "name": "CEO"}},
		"labels":      []any{"billing", "core"}}
	read := resource("/config", `"rev-7"`, document)
	secret := "ghp_" + strings.Repeat("b", 36)
	p, err := change.Preview(t.Context(), read, wednesday, map[string]any{
		"route": "PATCH /config", "body": map[string]any{
			"turn_engine": map[string]any{"onboarding_max_tool_rounds": 32, "temperature": nil},
			"roles": []any{map[string]any{"handle": "ceo", "name": "CEO"},
				map[string]any{"handle": "crewlet-swe", "name": "crewlet-swe",
					"responsibilities": []any{"Build", "Review"}}},
			"labels":   []any{"billing", "growth"},
			"_summary": "raise the cap with " + secret}})
	if err != nil {
		t.Fatal(err)
	}
	got := rows(p)
	for field, want := range map[string]struct {
		kind     wire.ChangeKind
		from, to string
	}{
		"turn_engine.onboarding_max_tool_rounds": {wire.ChangeChanged, "16", "32"},
		"turn_engine.temperature":                {wire.ChangeRemoved, "0.2", ""},
		"roles.crewlet-swe.name":                 {wire.ChangeAdded, "", "crewlet-swe"},
		"roles.crewlet-swe.responsibilities":     {wire.ChangeAdded, "", "Build, Review"},
		"labels":                                 {wire.ChangeChanged, "billing, core", "billing, growth"},
		"_summary":                               {wire.ChangeAdded, "", "raise the cap with [REDACTED:github-token]"},
	} {
		c, ok := got[field]
		if !ok || c.Kind != want.kind || c.From != want.from || c.To != want.to {
			t.Errorf("row %s is %+v, want %+v", field, c, want)
		}
	}
	// A LIST OF PLAIN VALUES IS DRAWN AS A LIST, never a word diff of its
	// joined line: its items ride beside the line, on each side it has.
	if c := got["roles.crewlet-swe.responsibilities"]; !slices.Equal(c.Items, []string{"Build", "Review"}) {
		t.Errorf("an added list carries the items %q", c.Items)
	}
	if c := got["labels"]; !slices.Equal(c.FromItems, []string{"billing", "core"}) ||
		!slices.Equal(c.Items, []string{"billing", "growth"}) {
		t.Errorf("a changed list carries the items %q to %q", c.FromItems, c.Items)
	}
	for _, untouched := range []string{"name", "mission", "roles.ceo.name"} {
		if _, ok := got[untouched]; ok {
			t.Errorf("the preview shows %s, which the change leaves alone", untouched)
		}
	}
	if p.Technical == nil || p.Technical.Method != http.MethodPatch || p.Technical.Path != "/config" ||
		strings.Contains(p.Technical.Body, secret) ||
		!strings.Contains(p.Technical.Body, `"onboarding_max_tool_rounds": 32`) {
		t.Errorf("the card's technical details are %+v, not the request redacted", p.Technical)
	}
	for field := range got {
		if strings.HasPrefix(field, "request") && field != "request.summary" {
			t.Errorf("the request is a row (%s), not the technical details", field)
		}
	}
	if p.Title != "Change company configuration" || p.Risk != "" {
		t.Errorf("the change is titled %q at risk %q", p.Title, p.Risk)
	}
	if p.Act != string(authz.ActionOrgWrite) || !p.StepUp {
		t.Errorf("the change is decided by %q, step-up %v", p.Act, p.StepUp)
	}
	req := proposal.RequestOf(proposalOf(p))
	if req.IfMatch != `"rev-7"` || !strings.Contains(string(req.Body), secret) {
		t.Errorf("the approval would send If-Match %q and the body %s: the request is the "+
			"call's own, bound to the tag it was shown against", req.IfMatch, req.Body)
	}

	// A VERSION FIELD, where no tag is answered, re-read by the approval.
	project := map[string]any{"key": "ENG", "name": "Engine", "version": 4}
	p, err = change.Preview(t.Context(), resource("/worklet/projects/ENG", "", project),
		wednesday, map[string]any{"route": "PUT /worklet/projects/{key}",
			"path": map[string]any{"key": "ENG"},
			"body": map[string]any{"key": "ENG", "name": "Engine room", "version": 4}})
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceRevision != "resource:/worklet/projects/ENG@version=4" ||
		proposal.RequestOf(proposalOf(p)).IfMatch != "" {
		t.Errorf("the project change is bound to %q", p.SourceRevision)
	}
	project["version"] = 5
	if now, err := catalog.Revision(t.Context(), resource("/worklet/projects/ENG", "", project),
		p.SourceRevision); err != nil || now == p.SourceRevision {
		t.Errorf("a moved project re-read as %q (%v)", now, err)
	}

	// A DELETE, a POST, and a change that moves nothing.
	p, err = change.Preview(t.Context(), resource("/worklet/items/ENG-1", "", nil), wednesday,
		map[string]any{"route": "DELETE /worklet/items/{key}", "path": map[string]any{"key": "ENG-1"}})
	if err != nil || rows(p)["removes"].To != "Item ENG-1" || p.Title != "Delete item ENG-1" ||
		p.Risk != wire.RiskDestructive || p.Summary != "This deletes item ENG-1." || p.Note == "" {
		t.Errorf("a delete of a resource nothing reads previews %+v, %v", p, err)
	}
	p, err = change.Preview(t.Context(), read, wednesday, map[string]any{
		"route": "POST /worklet/items", "body": map[string]any{"title": "Ship it"}})
	if err != nil || rows(p)["title"].Kind != wire.ChangeAdded || p.SourceRevision != "" {
		t.Errorf("a create previews %+v, %v", p, err)
	}
	if _, err = change.Preview(t.Context(), read, wednesday, map[string]any{
		"route": "PATCH /config", "body": map[string]any{"name": "Acme"}}); !errors.Is(err,
		catalog.ErrInvalidArguments) {
		t.Errorf("a change that moves nothing previewed: %v", err)
	}
}

// A CALL ON THE ACT ROUTE IS TITLED BY ITS TOOL, AND ONE A CURATED WRITE
// MAKES IS THAT WRITE'S.
//
// The act route runs one operator tool, which alone decides what changes, so
// the generic card is the call: titled by the tool, said as a person writes it
// (an initialism in it included), approved by the tool's verb, and its
// arguments shown as sent, each by its own name, never as values a create
// adds. The step it is prepared in names it the same way, and a call sending
// nothing still has a row saying so. A call a curated write makes, every
// argument one that write takes, is refused naming the write, whose preview
// reads what the tool changes; a call sending an argument no curated write
// takes stays the generic change's, so nothing the person may do is lost to
// the assistant; and a body the route would refuse once approved is refused
// before any card. Mutation: title the act route as a create, draw its
// arguments as added or label them by the `args` they travel in, spell DM as
// a word, or let the generic change propose a room's whole membership, and a
// case here fails.
func TestACallOnTheActRouteIsTitledByItsTool(t *testing.T) {
	t.Parallel()
	react, members, update := string(authz.ActionChatReact), string(authz.ActionChatMembers),
		string(authz.ActionWorkUpdate)
	open := string(authz.ActionChatOpenDM)
	_, _, change := generic(t, admin, react, members, update, open)
	call := func(tool string, args map[string]any) map[string]any {
		return map[string]any{"route": catalog.ActRoute, "path": map[string]any{"tool": tool},
			"body": map[string]any{"args": args}}
	}
	reaction := call(react, map[string]any{"channel": "c1", "message_id": "m1", "emoji": "tada"})
	p, err := change.Preview(t.Context(), resource("/nowhere", "", nil), wednesday, reaction)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "React to message" || p.ApproveLabel != "Approve and react" || p.Act != react ||
		p.Risk != "" {
		t.Errorf("a reaction is titled %q, approved by %q, decided by %q at risk %q", p.Title,
			p.ApproveLabel, p.Act, p.Risk)
	}
	if step := change.StepLabel(reaction, cards.Options{}); step != "Preparing react to message" {
		t.Errorf("a reaction is prepared in the step %q", step)
	}
	for _, c := range p.Changes {
		if c.Kind != wire.ChangeFact {
			t.Errorf("row %s is %s, want the argument as sent", c.Field, c.Kind)
		}
	}
	if emoji := rows(p)["args.emoji"]; emoji.To != "tada" || emoji.Label != "Emoji" {
		t.Errorf("the reaction's emoji is shown as %+v", emoji)
	}
	if id := rows(p)["args.message_id"]; id.To != "m1" || id.Label != "Message ID" {
		t.Errorf("the reaction's message is shown as %+v", id)
	}
	// A TOOL'S NAME IS SAID AS A PERSON WRITES IT, an initialism in it
	// included.
	p, err = change.Preview(t.Context(), resource("/nowhere", "", nil), wednesday,
		call(open, map[string]any{"to": []any{"ada"}}))
	if err != nil || p.Title != "Open DM" || p.ApproveLabel != "Approve and open" {
		t.Errorf("opening a conversation is titled %q, approved by %q (%v)", p.Title,
			p.ApproveLabel, err)
	}
	// A CALL THAT SENDS NOTHING still shows that it is the whole call.
	p, err = change.Preview(t.Context(), resource("/nowhere", "", nil), wednesday,
		call(react, map[string]any{}))
	if err != nil || len(p.Changes) != 1 || p.Changes[0].Label != "Details" ||
		p.Changes[0].To != "None" {
		t.Errorf("a call sending no argument previews %+v (%v)", p.Changes, err)
	}
	_, err = change.Request(call(members, map[string]any{"channel": "c1",
		"members": []any{map[string]any{"handle": "pm"}}}))
	if !errors.Is(err, catalog.ErrInvalidArguments) ||
		!strings.Contains(err.Error(), "set_chat_room_members") {
		t.Errorf("a room's whole membership through the generic change answered %v", err)
	}
	if _, err := change.Request(call(update, map[string]any{"item": "ENG-1",
		"if_match": 3, "status": "done"})); !errors.Is(err, catalog.ErrInvalidArguments) ||
		!strings.Contains(err.Error(), "update_task") {
		t.Errorf("a task change update_task makes answered %v", err)
	}
	if _, err := change.Request(call(update, map[string]any{"item": "ENG-1",
		"checklist": []any{"Write it"}})); err != nil {
		t.Errorf("a task change only the generic change makes was refused: %v", err)
	}
	// THE ROUTE'S OWN SHAPE: arguments sent beside `args` rather than in it
	// are refused before any card, not by the route once approved.
	if _, err := change.Request(map[string]any{"route": catalog.ActRoute,
		"path": map[string]any{"tool": react},
		"body": map[string]any{"channel": "c1", "emoji": "tada"}}); !errors.Is(err,
		catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), `"args"`) {
		t.Errorf("a call whose arguments are not its args answered %v", err)
	}
}

// A POST IS NAMED FOR WHAT ITS ROUTE DECLARES IT DOES.
//
// A POST's method says nothing of what it does, so its route says
// (authz.Policy.Effect) and the card says it after it: a destruction is titled
// by its action and what it acts on, destructive, its summary saying what goes
// and its note that the assistant cannot undo it; a rewrite by its action,
// with its body and its query as sent and, sending neither, what it acts on;
// a question says it changes nothing; a create is titled by what it creates,
// its body drawn as added. What a POST acts on is named by a read of it where
// the table serves one, and never by an opaque id its path carries. A POST
// that declares nothing is never a card at all, since nothing could say
// truthfully what it does. Mutation: title every POST a create, rate a
// destruction by its method, draw a rewrite's body as added, drop its query,
// name a record by its id, or draw a card for an undeclared POST, and a case
// here fails.
func TestAPostIsNamedForWhatItsRouteDeclares(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable([]catalog.Route{
		{Pattern: "GET /worklet/{id}"},
		{Pattern: "POST /worklet/items/{key}/purge", Action: authz.ActionWorkPurge,
			Effect: authz.EffectDestroys},
		{Pattern: "GET /doclet/pages/{id}", Action: authz.ActionPageRead},
		{Pattern: "POST /doclet/pages/{id}/restore", Action: authz.ActionPageRestore,
			Effect: authz.EffectRewrites},
		{Pattern: "POST /worklet/items/{key}/restore", Action: authz.ActionWorkRead,
			Effect: authz.EffectRewrites},
		{Pattern: "POST /artifacts/checks", Action: authz.ActionArtifactCreate,
			Effect: authz.EffectReads},
		{Pattern: "POST /worklet/items", Action: authz.ActionWorkCreate,
			Effect: authz.EffectCreates},
		{Pattern: "POST /doclet/pages", Action: authz.ActionPageCreate},
	})
	if err != nil {
		t.Fatal(err)
	}
	change := catalog.GenericTools(table, admin, nil)[2]
	preview := func(route string, path, body map[string]any) (proposal.Preview, error) {
		args := map[string]any{"route": route, "path": path}
		if body != nil {
			args["body"] = body
		}
		return change.Preview(t.Context(), resource("/nowhere", "", nil), wednesday, args)
	}

	p, err := preview("POST /worklet/items/{key}/purge", map[string]any{"key": "ENG-1"},
		map[string]any{"reason": "A key was pasted."})
	if err != nil || p.Title != "Purge item ENG-1" || p.ApproveLabel != "Approve and purge" ||
		p.Risk != wire.RiskDestructive || p.Summary != "This purges item ENG-1." ||
		p.Note != "This cannot be undone from Ask Crewlet." {
		t.Errorf("a purge reads %q / %q at risk %q: %q %q (%v)", p.Title, p.ApproveLabel, p.Risk,
			p.Summary, p.Note, err)
	}
	if reason := rows(p)["reason"]; reason.Kind != wire.ChangeFact ||
		reason.To != "A key was pasted." {
		t.Errorf("the purge's reason is shown as %+v", reason)
	}

	// WHAT A POST ACTS ON IS NAMED BY A READ OF IT, the record the answer
	// holds beside facts about it, and in neutral words where nothing names
	// it, never by the id its path carries.
	const page = "7f3c2a9e-4b1d-4c8e-9a6f-2d5e8b1c0a47"
	restore := map[string]any{"route": "POST /doclet/pages/{id}/restore",
		"path": map[string]any{"id": page}}
	p, err = change.Preview(t.Context(), resource("/doclet/pages/"+page, "", map[string]any{
		"page": map[string]any{"id": page, "title": "Runbook"}, "revision": 4}), wednesday, restore)
	if err != nil || p.Title != "Restore page Runbook" || len(p.Changes) != 1 ||
		p.Changes[0].To != "Page Runbook" {
		t.Errorf("a named page's restore reads %q with %+v (%v)", p.Title, p.Changes, err)
	}
	p, err = change.Preview(t.Context(), resource("/nowhere", "", nil), wednesday, restore)
	if err != nil || p.Title != "Restore the page" || p.Changes[0].To != "The page" {
		t.Errorf("an unread page's restore reads %q with %+v (%v)", p.Title, p.Changes, err)
	}

	p, err = preview("POST /worklet/items/{key}/restore", map[string]any{"key": "ENG-1"}, nil)
	if err != nil || p.Title != "Restore item ENG-1" || p.ApproveLabel != "Approve and restore" ||
		p.Risk != "" || len(p.Changes) != 1 || p.Changes[0].To != "Item ENG-1" {
		t.Errorf("a restore reads %q at risk %q with %+v (%v)", p.Title, p.Risk, p.Changes, err)
	}

	p, err = preview("POST /artifacts/checks", nil, map[string]any{"title": "Q3 deck"})
	if err != nil || p.Title != "Check an artifact's files" || p.Risk != "" ||
		!strings.Contains(p.Note, "changes nothing") || rows(p)["title"].Kind != wire.ChangeFact {
		t.Errorf("a check reads %q: %q with %+v (%v)", p.Title, p.Note, p.Changes, err)
	}

	p, err = preview("POST /worklet/items", nil, map[string]any{"title": "Ship it"})
	if err != nil || p.Title != "Create item Ship it" || p.ApproveLabel != "Approve and create" ||
		p.Risk != "" || rows(p)["title"].Kind != wire.ChangeAdded {
		t.Errorf("a create reads %q / %q at risk %q with %+v (%v)", p.Title, p.ApproveLabel,
			p.Risk, p.Changes, err)
	}

	_, err = preview("POST /doclet/pages", nil, map[string]any{"title": "Runbook"})
	if !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Errorf("a POST that says nothing of what it does previewed: %v", err)
	}
}

// A CHAT DESTRUCTION SAYS WHAT GOES, AND FROM WHICH ROOM.
//
// Erasing messages, pruning a room and setting its horizon are verbs nothing
// undoes (authz.IrreversibleOf), so each card is destructive, leads with what
// goes and says the assistant cannot undo it, all read from what the change
// sends: an erase counts the messages its body names, each once; a prune says
// the instant its query cuts at, its query shown as sent; a horizon says how
// long the room keeps what it holds, or that it goes back to the company's
// setting; one kept for ever takes nothing and is no destruction, and one
// that states no number of days says only that it changes the horizon. The
// room is named by a read of it through the person's grant ("#general"), in
// neutral words where that read is refused, and never by its id. Mutation:
// title a chat destruction by its path, rate one by its method, count a
// repeated message twice, drop the prune's query, or name the room by its id,
// and a case here fails.
func TestAChatDestructionSaysWhatGoesAndFromWhichRoom(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable([]catalog.Route{
		{Pattern: "GET /chatlet/channels/{channel_id}", Action: authz.ActionChatRead},
		{Pattern: "POST /chatlet/channels/{channel_id}/erase", Action: authz.ActionChatErase,
			Effect: authz.EffectDestroys},
		{Pattern: "POST /chatlet/channels/{channel_id}/prune", Action: authz.ActionChatPrune,
			Effect: authz.EffectDestroys},
		{Pattern: "PUT /chatlet/channels/{channel_id}/retention", Action: authz.ActionChatRetention},
	})
	if err != nil {
		t.Fatal(err)
	}
	change := catalog.GenericTools(table, admin, nil)[2]
	const room = "7f3c2a9e-4b1d-4c8e-9a6f-2d5e8b1c0a47"
	general := resource("/chatlet/channels/"+room, "", map[string]any{
		"channel": map[string]any{"id": room, "kind": "public", "name": "general"}})
	refused := func(context.Context, exec.Request) (exec.Response, error) {
		return exec.Response{Status: http.StatusForbidden, Body: []byte(`{"error":"forbidden"}`)}, nil
	}
	preview := func(read proposal.Read, route string, query, body map[string]any) proposal.Preview {
		t.Helper()
		args := map[string]any{"route": route, "path": map[string]any{"channel_id": room}}
		if query != nil {
			args["query"] = query
		}
		if body != nil {
			args["body"] = body
		}
		p, err := change.Preview(t.Context(), read, wednesday, args)
		if err != nil {
			t.Fatalf("%s: %v", route, err)
		}
		for _, words := range []string{p.Title, p.Summary, p.Note} {
			if strings.Contains(words, room) {
				t.Errorf("%s names the room by its id: %q", route, words)
			}
		}
		return p
	}
	const undone = "This cannot be undone from Ask Crewlet."

	p := preview(general, "POST /chatlet/channels/{channel_id}/erase",
		map[string]any{"confirm": "2"},
		map[string]any{"message_ids": []any{"m1", "m2", "m1"}, "reason": "A key was pasted."})
	if p.Title != "Erase 2 messages from #general" || p.ApproveLabel != "Approve and erase" ||
		p.Risk != wire.RiskDestructive ||
		p.Summary != "This erases 2 messages from #general for good." || p.Note != undone {
		t.Errorf("an erase reads %q / %q at risk %q: %q %q", p.Title, p.ApproveLabel, p.Risk,
			p.Summary, p.Note)
	}
	if ids := rows(p)["message_ids"]; ids.Kind != wire.ChangeFact || len(ids.Items) != 3 {
		t.Errorf("the erase's messages are shown as %+v", ids)
	}
	if confirm := rows(p)["query.confirm"]; confirm.Kind != wire.ChangeFact || confirm.To != "2" {
		t.Errorf("the erase's confirmation is shown as %+v", confirm)
	}
	p = preview(refused, "POST /chatlet/channels/{channel_id}/erase", nil,
		map[string]any{"message_ids": []any{"m1"}, "reason": "A key was pasted."})
	if p.Title != "Erase a message from the room" ||
		p.Summary != "This erases a message from the room for good." {
		t.Errorf("an erase in a room the person may not read reads %q: %q", p.Title, p.Summary)
	}

	p = preview(general, "POST /chatlet/channels/{channel_id}/prune",
		map[string]any{"cutoff": "2031-01-01T00:00:00Z", "confirm": "2031-01-01T00:00:00Z"}, nil)
	if p.Title != "Delete what #general said before 1 January 2031" ||
		p.ApproveLabel != "Approve and delete" || p.Risk != wire.RiskDestructive ||
		p.Summary != "This deletes everything said in #general before 1 January 2031 for good." ||
		p.Note != undone {
		t.Errorf("a prune reads %q / %q at risk %q: %q %q", p.Title, p.ApproveLabel, p.Risk,
			p.Summary, p.Note)
	}
	if cutoff := rows(p)["query.cutoff"]; cutoff.Kind != wire.ChangeFact ||
		cutoff.To != "2031-01-01T00:00:00Z" {
		t.Errorf("the prune's cutoff is shown as %+v", cutoff)
	}

	for _, c := range []struct {
		days           any
		title, summary string
		risk           wire.ProposalRisk
	}{
		{7, "Keep #general's messages for 7 days", "From now on #general keeps messages " +
			"for 7 days, and anything older is deleted for good.", wire.RiskDestructive},
		{nil, "Return #general to the company's setting", "From now on #general keeps " +
			"messages only as long as the company's setting does, and anything older is " +
			"deleted for good.", wire.RiskDestructive},
		{0, "Keep #general's messages for ever", "", ""},
		{"a week", "Change how long #general keeps messages", "This changes how long " +
			"#general keeps messages, and what it no longer keeps is deleted for good.",
			wire.RiskDestructive},
	} {
		p = preview(general, "PUT /chatlet/channels/{channel_id}/retention", nil,
			map[string]any{"retention_days": c.days})
		if p.Title != c.title || p.Summary != c.summary || p.Risk != c.risk {
			t.Errorf("a horizon of %v reads %q: %q at risk %q", c.days, p.Title, p.Summary, p.Risk)
		}
		if days := rows(p)["retention_days"]; c.days == nil && days.To != "Cleared" {
			t.Errorf("a horizon taken back shows its field as %+v", days)
		}
	}
}

// proposalOf is the proposal row a preview would be recorded as, for RequestOf.
func proposalOf(p proposal.Preview) assistantdomain.Proposal {
	return assistantdomain.Proposal{Method: p.Request.Method, Path: p.Request.Path,
		Query: p.Request.Query, Body: p.Request.Body, SourceRevision: p.SourceRevision}
}

// DESCRIBE_API LISTS WHAT THE PERSON MAY USE AND DOCUMENTS ONE ROUTE.
//
// The list is a filter on what is listed, never authority: a tracker-only
// person is not listed the company document's read (admin's), and is listed
// the tracker's; an administrator is listed the
// document's, each saying whether it is a change the person approves and
// whether it asks a step-up. One route's documentation is the reference's own
// text for it. A route the table does not hold is refused. Mutation: list
// every route to everybody, or answer a route's documentation from another
// route's row, and this fails.
func TestDescribeAPIListsWhatThePersonMayUse(t *testing.T) {
	t.Parallel()
	tracker := iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet}}
	docs, _, _ := generic(t, tracker)
	listing, err := docs.Docs(catalog.Args{})
	if err != nil || strings.Contains(listing, "GET /config (") ||
		!strings.Contains(listing, "GET /worklet/projects/{key} (reads") {
		t.Errorf("the tracker-only person is listed:\n%s (%v)", listing, err)
	}
	docs, _, _ = generic(t, admin)
	listing, _ = docs.Docs(catalog.Args{})
	if !strings.Contains(listing, "PATCH /config (a change the person approves, decided by "+
		"config.org.write, asks the person to confirm it is them)") {
		t.Errorf("the administrator is listed:\n%s", listing)
	}
	one, err := docs.Docs(catalog.Args{"route": "PATCH /config"})
	if err != nil || !strings.Contains(one, "Merge Patch") {
		t.Errorf("PATCH /config's documentation is %q (%v)", one, err)
	}
	if _, err := docs.Docs(catalog.Args{"route": "GET /nowhere"}); !errors.Is(err,
		catalog.ErrInvalidArguments) {
		t.Errorf("a route the table does not hold was documented: %v", err)
	}
	if listed := strings.Count(listing, "\n"); listed < 8 {
		t.Errorf("the administrator's list holds %d lines", listed)
	}
}

// A GENERIC READ IS HANDED TO THE MODEL WHOLE, REDACTED AND FENCED.
//
// The model is handed the answer itself, nested fields included, every string
// through redact.Secrets, fenced under the turn's boundary with the API's
// source, and cut, fence included, within what one result holds; the person is
// shown a table drawn from the answer's own shape. Mutation: hand the model
// the card's summary, or skip the redaction, and this fails.
func TestAGenericReadIsHandedToTheModelWhole(t *testing.T) {
	t.Parallel()
	_, read, _ := generic(t, admin)
	secret := "sk-ant-api03-" + strings.Repeat("x", 40)
	answer, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"key": "ENG-1", "title": "Ship", "meta": map[string]any{"token": secret}},
		map[string]any{"key": "ENG-2", "title": "Test", "meta": map[string]any{"depth": 2}},
	}})
	args := catalog.Args{"route": "GET /worklet/{id}", "path": map[string]any{"id": "ENG"}}
	b := provenance.NewBoundary()
	shaped, err := catalog.Shape(read, args, exec.Response{Status: 200, Body: answer}, b, wednesday, cards.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shaped.Model, secret) || !strings.Contains(shaped.Model, "depth") ||
		!strings.Contains(shaped.Model, string(b)) {
		t.Errorf("the model was handed %s", shaped.Model)
	}
	if len(shaped.Cards) == 0 || shaped.Cards[0].Table == nil ||
		!slices.ContainsFunc(shaped.Cards[0].Table.Columns, func(c wire.Column) bool {
			return c.Key == "key"
		}) {
		t.Errorf("the person was shown %+v", shaped.Cards)
	}
	big := `{"text":"` + strings.Repeat("a", 64<<10) + `"}`
	shaped, err = catalog.Shape(read, args, exec.Response{Status: 200, Body: []byte(big)}, b,
		wednesday, cards.Options{})
	if err != nil || len(shaped.Model) > 16<<10 || !strings.Contains(shaped.Model, "cut here") {
		t.Errorf("a long answer was handed as %d bytes (%v)", len(shaped.Model), err)
	}
}

// A HUMAN-ONLY GESTURE IS REFUSED IN THE PERSON'S OWN WORDS, AND A BODY PAST
// THE CAP IS REFUSED BEFORE ANYTHING IS SENT.
//
// A call naming a route whose verb is on the human-only list is refused with
// the sentence the list gives its family, which says why it stays with the
// person and where they do it, and names no internal verb; a change whose body
// is past GenericBodyBytes is refused naming the bound. Mutation: answer a
// kept route as an unknown one, or drop the cap, and this fails.
func TestAHumanOnlyGestureIsRefusedInThePersonsWords(t *testing.T) {
	t.Parallel()
	_, _, change := generic(t, admin)
	_, err := change.Request(map[string]any{"route": "POST /iam/invitations",
		"body": map[string]any{"email": "new@example.com"}})
	if err == nil || !strings.Contains(err.Error(), "hands out a way to sign in") ||
		!strings.Contains(err.Error(), "People in Settings") ||
		strings.Contains(err.Error(), string(authz.ActionSignInWrite)) {
		t.Errorf("an invitation was refused with %v", err)
	}
	big := strings.Repeat("a", catalog.GenericBodyBytes)
	if _, err := change.Request(map[string]any{"route": "POST /worklet/items",
		"body": map[string]any{"title": big}}); err == nil ||
		!strings.Contains(err.Error(), "65536") {
		t.Errorf("a body past the cap was built: %v", err)
	}
}

// A GENERIC CHANGE TO NOTHING IS A CREATE OR NO CARD AT ALL.
//
// The preview's own read decides the precondition. A PUT whose read answers
// 404 is a create: bound to that absence, every field drawn as added, and sent
// under `If-None-Match: *`, the API's one spelling of "add this", never under
// an If-Match. Its approval re-reads the path, and a path somebody filled since
// has moved. A PATCH of nothing, and a change whose read is refused, cannot
// apply: no preview is made, and the refusal is the route's own, for the step
// and the model. Mutation: draw the 404 as an edit with no precondition (the
// shape that sent a role's create as a plain PUT, refused 404 after the person
// approved it), or preview a PATCH of nothing, and a case here fails.
func TestAGenericChangeToNothingIsACreateOrNoCard(t *testing.T) {
	t.Parallel()
	_, _, change := generic(t, admin)
	nothing := resource("/elsewhere", "", nil)
	unit := map[string]any{"name": "linear", "command": "linear-mcp"}
	p, err := change.Preview(t.Context(), nothing, wednesday, map[string]any{
		"route": "PUT /config/mcp-servers/{id}", "path": map[string]any{"id": "linear"},
		"body": unit})
	if err != nil {
		t.Fatal(err)
	}
	want := proposal.Revision{Kind: proposal.RevisionCreate, ID: "/config/mcp-servers/linear",
		Version: proposal.VersionAbsent}.String()
	if p.SourceRevision != want || p.Title != "Create MCP server linear" {
		t.Errorf("the create is %q bound to %q, want a create bound to %q", p.Title,
			p.SourceRevision, want)
	}
	got := rows(p)
	for _, field := range []string{"name", "command"} {
		if got[field].Kind != wire.ChangeAdded || got[field].From != "" {
			t.Errorf("row %s is %+v, want it added", field, got[field])
		}
	}
	req := proposal.RequestOf(proposalOf(p))
	if req.IfNoneMatch != exec.AnyRepresentation || req.IfMatch != "" {
		t.Errorf("the create would be sent under If-Match %q and If-None-Match %q",
			req.IfMatch, req.IfNoneMatch)
	}
	if now, err := catalog.Revision(t.Context(), nothing, p.SourceRevision); err != nil ||
		now != p.SourceRevision {
		t.Errorf("a path still empty re-read as %q (%v), want the create's own", now, err)
	}
	filled := resource("/config/mcp-servers/linear", `"rev-9"`, unit)
	if now, err := catalog.Revision(t.Context(), filled, p.SourceRevision); err != nil ||
		now == p.SourceRevision {
		t.Errorf("a path filled since re-read as %q (%v), the create's own", now, err)
	}

	// A PUT OF NOTHING ON A ROUTE THAT DOES NOT CREATE BY ADDRESS, a PUT on
	// a route that declares nothing, and the person would approve a write
	// the API is certain to refuse: no card, and the step says where one is
	// added, in the route's own words.
	for _, c := range []struct{ route, wildcard, hint string }{
		{"PUT /config/units/{id}", "id", unitsElsewhere},
		{"PUT /worklet/projects/{key}", "key", "does not say it creates what it names"},
	} {
		route, hint := c.route, c.hint
		_, err := change.Preview(t.Context(), nothing, wednesday, map[string]any{
			"route": route, "path": map[string]any{c.wildcard: "research"},
			"body": map[string]any{"name": "Research"}})
		var refused *catalog.PreviewRefusal
		if !errors.As(err, &refused) || refused.Refusal.Code != "not_creatable" ||
			!strings.Contains(refused.Refusal.Hint, hint) {
			t.Errorf("%s of nothing answered %v, want not_creatable hinting %q", route, err, hint)
		}
	}

	// A PATCH OF NOTHING, and a read the route refuses: no card.
	for name, read := range map[string]proposal.Read{
		"nothing to patch": nothing,
		"refused": func(context.Context, exec.Request) (exec.Response, error) {
			return exec.Response{Status: http.StatusForbidden,
				Body: []byte(`{"error":"forbidden","message":"Not yours."}`)}, nil
		},
	} {
		_, err := change.Preview(t.Context(), read, wednesday, map[string]any{
			"route": "PATCH /config", "body": map[string]any{"name": "Acme"}})
		var refused *catalog.PreviewRefusal
		if !errors.As(err, &refused) || !errors.Is(err, catalog.ErrPreviewRead) ||
			refused.Refusal.Code == "" {
			t.Errorf("%s: the preview answered %v, want the route's refusal", name, err)
		}
	}
}

// THE REQUEST IS AN ADMINISTRATOR'S TO READ. An approver who administers the
// company is shown the exact request behind "Technical details"; anybody else
// reviews the rows, which are built from the same request, and is sent no
// method, path or body at all. Mutation: send the technical details to every
// approver, and this fails.
func TestTheRequestIsAnAdministratorsToRead(t *testing.T) {
	t.Parallel()
	read := resource("/worklet/projects/ENG", `"rev-2"`, map[string]any{"key": "ENG",
		"name": "Engine"})
	args := map[string]any{"route": "PUT /worklet/projects/{key}",
		"path": map[string]any{"key": "ENG"}, "body": map[string]any{"key": "ENG",
			"name": "Engine room"}}
	for _, c := range []struct {
		who       string
		person    iam.Principal
		technical bool
	}{
		{"an administrator", admin, true},
		{"a project lead", iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive,
			Grants: []iam.Grant{iam.GrantWorklet}}, false},
	} {
		_, _, change := generic(t, c.person)
		p, err := change.Preview(t.Context(), read, wednesday, args)
		if err != nil {
			t.Fatalf("%s: %v", c.who, err)
		}
		if (p.Technical != nil) != c.technical {
			t.Errorf("%s is shown the technical details %+v", c.who, p.Technical)
		}
		if p.Title != "Change project Engine room" {
			t.Errorf("%s reads the title %q", c.who, p.Title)
		}
	}
}

// A GENERIC LIST READ IS ONE PAGE: a route the reference says pages by `limit`
// is asked for GenericPage rows when the model names no limit, so a bare read
// of the event log or the turns is answered whole with its cursor to read on
// rather than cut at the step's bound and lost; a limit the model names is
// kept, and a route that takes none is sent as asked. A SEAT'S READ IS THE
// SEAT ALONE: the route answers its call history by default, for the
// dashboard, so the assistant's read asks for none of it unless the model
// names history itself. Mutation: drop either default and the bare reads
// carry no bound.
func TestAGenericListReadIsOnePage(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable([]catalog.Route{
		{Pattern: "GET /events", Action: authz.ActionWorkRead},
		{Pattern: "GET /turns", Action: authz.ActionWorkRead},
		{Pattern: "GET /config", Action: authz.ActionWorkRead},
		{Pattern: "GET /agents/{id}", Action: authz.ActionWorkRead},
		// A WRITE TOO, since the generic tools are built as a set and the
		// change tool needs a route to name.
		{Pattern: "POST /worklet/items", Action: authz.ActionWorkCreate},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := catalog.GenericTools(table, admin, nil)[1]
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"route": "GET /events"}, "limit=25"},
		{map[string]any{"route": "GET /turns", "query": map[string]any{"seat": "qa"}}, "limit=25&seat=qa"},
		{map[string]any{"route": "GET /events", "query": map[string]any{"limit": "5"}}, "limit=5"},
		{map[string]any{"route": "GET /config"}, ""},
		{map[string]any{"route": "GET /agents/{id}", "path": map[string]any{"id": "qa"}}, "history=false"},
		{map[string]any{"route": "HEAD /agents/{id}", "path": map[string]any{"id": "qa"}}, "history=false"},
		{map[string]any{"route": "GET /agents/{id}", "path": map[string]any{"id": "qa"},
			"query": map[string]any{"history": "true"}}, "history=true"},
	} {
		req, err := read.Request(c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := req.Query.Encode(); got != c.want {
			t.Errorf("%v asked %q, want %q", c.args, got, c.want)
		}
	}
}
