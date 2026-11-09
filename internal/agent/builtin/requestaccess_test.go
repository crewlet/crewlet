package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// accessDeps is the tracker surface the comment tool, and so request_access,
// writes through.
func accessDeps(trk *fakeTracker) builtin.WorkDeps {
	return builtin.WorkDeps{Reader: trk, Writer: trk.as, Dependencies: trk.depends}
}

// AN ACCESS REQUEST IS A DECISION THE ENGINE WRITES: the grant option is the
// catalogue's key and the only other option declines, recommended and asked of
// the person named, the agent's reason its rationale. Posted through the
// comment tool, so nothing about the item's gates or the ask's routing differs.
func TestAnAccessRequestIsADecisionWhoseOptionIsTheGrant(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	got := callIn(t, workRegistry(t, accessDeps(trk)), turnFor(t, "agent-cto"),
		builtin.RequestAccessTool, map[string]any{
			"item": "ENG-1", "ask": "founder", "access": string(advisor.AccessHealth),
			"why": "The backup alarm fired and no agent is told when an alarm fires.",
		})
	if got.Failed {
		t.Fatalf("the request failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil {
		t.Fatalf("patched = %+v, want one comment", trk.patched)
	}
	c := trk.patched[0].Comment
	d := c.Decision
	if d == nil || c.Ask != "founder" {
		t.Fatalf("comment = %+v, want a decision asked of founder", c)
	}
	if len(d.Options) != 2 || d.Options[0].ID != string(advisor.AccessHealth) ||
		d.Options[1].ID != "decline" || d.Recommended != string(advisor.AccessHealth) ||
		d.Role != decision.RoleApprover || !strings.Contains(d.Question, "watch the engine's health") {
		t.Errorf("decision = %+v, want the grant recommended over declining, approver's", d)
	}
	if err := d.ValidateRecommendedAt(""); err != nil {
		t.Errorf("the decision is not one an agent may ask: %v", err)
	}
}

// AND REFUSED BEFORE ANYTHING IS POSTED where it could never be granted: a key
// the catalogue does not name, a person who is not one, or no reason.
func TestAnAccessRequestNobodyCouldGrantIsRefused(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"an unknown access": {"item": "ENG-1", "ask": "founder", "access": "grant-everything",
			"why": "x"},
		"asked of an agent": {"item": "ENG-1", "ask": "agent-ceo",
			"access": string(advisor.AccessHealth), "why": "x"},
		"no reason": {"item": "ENG-1", "ask": "founder",
			"access": string(advisor.AccessHealth), "why": " "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, accessDeps(trk))
			entry, ok := reg.Lookup(builtin.RequestAccessTool)
			if !ok {
				t.Fatal("request_access is not registered")
			}
			got, err := entry.Tool.(tools.SeatCallable).CallForTurn(everyGrant(),
				turnFor(t, "agent-cto"), args)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Failed || len(trk.patched) != 0 {
				t.Errorf("result = %+v with %d writes, want a refusal and nothing posted",
					got, len(trk.patched))
			}
		})
	}
}

// NO AUTHOR'S DECISION MAY OFFER A GRANT: a reader takes a `grant-` choice as
// that grant, so only the decision request_access builds may carry one.
func TestNoAuthorsDecisionMayOfferAGrant(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	asked := decisionArg()
	asked["options"] = []any{
		map[string]any{"id": string(advisor.AccessHealth), "label": "Approve", "detail": "Ship it"},
		map[string]any{"id": "hold", "label": "Hold"},
	}
	asked["recommended"] = string(advisor.AccessHealth)
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": true,
		"item": "ENG-1", "body": "Approve the release?", "ask": "pm", "decision": asked})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, "request_access") {
		t.Errorf("a decision offering %s gave %+v, want it refused", advisor.AccessHealth, got)
	}
	if len(trk.patched) != 0 {
		t.Error("a decision offering a grant was posted")
	}
}

// A GRANT IS ANSWERED ONLY ONCE ITS ASKER HOLDS IT, whoever sends the answer:
// the agent is told yes only when the access is saved.
func TestAGrantIsAnsweredOnlyOnceItsAskerHoldsIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		seat func(context.Context, string) (*org.Role, error)
		want bool
	}{
		"not applied yet": {func(context.Context, string) (*org.Role, error) {
			return &org.Role{}, nil
		}, false},
		"applied": {func(context.Context, string) (*org.Role, error) {
			return &org.Role{HealthTools: true}, nil
		}, true},
		"cannot be confirmed": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.thread.AnswersDecision = &decision.Decision{Options: []decision.Option{
				{ID: string(advisor.AccessHealth), Label: "Allow it"}, {ID: "decline", Label: "Not now"}}}
			trk.thread.AnsweredAuthor = "agent-cto"
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, gated(builtin.Deps{Work: builtin.WorkDeps{
				Reader: trk, Writer: trk.as, FleetSeat: tc.seat}})); err != nil {
				t.Fatal(err)
			}
			got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
				"item": "ENG-1", "answers": "c-ask", "choice": string(advisor.AccessHealth)})
			if posted := !got.Failed && len(trk.patched) == 1; posted != tc.want {
				t.Errorf("answer = %+v with %d writes, want posted %v", got, len(trk.patched), tc.want)
			}
		})
	}
}
