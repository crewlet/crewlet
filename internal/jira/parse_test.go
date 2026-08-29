package jira_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The instance: three agent seats with account ids, a human seat reachable
// only by email, and a stranger who is a Jira user and not in the org chart.
const (
	acctLead     = "712020:aaaaaaaa-0000-0000-0000-000000000001"
	acctSWE      = "712020:bbbbbbbb-0000-0000-0000-000000000002"
	acctQA       = "712020:cccccccc-0000-0000-0000-000000000003"
	acctStranger = "712020:dddddddd-0000-0000-0000-000000000009"
	acctFounder  = "712020:eeeeeeee-0000-0000-0000-000000000010"
	founderMail  = "founder@example.com"
)

func registry(t *testing.T) *notify.Registry {
	t.Helper()
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "Eng Lead", DeclaredHandle: "lead"},
			{Name: "SWE", DeclaredHandle: "swe"},
			{Name: "QA", DeclaredHandle: "qa"},
			{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
				Email:   founderMail,
				Contact: &org.HumanContact{MattermostUserID: "founder"}},
		},
	}
	o.Normalize()
	reg := notify.NewRegistry(o)
	for id, handle := range map[string]string{
		acctLead: "lead", acctSWE: "swe", acctQA: "qa",
	} {
		if err := reg.Register(jira.Backend, id, handle); err != nil {
			t.Fatalf("register %s: %v", handle, err)
		}
	}
	return reg
}

// watchers answers the org-credential watcher lookup.
type watchers struct {
	of  map[string][]string
	err error
}

func (w watchers) Of(_ context.Context, issueKey string) ([]string, error) {
	if w.err != nil {
		return nil, w.err
	}
	return w.of[issueKey], nil
}

func parser(t *testing.T, mutate func(*jira.ParserOptions)) *jira.Parser {
	t.Helper()
	opts := jira.ParserOptions{
		URL:      "https://jira.example.com",
		Watchers: watchers{of: map[string][]string{}},
		Leads:    map[string]string{"ENG": "lead"},
	}
	if mutate != nil {
		mutate(&opts)
	}
	return jira.NewParser(opts)
}

// issue builds a webhook body. The shape is Jira Data Center's, which is
// also what the Forge relay normalises a Cloud event into.
func issue(event, actor string, fields, extra map[string]any) types.RawWebhook {
	all := map[string]any{
		"summary": "Fix the login redirect",
		"project": map[string]any{"key": "ENG", "name": "Engineering"},
	}
	for k, v := range fields {
		all[k] = v
	}
	body := map[string]any{
		"webhookEvent": event,
		"timestamp":    float64(1_718_000_000_000),
		"user":         map[string]any{"accountId": actor, "displayName": "Ana"},
		"issue": map[string]any{
			"id": "10001", "key": "ENG-42", "fields": all,
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	return types.RawWebhook{Body: body}
}

func person(account string) map[string]any {
	return map[string]any{"accountId": account, "displayName": "Somebody"}
}

// route runs one payload and returns the copies, by handle-or-external-id.
func route(t *testing.T, p *jira.Parser, reg *notify.Registry, w types.RawWebhook) []notify.Routed {
	t.Helper()
	out, err := p.Parse(context.Background(), w, reg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return out
}

func vias(copies []notify.Routed) map[string]string {
	out := map[string]string{}
	for _, c := range copies {
		key := c.To.Handle
		if key == "" && len(c.To.ExternalIDs) > 0 {
			key = c.To.ExternalIDs[0]
		}
		out[key] = c.Metadata[jira.RoutedViaField]
	}
	return out
}

// THE ASSIGNEE IS WOKEN, AND THE ACTOR IS NOT.
func TestAnAssigneeIsWokenAndTheActorIsNot(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	copies := route(t, parser(t, nil), reg,
		issue("jira:issue_updated", acctLead, map[string]any{"assignee": person(acctSWE)}, nil))

	got := vias(copies)
	if got[acctSWE] != jira.ViaAssignee {
		t.Fatalf("the assignee was not woken as one: %v", got)
	}
	if _, woken := got[acctLead]; woken {
		t.Errorf("the actor was woken by their own edit: %v", got)
	}
}

// A MENTION OUTRANKS EVERY OTHER REASON FOR THE SAME PERSON.
//
// Jira adds a mentioned user to the watcher list, so both reasons are true on
// nearly every comment — and the prompt renders them differently. A fan-out
// that walked the watchers first would tell a colleague who was asked a
// direct question that they are "watching this issue".
func TestBeingMentionedOutranksWatchingAndAssignment(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	p := parser(t, func(o *jira.ParserOptions) {
		o.Watchers = watchers{of: map[string][]string{"ENG-42": {acctSWE, acctQA}}}
	})
	comment := map[string]any{"comment": map[string]any{
		"id":     "20001",
		"author": person(acctLead),
		"body":   mention(acctSWE, "can you take this"),
	}}
	copies := route(t, p, reg,
		issue("comment_created", acctLead, map[string]any{"assignee": person(acctSWE)}, comment))

	got := vias(copies)
	if got[acctSWE] != jira.ViaMention {
		t.Fatalf("the mentioned seat was woken as %q, not a mention: %v",
			got[acctSWE], got)
	}
	// And exactly once. A person named twice is one notification.
	if len(copies) != 2 || got[acctQA] != jira.ViaWatcher {
		t.Errorf("want one mention and one watcher, got %v", got)
	}
}

// A WATCHER IS THE WEAKEST REASON AND STILL A REAL ONE.
func TestAWatcherIsWokenWhenNobodyElseIsNamed(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	p := parser(t, func(o *jira.ParserOptions) {
		o.Watchers = watchers{of: map[string][]string{"ENG-42": {acctQA}}}
	})
	got := vias(route(t, p, reg, issue("jira:issue_updated", acctLead, nil, nil)))
	if got[acctQA] != jira.ViaWatcher {
		t.Fatalf("the watcher was not woken: %v", got)
	}
	if _, fell := got["lead"]; fell {
		t.Error("the lead fallback fired even though a watcher was reached")
	}
}

// A WATCHER LOOKUP THAT FAILS COSTS THE WATCHERS AND NOTHING ELSE.
//
// The assignee and the mentions are in the payload. Failing the delivery
// because the instance was briefly unreachable would lose all three.
func TestAFailedWatcherLookupStillRoutesWhatThePayloadNames(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	p := parser(t, func(o *jira.ParserOptions) {
		o.Watchers = watchers{err: errors.New("503 from the instance")}
	})
	got := vias(route(t, p, reg,
		issue("jira:issue_updated", acctLead, map[string]any{"assignee": person(acctSWE)}, nil)))
	if got[acctSWE] != jira.ViaAssignee {
		t.Fatalf("the assignee went unrouted because the watcher lookup failed: %v", got)
	}
}

// AN UNASSIGNED TICKET WAKES THE OWNING LEAD, even though its reporter is
// already on the watcher list.
//
// Jira adds the reporter automatically, so "was every target the actor" is
// true for the single most important case in the integration: a founder
// filing an unassigned ticket into a team's project.
func TestAnUnassignedTicketReachesTheProjectLead(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	p := parser(t, func(o *jira.ParserOptions) {
		o.Watchers = watchers{of: map[string][]string{"ENG-42": {acctFounder}}}
	})
	got := vias(route(t, p, reg, issue("jira:issue_created", acctFounder, nil, nil)))
	if got["lead"] != jira.ViaLeadFallback {
		t.Fatalf("an unassigned ticket reached nobody: %v", got)
	}
}

// A SELF-ASSIGNED TICKET STAYS SILENT. Somebody took the work in the open;
// waking their lead to say so is noise.
func TestAnIssueTheActorAssignedToThemselvesWakesNobody(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	copies := route(t, parser(t, nil), reg,
		issue("jira:issue_created", acctFounder, map[string]any{"assignee": person(acctFounder)}, nil))
	if len(copies) != 0 {
		t.Fatalf("want silence, got %v", vias(copies))
	}
}

// A TICKET HANDED ENTIRELY TO OUTSIDERS STILL WAKES THE LEAD.
//
// Nobody here can act on it and nothing else reports it, so the ticket has
// landed nowhere — which is the failure the fallback exists for.
func TestATicketAssignedToAStrangerReachesTheLead(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	got := vias(route(t, parser(t, nil), reg,
		issue("jira:issue_created", acctFounder, map[string]any{"assignee": person(acctStranger)}, nil)))
	if got["lead"] != jira.ViaLeadFallback {
		t.Fatalf("a ticket owned by nobody here reached nobody: %v", got)
	}
	if _, woken := got[acctStranger]; woken {
		t.Error("a copy was addressed to somebody the service cannot resolve")
	}
}

// THE LEAD'S OWN ACTION MUST NOT RE-TRIGGER THE LEAD, or a lead filing a
// ticket in their own project wakes themselves for as long as nobody looks.
func TestTheLeadIsNotWokenByTheirOwnUnassignedTicket(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	copies := route(t, parser(t, nil), reg, issue("jira:issue_created", acctLead, nil, nil))
	if len(copies) != 0 {
		t.Fatalf("the lead woke themselves: %v", vias(copies))
	}
}

// A PROJECT NOBODY LEADS PRODUCES NO GUESS. A misroute teaches a seat that
// work it does not own is its problem.
func TestAnUnmappedProjectRoutesToNobody(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	w := issue("jira:issue_created", acctFounder, map[string]any{
		"project": map[string]any{"key": "OPS", "name": "Operations"},
	}, nil)
	if copies := route(t, parser(t, nil), reg, w); len(copies) != 0 {
		t.Fatalf("want no copies for an unowned project, got %v", vias(copies))
	}
}

// THE HUMAN SEAT IS REACHED BY EMAIL. A person in the org chart holds no
// tracker credential, so nothing ever registers an account id for them — and
// gating the assignee target on the account id alone would leave every human
// seat unreachable.
func TestAHumanAssigneeIsReachedByTheirAddress(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	assignee := map[string]any{
		"accountId": acctFounder, "emailAddress": founderMail, "displayName": "Founder",
	}
	copies := route(t, parser(t, nil), reg,
		issue("jira:issue_updated", acctLead, map[string]any{"assignee": assignee}, nil))
	if len(copies) != 1 {
		t.Fatalf("want one copy, got %v", vias(copies))
	}
	if copies[0].To.Email != founderMail {
		t.Errorf("the copy carries no address to resolve: %+v", copies[0].To)
	}
}

// A HUMAN SEAT'S DECLARED ATLASSIAN ACCOUNT ROUTES LIKE A SEAT'S.
//
// contact.atlassian_account_id is the one identity a person declares for
// both Atlassian products, and the party registry registers it under this
// backend's namespace — so a founder assigned an issue is woken by the
// account id in the payload, with no email round trip.
func TestAHumanSeatsDeclaredAtlassianAccountRoutes(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
			Contact: &org.HumanContact{AtlassianAccountID: acctFounder}},
		{Name: "SWE", DeclaredHandle: "swe"},
	}}
	o.Normalize()
	reg := notify.NewRegistry(o)
	reg.ReconcileHumanContacts(o, func(string) (string, bool) { return "", false })

	got := vias(route(t, parser(t, nil), reg,
		issue("jira:issue_updated", acctSWE, map[string]any{"assignee": person(acctFounder)}, nil)))
	if got[acctFounder] != jira.ViaAssignee {
		t.Fatalf("the human assignee was not woken: %v", got)
	}
}

// A DATA CENTRE PAYLOAD NAMES PEOPLE BY USERNAME, and the same routing has
// to work on it: the two deployments differ in the identity field, not in
// what the event means.
func TestDataCentreUsernamesRouteLikeCloudAccountIds(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "SWE", DeclaredHandle: "swe"},
	}}
	o.Normalize()
	reg := notify.NewRegistry(o)
	if err := reg.Register(jira.Backend, "swe.account", "swe"); err != nil {
		t.Fatal(err)
	}
	w := issue("jira:issue_updated", "ana.account", map[string]any{
		"assignee": map[string]any{"name": "swe.account", "displayName": "SWE"},
	}, nil)
	w.Body["user"] = map[string]any{"name": "ana.account"}

	got := vias(route(t, parser(t, nil), reg, w))
	if got["swe.account"] != jira.ViaAssignee {
		t.Fatalf("a Data Center payload routed nowhere: %v", got)
	}
}

// A COMMENT WEBHOOK CARRIES NO TOP-LEVEL ACTOR, and reading only the top
// level attributes every Cloud comment to nobody — which defeats the
// self-action guard: the seat that wrote the comment gets woken by it.
func TestACommentsAuthorIsTheActor(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	w := issue("comment_created", "", map[string]any{"assignee": person(acctSWE)},
		map[string]any{"comment": map[string]any{
			"author": person(acctSWE), "body": "done",
		}})
	delete(w.Body, "user")

	if copies := route(t, parser(t, nil), reg, w); len(copies) != 0 {
		t.Fatalf("a seat was woken by its own comment: %v", vias(copies))
	}
}

// WORKSPACE BOOKKEEPING NAMES NOBODY. Routing a sprint or a board event
// spends a turn on "somebody made a sprint".
func TestEventsThatConcernNobodyRouteNothing(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	for _, event := range []string{
		"jira:issue_property_set", "sprint_started", "board_created",
		"jira:worklog_updated", "project_created", "",
	} {
		w := issue(event, acctLead, map[string]any{"assignee": person(acctSWE)}, nil)
		if copies := route(t, parser(t, nil), reg, w); len(copies) != 0 {
			t.Errorf("%q produced %d notification(s)", event, len(copies))
		}
	}
}

// A PAYLOAD WITH NO ISSUE ROUTES NOTHING. Every rule downstream rests on the
// issue — the conversation key, the recon pointer, the watcher lookup — so a
// copy without one is a wake with nowhere to look.
func TestAnEventNamingNoIssueRoutesNothing(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	w := issue("jira:issue_updated", acctLead, map[string]any{"assignee": person(acctSWE)}, nil)
	w.Body["issue"] = map[string]any{"fields": map[string]any{}}
	if copies := route(t, parser(t, nil), reg, w); len(copies) != 0 {
		t.Fatalf("want nothing, got %v", vias(copies))
	}
}

// EACH COPY CARRIES ITS OWN REASON. One payload produces several
// notifications, and stamping the shared metadata map would make every copy
// claim whichever reason was written last.
func TestEachCopyKeepsItsOwnRoutingReason(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	p := parser(t, func(o *jira.ParserOptions) {
		o.Watchers = watchers{of: map[string][]string{"ENG-42": {acctQA}}}
	})
	comment := map[string]any{"comment": map[string]any{
		"author": person(acctLead), "body": mention(acctSWE, "look at this"),
	}}
	copies := route(t, p, reg, issue("comment_created", acctLead, nil, comment))
	if len(copies) != 2 {
		t.Fatalf("want two copies, got %d", len(copies))
	}
	reasons := []string{
		copies[0].Metadata[jira.RoutedViaField],
		copies[1].Metadata[jira.RoutedViaField],
	}
	slices.Sort(reasons)
	if !slices.Equal(reasons, []string{jira.ViaMention, jira.ViaWatcher}) {
		t.Fatalf("the copies share a reason: %v", reasons)
	}
}

// THE SHAREABLE LINK IS OMITTED RATHER THAN GUESSED. A base the engine does
// not have produces no link, because one that looks right and opens nothing
// is worse than none.
func TestTheLinkIsOmittedWithoutAShareableBase(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	with := route(t, parser(t, nil), reg,
		issue("jira:issue_updated", acctLead, map[string]any{"assignee": person(acctSWE)}, nil))
	if got := with[0].Metadata["url"]; got != "https://jira.example.com/browse/ENG-42" {
		t.Errorf("url = %q", got)
	}
	bare := parser(t, func(o *jira.ParserOptions) { o.URL = "" })
	without := route(t, bare, reg,
		issue("jira:issue_updated", acctLead, map[string]any{"assignee": person(acctSWE)}, nil))
	if got, present := without[0].Metadata["url"]; present {
		t.Errorf("a link was built with no base: %q", got)
	}
}

// mention builds an ADF comment body naming one account.
func mention(account, text string) map[string]any {
	var node map[string]any
	raw := `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
		{"type":"mention","attrs":{"id":"` + account + `","text":"@SWE"}},
		{"type":"text","text":" ` + text + `"}]}]}`
	if err := json.Unmarshal([]byte(strings.ReplaceAll(raw, "\t", "")), &node); err != nil {
		panic(err)
	}
	return node
}
