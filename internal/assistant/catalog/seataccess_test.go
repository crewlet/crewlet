package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// accessRoutes answers Datadog's Agents tab as the engine does: the SRE on at
// the Standard Role and the QA engineer off, under a strong tag, and a check
// that plans one step per changed lane and asks for a step-up on a raise to the
// Admin Role.
type accessRoutes struct {
	t    *testing.T
	mu   sync.Mutex
	sent []exec.Request
}

func (a *accessRoutes) read(_ context.Context, r exec.Request) (exec.Response, error) {
	a.mu.Lock()
	a.sent = append(a.sent, r)
	a.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.Path == "/setup/integrations/datadog/access":
		raw, _ := json.Marshal(seataccess.Answer{Kind: "datadog", Revision: "rev-4",
			Options: []seataccess.Options{{Lane: seataccess.LaneDatadog, Live: true,
				Levels: []seataccess.Level{{ID: "role-ro", Name: "Datadog Read Only Role"},
					{ID: "role-std", Name: "Datadog Standard Role"},
					{ID: "role-admin", Name: "Datadog Admin Role", Administers: true}}}},
			Seats: []seataccess.SeatAnswer{
				{Handle: "sre", Name: "SRE", Lanes: map[seataccess.Lane]seataccess.LaneAnswer{
					seataccess.LaneDatadog: {Desired: seataccess.Desired{Enabled: true,
						Level: "role-std"}}}},
				{Handle: "qa", Name: "QA Engineer", Lanes: map[seataccess.Lane]seataccess.LaneAnswer{
					seataccess.LaneDatadog: {}}},
			}})
		return exec.Response{Status: http.StatusOK, Header: http.Header{"Etag": {`"rev-4"`}},
			Body: raw}, nil
	case r.Method == http.MethodGet:
		return exec.Response{Status: http.StatusNotFound}, nil
	case r.Query.Get(authz.DryRunQuery) != "true":
		a.t.Errorf("the preview sent %s %s, a write", r.Method, r.Path)
		return exec.Response{Status: http.StatusInternalServerError}, nil
	}
	if r.IfMatch != `"rev-4"` {
		a.t.Errorf("the check was sent under %q, want the tag the tab was read at", r.IfMatch)
	}
	var draft seataccess.Draft
	if err := json.Unmarshal(r.Body, &draft); err != nil {
		a.t.Errorf("the check's body is not a draft: %v", err)
	}
	plan := seataccess.PlanAnswer{Kind: "datadog", Revision: "rev-4"}
	for handle, lanes := range draft.Seats {
		for lane, d := range lanes {
			raise := d.Level == "role-admin"
			plan.StepUp = plan.StepUp || raise
			plan.Plans = append(plan.Plans, seataccess.SeatPlan{Handle: handle, Lane: lane,
				StepUp: raise, Steps: []seataccess.Step{{Verb: seataccess.VerbRerole,
					Text: "Move it to " + d.Level, By: seataccess.ByCrewlet}}})
		}
	}
	raw, _ := json.Marshal(map[string]any{"kind": plan.Kind, "revision": plan.Revision,
		"plans": plan.Plans, "step_up": plan.StepUp, "warnings": []any{}})
	return exec.Response{Status: http.StatusOK, Body: raw}, nil
}

// AN AGENT'S ACCESS IS CHANGED AS ITS AGENTS TAB CHANGES IT.
//
// "Give the SRE the Datadog Admin Role" names the role as Datadog does. The
// change is the seat's lane as read with only the role moved, sent where the
// tab saves (PUT /setup/integrations/datadog/access) under the tag it was read
// at; the card shows the role from Standard to Admin by Datadog's names, the
// step the engine will take, and that it is a raise to an administration role,
// with its step-up. Mutation: send the role's name instead of its id, or bind
// no revision, and this fails.
func TestAnAgentsAccessIsChangedAsItsTabChangesIt(t *testing.T) {
	t.Parallel()
	routes := &accessRoutes{t: t}
	p, err := mustTool(t, "set_seat_access").Preview(t.Context(), routes.read, wednesday,
		map[string]any{"handle": "SRE", "integration": "datadog",
			"access": "Datadog Admin Role"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.Method != http.MethodPut ||
		p.Request.Path != "/setup/integrations/datadog/access" {
		t.Fatalf("the change is sent as %s %s", p.Request.Method, p.Request.Path)
	}
	var draft seataccess.Draft
	if err := json.Unmarshal(p.Request.Body, &draft); err != nil {
		t.Fatal(err)
	}
	if got := draft.Seats["sre"][seataccess.LaneDatadog]; !got.Enabled || got.Level != "role-admin" {
		t.Errorf("the draft is %+v, want the SRE on at Datadog's admin role id", got)
	}
	got := rows(p)
	if row := got["level"]; row.From != "Datadog Standard Role" || row.To != "Datadog Admin Role" {
		t.Errorf("the role row is %+v", row)
	}
	if got["plan.1"].To == "" || got["raise"].To == "" {
		t.Errorf("the card does not show the plan and the raise: %v", got)
	}
	r, err := proposal.ParseRevision(p.SourceRevision)
	if err != nil || r.Kind != proposal.RevisionEntity ||
		r.ID != "/setup/integrations/datadog/access" || r.Version != "rev-4" {
		t.Errorf("the change is bound to %q", p.SourceRevision)
	}
	if !p.StepUp || p.Target != "SRE on Datadog" {
		t.Errorf("the card is %+v", p)
	}
	if strings.Contains(string(p.Request.Body), "${") {
		t.Errorf("the change names a variable: %s", p.Request.Body)
	}
}

// THE CALL NAMES ONLY WHAT THE TAB OFFERS.
//
// A role Datadog does not list is refused with the ones it does; an agent the
// tab does not list is refused with the ones it does; and a change that
// changes nothing is refused rather than drawn as a card.
func TestASeatAccessCallNamesOnlyWhatTheTabOffers(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"no such role":  {"handle": "sre", "integration": "datadog", "access": "Root"},
		"no such agent": {"handle": "nobody", "integration": "datadog", "enabled": true},
		"no change":     {"handle": "sre", "integration": "datadog", "access": "role-std"},
		"no levels":     {"handle": "sre", "integration": "datadog", "permissions": map[string]any{"contents": "read"}},
	} {
		routes := &accessRoutes{t: t}
		_, err := mustTool(t, "set_seat_access").Preview(t.Context(), routes.read, wednesday,
			args)
		if !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s: err = %v, want the arguments refused", name, err)
		}
	}
}
