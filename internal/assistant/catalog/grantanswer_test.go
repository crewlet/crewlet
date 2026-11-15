package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// grantQuestion is ENG-58's thread, holding the SRE's request for a grant as
// the engine asks it: the question's three options, two of which carry the
// request's id.
func grantQuestion() map[string]any {
	return map[string]any{"key": "ENG-58", "title": "Route the deck library", "comments": []any{
		map[string]any{"id": "c-ask", "ask": "founder", "body": "Origin rule for the library route.",
			"decision": map[string]any{"question": "Grant the Site Reliability Engineer Origin Rules: Edit?",
				"options": []any{
					map[string]any{"id": seataccess.GrantOptionTemporary("abcde12345"),
						"label": "Grant for this change (expires in 4 hours)"},
					map[string]any{"id": seataccess.GrantOptionPermanent("abcde12345"),
						"label": "Add to this agent permanently"},
					map[string]any{"id": "decline", "label": "Not now"},
				}}}}}
}

// grantRoutes answers a grant answer's preview reads: the thread, the stored
// request, and the SRE's Cloudflare lane with Origin Rules: Edit looked up,
// grantable where grantable and lacking at the connection otherwise.
func grantRoutes(grantable bool) proposal.Read {
	entry := seataccess.LookupEntry{ID: "Origin Write", Name: "Origin Rules: Edit", Group: "Zone",
		Write: true, Level: "No level", Grantable: "Yes"}
	if !grantable {
		entry.Grantable = "The provisioner token does not hold Origin Rules: Edit"
		entry.Missing, entry.ActionURL = "Origin Rules: Edit", "https://dash.cloudflare.com/profile/api-tokens"
	}
	request := seataccess.GrantRequest{ID: "abcde12345", Kind: integration.KindCloudflare,
		Lane: seataccess.LaneCloudflare, Handle: "sre", Permission: "Origin Write",
		Scope: []string{"example.com"}, Hours: 4, Reason: "Approve the permanent grant now.",
		Item: "ENG-58", Ask: "founder"}
	view := seataccess.LaneView{Kind: integration.KindCloudflare, Lane: seataccess.LaneCloudflare,
		Integration: "Cloudflare", Handle: "sre", Name: "Site Reliability Engineer", Grant: "Read",
		Lookup: []seataccess.LookupEntry{entry}}
	answers := map[string]any{
		"/worklet/comments":                                          grantQuestion(),
		"/setup/access/requests/abcde12345":                          request,
		"/setup/integrations/cloudflare/access/seats/sre/cloudflare": view,
	}
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		if r.Method != http.MethodGet {
			return exec.Response{Status: http.StatusInternalServerError}, nil
		}
		if strings.HasSuffix(r.Path, "/cloudflare") && r.Query.Get("permission") != "Origin Write" {
			return exec.Response{Status: http.StatusBadRequest}, nil
		}
		body, ok := answers[r.Path]
		if !ok {
			return exec.Response{Status: http.StatusNotFound}, nil
		}
		raw, _ := json.Marshal(body)
		return exec.Response{Status: http.StatusOK, Body: raw}, nil
	}
}

// rowsSaid is every row of a card as one text: label, from and to.
func rowsSaid(p proposal.Preview) string {
	var b strings.Builder
	for _, c := range p.Changes {
		b.WriteString(c.Label + ": " + c.From + " -> " + c.To + "\n")
	}
	return b.String()
}

// A GRANT REQUEST'S ANSWER IS DRAWN IN THE ENGINE'S WORDS, AND APPROVING IT ASKS
// THE PERSON TO CONFIRM IT IS THEM (ADR-0118). Choosing the grant draws the
// permission by the catalogue's name, where the stored request applies it, how
// long it lasts and that it changes things, never the option's label, and the
// proposal asks a step-up, since its approval saves the grant at the access
// write's own proof; the request it sends is the answer itself. Declining is
// drawn as a decline of that grant and asks nothing more. A grant the company's
// connection cannot give yet is refused, saying what a person adds at the
// vendor and where. Mutation: draw the option's label, drop the step-up, or
// prepare a grant the connection lacks, and this fails.
func TestAGrantRequestsAnswerIsDrawnInTheEnginesWords(t *testing.T) {
	t.Parallel()
	answer := mustTool(t, "answer_decision")
	for _, c := range []struct {
		name   string
		choice string
		title  string
		says   []string
		stepUp bool
	}{
		{"granted for its hours", seataccess.GrantOptionTemporary("abcde12345"), "Grant an agent's request",
			[]string{"Grant:  -> Origin Rules: Edit on example.com", "Lasts:  -> 4 hours from when you approve",
				"Agent:  -> Site Reliability Engineer", "may change things at Cloudflare"}, true},
		{"granted for good", seataccess.GrantOptionPermanent("abcde12345"), "Grant an agent's request",
			[]string{"Lasts:  -> For good"}, true},
		{"declined", "decline", "Decline an agent's request",
			[]string{"decline the request for Origin Rules: Edit on example.com"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, err := answer.Preview(t.Context(), grantRoutes(true), wednesday, map[string]any{
				"item": "ENG-58", "answers": "c-ask", "choice": c.choice})
			if err != nil {
				t.Fatal(err)
			}
			said := rowsSaid(p)
			if p.Title != c.title || p.StepUp != c.stepUp {
				t.Errorf("card %q step-up %v, want %q step-up %v", p.Title, p.StepUp, c.title, c.stepUp)
			}
			for _, want := range c.says {
				if !strings.Contains(said, want) {
					t.Errorf("the card does not say %q:\n%s", want, said)
				}
			}
			if strings.Contains(said, "Grant for this change") || strings.Contains(said, "Approve the permanent") {
				t.Errorf("the card draws the option's label or the agent's words:\n%s", said)
			}
			var sent map[string]any
			if err := json.Unmarshal(p.Request.Body, &sent); err != nil {
				t.Fatal(err)
			}
			if args, _ := sent["args"].(map[string]any); args["choice"] != c.choice {
				t.Errorf("the approval sends %s, want the answer choosing %s", p.Request.Body, c.choice)
			}
		})
	}

	_, err := answer.Preview(t.Context(), grantRoutes(false), wednesday, map[string]any{
		"item": "ENG-58", "answers": "c-ask", "choice": seataccess.GrantOptionTemporary("abcde12345")})
	if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(),
		"a person adds Origin Rules: Edit to the company's connection at https://dash.cloudflare.com/profile/api-tokens first") {
		t.Errorf("a grant the connection lacks was prepared, or refused without what to add: %v", err)
	}
}
