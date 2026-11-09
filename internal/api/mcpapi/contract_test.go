package mcpapi_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WIZARD READS WHAT THE ROUTES SEND. Settings › MCP & tools adds and edits
// a server from the catalogue (`GET /mcp-catalogue`), tests it
// (`POST /mcp-servers/{name}/check`, `POST /mcp-checks`), checks an address
// (`POST /mcp-probe`) and stores a value (`PUT /mcp-servers/{name}/values/{key}`),
// and reads each answer as `contract/mcp.ts` declares it. Each gate below
// holds a declaration both ways against a real answer: every member it
// declares is sent (required ones by every row), and every key sent is
// declared, so a field added or dropped here fails until the client says so.

// holdDeclared holds rows to the interface iface declares; open allows keys
// the declaration leaves out (an error envelope's own).
func holdDeclared(t *testing.T, iface string, rows []map[string]any, open bool) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("the answer carried no %s, so this proves nothing about it", iface)
	}
	members, err := clientsource.Interface(clientsource.Tree(t), iface)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	declared := map[string]bool{}
	for _, m := range members {
		declared[m.Name] = !m.Optional
	}
	sent := map[string]bool{}
	for _, row := range rows {
		for key := range row {
			sent[key] = true
		}
	}
	for _, field := range slices.Sorted(maps.Keys(declared)) {
		if !sent[field] {
			t.Errorf("%s declares %s and no answer sends it", iface, field)
			continue
		}
		for _, row := range rows {
			if _, ok := row[field]; declared[field] && !ok {
				t.Errorf("%s declares %s as required and an answer leaves it out: %v", iface, field, row)
			}
		}
	}
	if open {
		return
	}
	for _, field := range slices.Sorted(maps.Keys(sent)) {
		if _, ok := declared[field]; !ok {
			t.Errorf("the answer sends %s on a %s and the dashboard does not declare it", field, iface)
		}
	}
}

// holdUnion holds a union the dashboard declares to the engine's values.
func holdUnion[S ~string](t *testing.T, union string, want []S) {
	t.Helper()
	got, err := clientsource.Union(clientsource.Tree(t), union)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	words := make([]string, 0, len(want))
	for _, w := range want {
		words = append(words, string(w))
	}
	if slices.Sort(got); !slices.Equal(got, slices.Sorted(slices.Values(words))) {
		t.Errorf("%s declares %v, the engine sends %v", union, got, words)
	}
}

// objects is a JSON list of objects as rows; one object is a list of one.
func objects(t *testing.T, v any) []map[string]any {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		return []map[string]any{v}
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("a list item is %T, not an object", item)
			}
			out = append(out, m)
		}
		return out
	}
	t.Fatalf("%T is not an object or a list of them", v)
	return nil
}

// asJSON is v as the routes write it.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Mutation: drop `left_out` from McpCatalogueAnswer, or `from_integration`
// from McpCatalogueInput, and this is red.
func TestTheWizardReadsWhatTheCatalogueSends(t *testing.T) {
	t.Parallel()
	yes := true
	updated := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	listing := catalogue.Entry{ID: "io.github.x/browser", Name: "browser", Title: "Browser",
		Description: "Drives a browser.", Transport: catalogue.TransportStdio, Command: "npx",
		Args: []string{"-y", "browser@1.0.0"}, Headers: []catalogue.Input{},
		Env: []catalogue.Input{{Name: "BROWSER_KEY", Secret: true}},
		ToolAnnotations: map[string]catalogue.Hints{"open": {ReadOnly: &yes, Destructive: &yes, Idempotent: &yes,
			OpenWorld: &yes}},
		Auth: []catalogue.AuthMethod{catalogue.AuthNone}, Package: "browser", Release: "1.0.0", Community: true,
		Publisher: "io.github.x", Version: "1.0.0", UpdatedAt: &updated}
	svc := surface(t, registryURL, &fakeRegistry{found: catalogue.Found{Entries: []catalogue.Entry{listing}}})
	code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-catalogue?q=browser")
	if code != http.StatusOK {
		t.Fatalf("the catalogue answered %d %v", code, body)
	}
	holdDeclared(t, "McpCatalogueAnswer", []map[string]any{body}, false)
	entries := append(objects(t, body["curated"]), objects(t, body["community"])...)
	holdDeclared(t, "McpCatalogueEntry", entries, false)
	var inputs, hints []map[string]any
	for _, e := range entries {
		inputs = append(inputs, append(objects(t, e["headers"]), objects(t, e["env"])...)...)
		for _, h := range e["tool_annotations"].(map[string]any) {
			hints = append(hints, h.(map[string]any))
		}
	}
	holdDeclared(t, "McpCatalogueInput", inputs, false)
	holdDeclared(t, "McpCatalogueHints", hints, false)
	holdUnion(t, "McpAuthMethod", catalogue.AuthMethods)
	holdUnion(t, "McpOAuthClient", catalogue.OAuthClients)
}

// Mutation: drop `starting_level` from McpCheckedTool, `warnings` from
// McpCheckAnswer or `pkce` from McpProbeOAuth, and this is red.
func TestTheTestReadsWhatTheCheckSends(t *testing.T) {
	t.Parallel()
	svc := checkSurface(t, &fakeChecker{}, &fakeDrafts{}, &recordedAudit{})
	fresh := proved(time.Minute)
	var checks []map[string]any
	for _, ask := range []struct{ path, body string }{
		{"/mcp-servers/tracker/check", ""},
		{"/mcp-checks", `{"entity":{"name":"wiki","command":"wiki-mcp"}}`},
	} {
		code, body := send(t, svc, fresh, ask.path, ask.body)
		if code != http.StatusOK {
			t.Fatalf("%s answered %d %v", ask.path, code, body)
		}
		checks = append(checks, body)
	}
	holdDeclared(t, "McpCheckAnswer", checks, false)
	var tools []map[string]any
	for _, c := range checks {
		tools = append(tools, objects(t, c["tools"])...)
	}
	holdDeclared(t, "McpCheckedTool", tools, false)
	// A TOOL'S STARTING LEVEL is a level like any other ([mcp.Level]), read
	// in the one union the server's page reads its tools' levels in.
	holdUnion(t, "McpToolLevel", mcp.AllLevels)

	code, probe := send(t, svc, fresh, "/mcp-probe", `{"url":"https://mcp.example.com/mcp"}`)
	if code != http.StatusOK {
		t.Fatalf("the probe answered %d %v", code, probe)
	}
	holdDeclared(t, "McpProbeAnswer", []map[string]any{probe}, false)
	holdDeclared(t, "McpProbeOAuth", objects(t, probe["oauth"]), false)
}

// Mutation: drop `pointed` from McpValueAnswer, `deferred` from
// McpValueReload or `stage` from McpValueRefusal, and this is red.
func TestTheReplaceValueReadsWhatTheRouteSends(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{}
	svc := valueSurface(t, checker, newDocuments(t), valueStore(t), &recordedAudit{})
	admin := proved(time.Minute)

	code, raw := put(t, svc, admin, "/mcp-servers/sentry/values/Authorization", valueBody(theValue))
	stored := decoded(t, raw)
	if code != http.StatusOK {
		t.Fatalf("a value that works = %d %v", code, stored)
	}
	holdDeclared(t, "McpValueAnswer", []map[string]any{stored}, false)
	holdDeclared(t, "McpValueReload", []map[string]any{asJSON(t, configapi.Reloaded{RevisionID: "rev-2",
		Epoch: 2, Error: "unavailable", Detail: "the fleet did not answer", Deferred: true})}, false)

	checker.fail = &mcp.Failure{Class: mcp.FailCredentialRejected, Said: "the server answered 401"}
	code, raw = put(t, svc, admin, "/mcp-servers/sentry/values/Authorization", valueBody(theValue))
	refused := decoded(t, raw)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("a value the server refused = %d %v", code, refused)
	}
	// EVERY KEY BUT THE ENVELOPE'S OWN: `error` and `message` every refusal
	// carries, and `problems`, the configuration's problem list the page reads
	// as every configuration refusal's. The rest is this refusal's, held closed.
	for _, envelope := range []string{"error", "message", "problems"} {
		if _, ok := refused[envelope]; !ok {
			t.Fatalf("the refusal carries no %s: %v", envelope, refused)
		}
		delete(refused, envelope)
	}
	holdDeclared(t, "McpValueRefusal", []map[string]any{refused}, false)
}

// THE HELD CALL'S ANSWER READS WHAT THE ROUTE SENDS. The Inbox, Home and a
// server's page answer a held call through `POST /mcp-approvals/{id}` and read
// its answer as `McpApprovalAnswer` (whether the answer reached the run, from
// `outcome`). Held both ways over an answer to once and to always (whose
// `revision` is the configuration's own answer), and the outcome union to the
// two the route sends.
//
// Mutation: drop `answer_id` from McpApprovalAnswer, or `unknown` from
// McpApprovalOutcome, and this is red.
func TestTheApprovalReadsWhatTheRouteSends(t *testing.T) {
	t.Parallel()
	var rows []map[string]any
	for _, c := range []struct {
		answer string
		as     iam.Principal
	}{
		{sandbox.HeldOnce, proved(time.Minute)},
		{sandbox.HeldAlways, proved(time.Minute)},
	} {
		desk := &fakeApprovals{run: waitingRun()}
		svc := approvalSurface(t, desk, &fakeToolDocuments{}, &recordedAudit{})
		code, body := answer(t, svc, c.as, "/mcp-approvals/"+heldID, `{"answer": "`+c.answer+`"}`)
		if code != http.StatusAccepted {
			t.Fatalf("POST %s = %d %v", c.answer, code, body)
		}
		rows = append(rows, body)
	}
	holdDeclared(t, "McpApprovalAnswer", rows, false)
	holdUnion(t, "McpApprovalOutcome", []statelog.Outcome{statelog.OutcomePending, statelog.OutcomeUnknown})
}

// THE REVIEW READS WHAT THE HELD CALL'S READ SENDS. Review and allow once
// reads an operate call whole through `GET /mcp-approvals/{id}` and reads it
// as `McpHeldWhole`, held both ways over the answer as the route writes it.
//
// Mutation: drop `arguments_sha256` from McpHeldWhole, and this is red.
func TestTheReviewReadsWhatTheHeldCallReadSends(t *testing.T) {
	t.Parallel()
	seal := operateSeal(t)
	svc := operateSurface(t, &fakeApprovals{run: operateRun(t, seal)}, seal)
	code, held := read(t, svc, seated("ops-lead", time.Minute), "/mcp-approvals/"+heldID)
	if code != http.StatusOK {
		t.Fatalf("the named approver reads %d %v", code, held)
	}
	holdDeclared(t, "McpHeldWhole", []map[string]any{held}, false)
}

// THE REVIEW READS WHAT THE ROUTE SENDS. The Drift dialog decides a server's
// changed tools through `POST /mcp-servers/{name}/changes` and reads its
// answer as `McpChangesAnswer` (`moved`, the tools somebody else decided
// meanwhile). Held both ways over the answer as the route writes it, a block
// with its revision and an acceptance without one.
//
// Mutation: drop `moved` from McpChangesAnswer, and this is red.
func TestTheReviewReadsWhatTheRouteSends(t *testing.T) {
	t.Parallel()
	rows := []map[string]any{
		asJSON(t, mcpapi.ChangesAnswer{Server: "tracker", Accepted: []string{"close_issue"},
			Blocked: []string{}, Moved: []string{}}),
		asJSON(t, mcpapi.ChangesAnswer{Server: "tracker", Accepted: []string{}, Blocked: []string{"drop_issue"},
			Moved: []string{"close_issue"}, Revision: map[string]any{"revision_id": "r2", "epoch": 3}}),
	}
	holdDeclared(t, "McpChangesAnswer", rows, false)
}

// THE SIGN IN AND REVOKE DIALOGS READ WHAT THE SIGN-IN ROUTES SEND: the start
// (`POST /mcp-servers/{name}/oauth`), the read the dialog polls
// (`GET /mcp-sign-ins/{id}`) and the sign-out (`DELETE`), each as declared, and
// a sign-in's status in the engine's own words. Mutation: drop
// `authorize_url` from McpSignIn or `said` from McpSignOut, or declare a status
// the engine never sends, and this is red.
func TestTheSignInReadsWhatTheRoutesSend(t *testing.T) {
	t.Parallel()
	svc := signInSurface(t, &fakeSignIns{}, &recordedAudit{})
	fresh := proved(time.Minute)
	code, started := call(t, svc, fresh, http.MethodPost, "/mcp-servers/linear/oauth")
	if code != http.StatusCreated {
		t.Fatalf("the start answered %d %v", code, started)
	}
	code, read := call(t, svc, fresh, http.MethodGet, "/mcp-sign-ins/"+signInID.String())
	if code != http.StatusOK {
		t.Fatalf("the read answered %d %v", code, read)
	}
	holdDeclared(t, "McpSignIn", []map[string]any{started, read}, false)
	holdUnion(t, "McpSignInStatus", coord.MCPSignInStatuses)
	code, ended := call(t, svc, fresh, http.MethodDelete, "/mcp-servers/linear/oauth")
	if code != http.StatusOK {
		t.Fatalf("the sign-out answered %d %v", code, ended)
	}
	holdDeclared(t, "McpSignOut", []map[string]any{ended}, false)
}
