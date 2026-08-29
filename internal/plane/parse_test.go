package plane_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plane"
)

// The workspace: two agent seats and a lead, each with a Plane user id, plus
// an outsider who is in the workspace and not in the org chart.
const (
	uidLead     = "11111111-1111-1111-1111-111111111111"
	uidSWE      = "22222222-2222-2222-2222-222222222222"
	uidQA       = "33333333-3333-3333-3333-333333333333"
	uidOutsider = "99999999-9999-9999-9999-999999999999"
)

func registry(t *testing.T) *notify.Registry {
	t.Helper()
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "Eng Lead", DeclaredHandle: "lead"},
			{Name: "SWE", DeclaredHandle: "swe"},
			{Name: "QA", DeclaredHandle: "qa"},
		},
	}
	o.Normalize()
	reg := notify.NewRegistry(o)
	for id, handle := range map[string]string{
		uidLead: "lead", uidSWE: "swe", uidQA: "qa",
	} {
		if err := reg.Register(plane.Backend, id, handle); err != nil {
			t.Fatalf("register %s: %v", handle, err)
		}
	}
	return reg
}

// projects is the id → identifier map, as the transport's cache answers it.
type projects map[string]string

func (p projects) Identifier(_ context.Context, id string) string { return p[id] }

// subscribers answers the work-item subscriber lookup.
type subscribers struct {
	of  map[string][]string
	err error
}

func (s subscribers) Of(_ context.Context, _, issueID string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.of[issueID], nil
}

func parser(t *testing.T, mutate func(*plane.ParserOptions)) *plane.Parser {
	t.Helper()
	opts := plane.ParserOptions{
		URL:         "https://plane.example.com",
		Projects:    projects{"proj-1": "ENG", "proj-skills": "SKILLS"},
		Subscribers: subscribers{of: map[string][]string{}},
		Leads:       map[string]string{"eng": "lead"},
		Excluded:    []string{"skills"},
	}
	if mutate != nil {
		mutate(&opts)
	}
	p, err := plane.NewParser(opts)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	return p
}

func hook(event, action string, data map[string]any, mutate func(map[string]any)) types.RawWebhook {
	body := map[string]any{
		"event": event, "action": action, "workspace_slug": "nimbus",
		"data":     data,
		"activity": map[string]any{"actor_id": uidLead},
	}
	if mutate != nil {
		mutate(body)
	}
	return types.RawWebhook{Body: body}
}

func routeAll(t *testing.T, p *plane.Parser, w types.RawWebhook, reg *notify.Registry) []notify.Routed {
	t.Helper()
	got, err := p.Parse(t.Context(), w, reg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return got
}

// recipients names who was woken, by handle where one was resolved and by
// external id otherwise — the two forms the cascade accepts.
func recipients(t *testing.T, reg *notify.Registry, routed []notify.Routed) []string {
	t.Helper()
	var out []string
	for _, r := range routed {
		switch {
		case r.To.Handle != "":
			out = append(out, r.To.Handle)
		case len(r.To.ExternalIDs) > 0:
			if p, ok := reg.ByExternalID(plane.Backend, r.To.ExternalIDs[0]); ok {
				out = append(out, p.Handle)
			} else {
				out = append(out, r.To.ExternalIDs[0])
			}
		}
	}
	slices.Sort(out)
	return out
}

func vias(routed []notify.Routed) []string {
	var out []string
	for _, r := range routed {
		out = append(out, r.Metadata[plane.RoutedViaField])
	}
	slices.Sort(out)
	return out
}

// Everything a workspace emits that is not one of the four content events —
// a project created, a cycle closed, a module renamed — is bookkeeping about
// the workspace rather than a message to anybody.
func TestOnlyContentEventsAreRouted(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	for _, event := range []string{"project", "cycle", "module", "user", ""} {
		// A NON-LEAD actor, so an event that fell through to the intake
		// route would actually wake the lead — with the lead as actor
		// the self-trigger guard hides the fall-through.
		got := routeAll(t, p, hook(event, "created", map[string]any{
			"project": "proj-1", "name": "Something", "assignees": []any{uidSWE},
		}, func(b map[string]any) {
			b["activity"] = map[string]any{"actor_id": uidSWE}
		}), reg)
		if len(got) != 0 {
			t.Errorf("%q produced %d notifications", event, len(got))
		}
	}
	// And a content event with no data object is nothing at all.
	w := hook("issue", "created", nil, func(b map[string]any) { b["data"] = "not an object" })
	if got := routeAll(t, p, w, reg); len(got) != 0 {
		t.Fatalf("a malformed payload produced %d notifications", len(got))
	}
}

func TestANewTicketWakesItsAssignees(t *testing.T) {
	p, reg := parser(t, nil), registry(t)

	got := routeAll(t, p, hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Fix the login redirect",
		"sequence_id": float64(42), "assignees": []any{uidSWE, uidQA},
		"description_html": "<p>It bounces.</p>",
	}, nil), reg)

	if want := []string{"qa", "swe"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v, want %v", recipients(t, reg, got), want)
	}
	if want := []string{plane.ViaAssignee, plane.ViaAssignee}; !slices.Equal(vias(got), want) {
		t.Fatalf("routed via %v", vias(got))
	}
	// The work item's key leads the subject — the line somebody can act
	// on without opening anything.
	if got[0].Subject != "[ENG-42] Fix the login redirect" {
		t.Fatalf("subject = %q", got[0].Subject)
	}
	if got[0].Body != "It bounces." {
		t.Fatalf("body = %q", got[0].Body)
	}
	if url := got[0].Metadata["url"]; url != "https://plane.example.com/nimbus/projects/proj-1/issues/issue-1" {
		t.Fatalf("url = %q", url)
	}
	if got[0].Metadata[notify.ActorField] != uidLead {
		t.Fatalf("the actor is %q", got[0].Metadata[notify.ActorField])
	}
}

// A NEW TICKET MUST NEVER VANISH: with nobody named, the owning unit's lead
// gets it.
func TestAnUnassignedTicketWakesTheLead(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	w := hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Something broke",
	}, func(b map[string]any) {
		b["activity"] = map[string]any{"actor_id": uidSWE}
	})

	got := routeAll(t, p, w, reg)
	if want := []string{"lead"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v, want the lead", recipients(t, reg, got))
	}
	if got[0].Metadata[plane.RoutedViaField] != plane.ViaLeadFallback {
		t.Fatalf("routed via %q", got[0].Metadata[plane.RoutedViaField])
	}
}

// Handed entirely to people outside the org chart, a new ticket STILL wakes
// the lead — otherwise it vanishes. But a purely SELF-assigned create stays
// silent: the actor claimed the work and knows they have it.
func TestATicketAssignedOnlyToOutsidersStillWakesTheLead(t *testing.T) {
	p, reg := parser(t, nil), registry(t)

	outsiders := hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "For a person",
		"assignees": []any{uidOutsider},
	}, func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} })
	got := routeAll(t, p, outsiders, reg)
	if want := []string{"lead"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v, want the lead", recipients(t, reg, got))
	}

	selfAssigned := hook("issue", "created", map[string]any{
		"id": "issue-2", "project": "proj-1", "name": "Mine",
		"assignees": []any{uidSWE},
	}, func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} })
	if got := routeAll(t, p, selfAssigned, reg); len(got) != 0 {
		t.Fatalf("a self-assigned create woke %v", recipients(t, reg, got))
	}
}

// Being PUT ON something is the moment it becomes yours. Being taken off is
// not a task, so a removal wakes nobody.
func TestAnAssignmentChangeWakesOnlyTheNewAssignee(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	assign := func(newID string) types.RawWebhook {
		return hook("issue", "updated", map[string]any{
			"id": "issue-1", "project": "proj-1", "name": "Work",
			"assignees": []any{uidSWE, uidQA},
		}, func(b map[string]any) {
			b["activity"] = map[string]any{
				"actor_id": uidLead, "field": "assignees", "new_identifier": newID,
			}
		})
	}

	got := routeAll(t, p, assign(strings.ToUpper(uidQA)), reg)
	if want := []string{"qa"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v, want only the new assignee", recipients(t, reg, got))
	}
	if got[0].Metadata[plane.RoutedViaField] != plane.ViaAssigneeAdded {
		t.Fatalf("routed via %q", got[0].Metadata[plane.RoutedViaField])
	}
	// A removal carries no new identifier.
	if got := routeAll(t, p, assign(""), reg); len(got) != 0 {
		t.Fatalf("a removal woke %v", recipients(t, reg, got))
	}
}

// Any other field change is thread activity: the people following it.
func TestAFieldChangeWakesTheThread(t *testing.T) {
	p := parser(t, func(o *plane.ParserOptions) {
		o.Subscribers = subscribers{of: map[string][]string{"issue-1": {uidSWE, uidQA}}}
	})
	reg := registry(t)

	got := routeAll(t, p, hook("issue", "updated", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Work",
		"assignees": []any{uidOutsider},
	}, func(b map[string]any) {
		b["activity"] = map[string]any{"actor_id": uidLead, "field": "state"}
	}), reg)

	if want := []string{"qa", "swe"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v, want the subscribers", recipients(t, reg, got))
	}
	if got[0].Metadata[plane.RoutedViaField] != plane.ViaSubscriber {
		t.Fatalf("routed via %q", got[0].Metadata[plane.RoutedViaField])
	}
}

// The fan-out DEGRADES to the payload's assignees whenever the lookup cannot
// answer: no client, a failure, an empty response. An unhearable thread is
// worse than a slightly wider one.
func TestTheSubscriberLookupDegradesToAssignees(t *testing.T) {
	reg := registry(t)
	item := hook("issue", "updated", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Work",
		"assignees": []any{uidSWE},
	}, func(b map[string]any) {
		b["activity"] = map[string]any{"actor_id": uidLead, "field": "state"}
	})

	for name, opts := range map[string]func(*plane.ParserOptions){
		"no client":      func(o *plane.ParserOptions) { o.Subscribers = nil },
		"lookup failed":  func(o *plane.ParserOptions) { o.Subscribers = subscribers{err: errors.New("down")} },
		"empty response": func(o *plane.ParserOptions) { o.Subscribers = subscribers{of: map[string][]string{}} },
		"no usable rows": func(o *plane.ParserOptions) { o.Subscribers = subscribers{of: map[string][]string{"issue-1": {""}}} },
	} {
		got := routeAll(t, parser(t, opts), item, reg)
		if want := []string{"swe"}; !slices.Equal(recipients(t, reg, got), want) {
			t.Errorf("%s: woke %v, want the assignee", name, recipients(t, reg, got))
		}
	}
}

// Being NAMED is a directed ask, and the first reason for a person wins — a
// mentioned subscriber is told they were mentioned, not that they subscribed.
func TestAMentionOutranksASubscription(t *testing.T) {
	p := parser(t, func(o *plane.ParserOptions) {
		o.Subscribers = subscribers{of: map[string][]string{"issue-1": {uidSWE, uidQA}}}
	})
	reg := registry(t)

	got := routeAll(t, p, hook("issue_comment", "created", map[string]any{
		"id": "comment-1", "issue": "issue-1", "project": "proj-1",
		"comment_html": `<p>Hey <mention-component entity_identifier="` + uidSWE +
			`" entity_name="user_mention"></mention-component> can you look?</p>`,
	}, nil), reg)

	if want := []string{"qa", "swe"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v", recipients(t, reg, got))
	}
	byHandle := map[string]string{}
	for _, r := range got {
		p, _ := reg.ByExternalID(plane.Backend, r.To.ExternalIDs[0])
		byHandle[p.Handle] = r.Metadata[plane.RoutedViaField]
	}
	if byHandle["swe"] != plane.ViaMention {
		t.Fatalf("the mentioned seat was routed via %q", byHandle["swe"])
	}
	if byHandle["qa"] != plane.ViaSubscriber {
		t.Fatalf("the subscriber was routed via %q", byHandle["qa"])
	}
	// The mention renders as a NAME: a body reading "@8f2c1e…" tells a
	// seat nothing about who is asking or whether it is them.
	if !strings.Contains(got[0].Body, "@SWE") {
		t.Fatalf("the mention did not render as a name: %q", got[0].Body)
	}
	if got[0].Metadata["mention_ids"] != uidSWE {
		t.Fatalf("mention_ids = %q", got[0].Metadata["mention_ids"])
	}
}

// A comment deletion carries no body, so routing it is pure noise.
func TestACommentDeletionWakesNobody(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	for _, action := range []string{"deleted", "archived", ""} {
		// WITH an assignee, so a routed deletion would wake somebody —
		// a fixture with no recipients cannot tell the guard from its
		// own emptiness.
		got := routeAll(t, p, hook("issue_comment", action, map[string]any{
			"id": "comment-1", "issue": "issue-1", "project": "proj-1",
			"assignees":    []any{uidSWE},
			"comment_html": "<p>gone</p>",
		}, nil), reg)
		if len(got) != 0 {
			t.Errorf("%q woke %v", action, recipients(t, reg, got))
		}
	}
}

// On a comment, `data.id` is the COMMENT's id and the work item is under
// `data.issue`. A comment id in the issue slot produces a link that 404s and
// a subscriber lookup that cannot succeed.
func TestACommentPointsAtItsWorkItemNotItself(t *testing.T) {
	p := parser(t, func(o *plane.ParserOptions) {
		o.Subscribers = subscribers{of: map[string][]string{"issue-1": {uidSWE}}}
	})
	reg := registry(t)

	got := routeAll(t, p, hook("issue_comment", "created", map[string]any{
		"id": "comment-1", "issue": "issue-1", "project": "proj-1",
		"comment_html": "<p>a thought</p>",
	}, nil), reg)
	if len(got) == 0 {
		t.Fatal("the comment woke nobody, so the issue id did not resolve")
	}
	if got[0].Metadata["issue_id"] != "issue-1" {
		t.Fatalf("issue_id = %q", got[0].Metadata["issue_id"])
	}
	if got[0].Metadata["comment_id"] != "comment-1" {
		t.Fatalf("comment_id = %q", got[0].Metadata["comment_id"])
	}
	if !strings.HasSuffix(got[0].Metadata["url"], "/issues/issue-1") {
		t.Fatalf("url = %q", got[0].Metadata["url"])
	}
}

// Intake is the unassigned-inbound surface: triage is the lead's, for any
// action, because nobody has decided who owns the thing yet.
func TestIntakeGoesToTheLead(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	for _, action := range []string{"created", "updated", "deleted"} {
		w := hook("intake_issue", action, map[string]any{
			"id": "intake-1", "issue": "issue-9", "project": "proj-1",
			"name": "From a customer",
		}, func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} })
		got := routeAll(t, p, w, reg)
		if want := []string{"lead"}; !slices.Equal(recipients(t, reg, got), want) {
			t.Errorf("%q woke %v, want the lead", action, recipients(t, reg, got))
		}
		if got[0].Metadata[plane.RoutedViaField] != plane.ViaIntake {
			t.Errorf("%q routed via %q", action, got[0].Metadata[plane.RoutedViaField])
		}
	}
}

// THE LEAD'S OWN ACTION MUST NOT RE-TRIGGER THE LEAD, or a lead filing a
// ticket in their own project wakes themselves, answers, and wakes
// themselves again for as long as nobody is watching.
func TestALeadIsNotWokenByItsOwnAction(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	w := hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Mine to file",
	}, nil) // the actor is the lead
	if got := routeAll(t, p, w, reg); len(got) != 0 {
		t.Fatalf("the lead was woken by its own filing: %v", recipients(t, reg, got))
	}
}

// A misroute is worse than a miss: it teaches a seat that work it does not
// own is its problem.
func TestAnUnmappedProjectWakesNobody(t *testing.T) {
	p, reg := parser(t, nil), registry(t)

	// A project with no lead.
	unmapped := parser(t, func(o *plane.ParserOptions) {
		o.Projects = projects{"proj-2": "OPS"}
	})
	w := hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-2", "name": "Nobody's",
	}, func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} })
	if got := routeAll(t, unmapped, w, reg); len(got) != 0 {
		t.Fatalf("an unmapped project woke %v", recipients(t, reg, got))
	}
	// A project whose identifier does not resolve at all.
	w = hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-unknown", "name": "Nobody's",
	}, func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} })
	if got := routeAll(t, p, w, reg); len(got) != 0 {
		t.Fatalf("an unresolvable project woke %v", recipients(t, reg, got))
	}
}

// CE pages have no subscription model and no comments, so the lead is the
// only recipient there is — an accepted degradation, because page discussion
// happens on work items where the full routing applies.
func TestAPageChangeWakesTheProjectLead(t *testing.T) {
	p, reg := parser(t, nil), registry(t)

	got := routeAll(t, p, hook("page", "updated", map[string]any{
		"id": "page-1", "project": "proj-1", "name": "Deploy runbook",
	}, func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} }), reg)

	if want := []string{"lead"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v, want the lead", recipients(t, reg, got))
	}
	if got[0].Metadata[plane.RoutedViaField] != plane.ViaPageLead {
		t.Fatalf("routed via %q", got[0].Metadata[plane.RoutedViaField])
	}
	if got[0].Metadata["page_id"] != "page-1" {
		t.Fatalf("page_id = %q", got[0].Metadata["page_id"])
	}
	if url := got[0].Metadata["url"]; url != "https://plane.example.com/nimbus/projects/proj-1/pages/page-1" {
		t.Fatalf("url = %q", url)
	}
}

// THE INDEXER ALWAYS RUNS, before every routing filter. The tool-skill
// registry is rebuilt from page content and cares about every change — a
// seat's own edit, an excluded project, a page whose project cannot be
// resolved at all.
func TestThePageIndexerRunsEvenWhereRoutingStops(t *testing.T) {
	type indexed struct{ eventType, pageID string }
	var seen []indexed
	p := parser(t, func(o *plane.ParserOptions) {
		// The excluded project HAS a lead, so a routed page there would
		// wake somebody — otherwise the exclusion is indistinguishable
		// from an unmapped project.
		o.Leads = map[string]string{"eng": "lead", "skills": "qa"}
		o.OnPage = func(_ context.Context, eventType, pageID string) error {
			seen = append(seen, indexed{eventType, pageID})
			return nil
		}
	})
	reg := registry(t)

	cases := []struct {
		name string
		data map[string]any
	}{
		{"an excluded project", map[string]any{"id": "p-ex", "project": "proj-skills"}},
		{"an unresolvable project", map[string]any{"id": "p-orphan"}},
		{"the lead's own edit", map[string]any{"id": "p-own", "project": "proj-1"}},
	}
	for _, c := range cases {
		got := routeAll(t, p, hook("page", "updated", c.data, nil), reg)
		if len(got) != 0 {
			t.Errorf("%s routed to %v", c.name, recipients(t, reg, got))
		}
	}
	if len(seen) != len(cases) {
		t.Fatalf("the indexer ran %d times, want %d", len(seen), len(cases))
	}
	for i, c := range cases {
		if seen[i].eventType != "page.updated" || seen[i].pageID != str(c.data["id"]) {
			t.Errorf("%s indexed %+v", c.name, seen[i])
		}
	}

	// An indexer that FAILS must not break routing: indexing is a
	// separate concern from telling somebody.
	failing := parser(t, func(o *plane.ParserOptions) {
		o.OnPage = func(context.Context, string, string) error {
			return errors.New("registry unavailable")
		}
	})
	w := hook("page", "updated", map[string]any{"id": "page-1", "project": "proj-1"},
		func(b map[string]any) { b["activity"] = map[string]any{"actor_id": uidSWE} })
	if got := routeAll(t, failing, w, reg); len(got) != 1 {
		t.Fatalf("a failing indexer stopped routing: %d notifications", len(got))
	}
}

func str(v any) string { s, _ := v.(string); return s }

// A foreign key arrives as a UUID string on one build and an expanded object
// on the next — a parser that assumed one shape would stop routing anything
// the day a fork rebased, and would do it silently.
func TestExpandedForeignKeysAreRead(t *testing.T) {
	p := parser(t, func(o *plane.ParserOptions) {
		o.Subscribers = subscribers{of: map[string][]string{}}
	})
	reg := registry(t)

	got := routeAll(t, p, hook("issue", "created", map[string]any{
		"id":      map[string]any{"id": "ISSUE-1"},
		"project": map[string]any{"id": "proj-1", "identifier": "ENG"},
		"name":    "Expanded",
		"assignees": []any{
			map[string]any{"id": uidSWE, "display_name": "SWE"},
		},
	}, nil), reg)

	if want := []string{"swe"}; !slices.Equal(recipients(t, reg, got), want) {
		t.Fatalf("woke %v", recipients(t, reg, got))
	}
	// And the id is LOWERCASED, so a build that upper-cases a UUID does
	// not produce a second identity for one person.
	if got[0].Metadata["issue_id"] != "issue-1" {
		t.Fatalf("issue_id = %q", got[0].Metadata["issue_id"])
	}
}

// The actor's own name, however this build serialises it.
func TestTheActorIsNamedHoweverItArrives(t *testing.T) {
	p, reg := parser(t, nil), registry(t)
	sender := func(activity map[string]any) string {
		w := hook("issue", "created", map[string]any{
			"id": "issue-1", "project": "proj-1", "name": "Work",
			"assignees": []any{uidSWE},
		}, func(b map[string]any) { b["activity"] = activity })
		got := routeAll(t, p, w, reg)
		if len(got) == 0 {
			t.Fatal("nobody was woken")
		}
		return got[0].Sender
	}

	if got := sender(map[string]any{
		"actor_id": uidLead,
		"actor":    map[string]any{"display_name": "Ana Lead"},
	}); got != "Ana Lead" {
		t.Errorf("a display name rendered as %q", got)
	}
	if got := sender(map[string]any{
		"actor_id": uidLead,
		"actor":    map[string]any{"first_name": "Ana", "last_name": "Lead"},
	}); got != "Ana Lead" {
		t.Errorf("a split name rendered as %q", got)
	}
	if got := sender(map[string]any{
		"actor_id": uidLead,
		"actor":    map[string]any{"email": "ana@example.com"},
	}); got != "ana@example.com" {
		t.Errorf("an email-only actor rendered as %q", got)
	}
	// With no expanded actor at all, the UUID is what there is — better
	// than a blank From line.
	if got := sender(map[string]any{"actor_id": uidLead}); got != uidLead {
		t.Errorf("a bare actor rendered as %q", got)
	}
}

// The work item's key is DISPLAY ONLY: the conversation key stays the UUID,
// because the sequence number rides only the issue payload and the
// identifier needs a warm cache — keying coalescing on it would split one
// work item into two partitions the moment either was missing.
func TestTheWorkItemKeyIsAbsentWithoutBothHalves(t *testing.T) {
	reg := registry(t)
	noIdentifier := parser(t, func(o *plane.ParserOptions) {
		o.Projects = projects{}
		o.Leads = map[string]string{}
	})

	got := routeAll(t, noIdentifier, hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Work",
		"sequence_id": float64(42), "assignees": []any{uidSWE},
	}, nil), reg)
	if len(got) != 1 {
		t.Fatalf("woke %v", recipients(t, reg, got))
	}
	if key := got[0].Metadata["work_item_key"]; key != "" {
		t.Fatalf("a key was built with no identifier: %q", key)
	}
	// The subject falls back to the workspace rather than rendering an
	// empty bracket.
	if got[0].Subject != "[nimbus] Work" {
		t.Fatalf("subject = %q", got[0].Subject)
	}
	// A sequence number decodes as a float, and "42.000000" is not what
	// anybody means by ENG-42.
	full := routeAll(t, parser(t, nil), hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Work",
		"sequence_id": float64(42), "assignees": []any{uidSWE},
	}, nil), reg)
	if full[0].Metadata["work_item_key"] != "ENG-42" {
		t.Fatalf("work_item_key = %q", full[0].Metadata["work_item_key"])
	}
}

func TestAParserNeedsAWorkspaceURL(t *testing.T) {
	if _, err := plane.NewParser(plane.ParserOptions{}); err == nil {
		t.Fatal("a parser was built with no workspace url")
	}
	p := parser(t, nil)
	if p.Source() != plane.Backend {
		t.Fatalf("Source = %q", p.Source())
	}
	var _ notify.Parser = p
}

// Without a registry the parser cannot tell a colleague from an outsider, so
// it routes what it was given rather than going silent — a degraded bare
// parser must still be useful.
func TestWithoutARegistryTheParserStillRoutes(t *testing.T) {
	p := parser(t, nil)
	got := routeAll(t, p, hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Work",
		"assignees": []any{uidSWE, uidOutsider, ""},
	}, nil), nil)
	if len(got) != 2 {
		t.Fatalf("routed %d, want both real assignees with no registry to filter by", len(got))
	}
	for _, r := range got {
		if len(r.To.ExternalIDs) == 0 || r.To.ExternalIDs[0] == "" {
			t.Fatalf("an empty target was routed: %+v", r.To)
		}
	}

	// AN ASSIGNEE REMOVAL is the one path that can hand an empty target
	// down — every other source filters ids on the way in — and with no
	// registry there is nothing downstream to drop it as an outsider. It
	// would otherwise become a notification whose recipient cascade can
	// only miss, recorded as an undeliverable skip on every removal.
	removal := hook("issue", "updated", map[string]any{
		"id": "issue-1", "project": "proj-1", "name": "Work",
	}, func(b map[string]any) {
		b["activity"] = map[string]any{
			"actor_id": uidLead, "field": "assignees", "new_identifier": "",
		}
	})
	if got := routeAll(t, p, removal, nil); len(got) != 0 {
		t.Fatalf("a removal routed %d notifications with no registry: %+v", len(got), got)
	}
}

// A COMPANY WITH NO ENGINE READ CREDENTIAL still routes. The cache has
// nothing to walk and the subscriber lookup has nothing to ask, so both must
// degrade to what the payload names. The alternative, found by an e2e run,
// was a nil dereference on every inbound webhook — which killed the whole
// fleet-wide inbound consumer rather than one integration.
func TestRoutingSurvivesAMissingEngineCredential(t *testing.T) {
	t.Parallel()
	cache := plane.NewProjectCache(nil, nil)
	if got := cache.Identifier(t.Context(), "some-project"); got != "" {
		t.Fatalf("an unwalkable cache answered %q", got)
	}
	// What it CAN answer is a mapping a payload taught it.
	cache.Learn("proj-eng", "ENG")
	if got := cache.Identifier(t.Context(), "proj-eng"); got != "ENG" {
		t.Fatalf("a learned mapping answered %q, want ENG", got)
	}

	p := parser(t, func(o *plane.ParserOptions) {
		o.Projects = cache
		o.Subscribers = nil
	})
	routed := routeAll(t, p, hook("issue", "created", map[string]any{
		"id": "issue-1", "project": "proj-eng",
		"name": "Fix the login redirect", "assignees": []any{},
	}, func(body map[string]any) {
		body["activity"] = map[string]any{"actor_id": uidOutsider}
	}), registry(t))

	if got := recipients(t, registry(t), routed); !slices.Equal(got, []string{"lead"}) {
		t.Fatalf("recipients = %v, want the project lead", got)
	}
}

// A thread update with no subscriber lookup falls back to the payload's
// assignees rather than reaching nobody — the same degradation, on the path
// that normally spends a request.
func TestAThreadUpdateFallsBackToAssigneesWithNoLookup(t *testing.T) {
	t.Parallel()
	p := parser(t, func(o *plane.ParserOptions) { o.Subscribers = nil })
	routed := routeAll(t, p, hook("issue", "updated", map[string]any{
		"id": "issue-1", "project": "proj-eng", "name": "Fix it",
		"assignees": []any{uidSWE},
	}, nil), registry(t))

	if got := recipients(t, registry(t), routed); !slices.Equal(got, []string{"swe"}) {
		t.Fatalf("recipients = %v, want the payload's assignee", got)
	}
}
