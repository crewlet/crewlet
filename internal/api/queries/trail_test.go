package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// trailFixture is a store holding one turn of each unit seat of
// [twinUnitsCompany] — a phase and its completion, each charged to a work item
// — and the sources over it, the chart naming `lead` the lead of eng-payments
// and of nobody else.
func trailFixture(t *testing.T) queries.Sources {
	t.Helper()
	company, err := config.ParseCompany([]byte(twinUnitsCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	log := openStore(t).Events()
	at := time.Now().UTC().Add(-30 * time.Minute)
	i := 0
	for _, handle := range []string{"eng-payments", "eng-search"} {
		id, ok := org.DeriveAgentID("Acme", handle)
		if !ok {
			t.Fatalf("no id for %s", handle)
		}
		for _, typ := range []string{"agent_phase_completed", "turn_completed"} {
			raw, _ := json.Marshal(map[string]any{
				"agent_id": id.String(), "role": "Engineer", "turn_id": "turn-" + handle,
				"phase": "execute", "response": "done",
				"work_item": map[string]any{"backend": "native", "id": "task-" + handle,
					"key": "ENG-1", "project": "ENG"},
			})
			if err := log.Append(t.Context(), store.EventRecord{
				ID: typ + "-" + handle, Type: typ, Source: "engine", Category: "task",
				Time: at.Add(time.Duration(i) * time.Second),
				Tags: store.ExtractTags(raw), Payload: raw,
			}); err != nil {
				t.Fatal(err)
			}
			i++
		}
	}
	return queries.Sources{
		Events:  fleetOf(log),
		Company: companySource(t, company),
		Chart:   leadsChart{lead: "lead", report: "eng-payments"},
	}
}

// ONE SEAT'S TURNS AND PHASES ARE ITS TRAIL, AND EVERY SEAT'S ARE ADMIN'S
// (ADR-0033).
//
// A listing naming no seat is the company-wide trail — every prompt every
// agent was given — and only an admin reads it, whatever apps anybody else
// holds or whoever they lead. A listing naming a seat is that seat's trail,
// read by its lead holding no permission at all, and refused to them for a
// seat they do not lead, each refusal naming admin; and a chart this node
// cannot read is unavailable rather than a refusal.
//
// Mutation: drop [queries.Sources.trailOf]'s company-wide decision and the
// lead reads every seat's turns; decide `seat=` on the caller instead of the
// seat named, and the lead is refused their own agent.
func TestTurnsWithoutASeatAreAdmins(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"turns", "phases"} {
		for _, c := range []struct {
			name    string
			ctx     context.Context
			params  map[string]any
			chart   authz.Chart
			allowed bool
			err     error
		}{
			{"an admin, every seat", asGrants(t, "", iam.GrantAdmin), nil, nil, true, nil},
			{"every app, every seat", asGrants(t, "", iam.Apps...), nil, nil, false, nil},
			{"a lead, every seat", asSeat(t, "lead"), nil, nil, false, nil},
			{"a lead, the seat they lead", asSeat(t, "lead"),
				map[string]any{"seat": "eng-payments"}, nil, true, nil},
			{"a lead, a seat they do not lead", asSeat(t, "lead"),
				map[string]any{"seat": "eng-search"}, nil, false, nil},
			{"a stranger holding every app, one seat", asSeat(t, "pat", iam.Apps...),
				map[string]any{"seat": "eng-payments"}, nil, false, nil},
			{"a chart this node cannot read", asSeat(t, "lead"),
				map[string]any{"seat": "eng-payments"}, authz.NoChart{}, false,
				queries.ErrUnavailable},
		} {
			t.Run(what+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				s := trailFixture(t)
				if c.chart != nil {
					s.Chart = c.chart
				}
				r := queries.NewRegistry()
				queries.Register(r, s)
				_, err := r.Answer(c.ctx, what, c.params)
				switch {
				case c.err != nil:
					if !errors.Is(err, c.err) {
						t.Fatalf("answered %v, want %v", err, c.err)
					}
				case c.allowed:
					if err != nil {
						t.Fatalf("refused: %v", err)
					}
				default:
					var refusal *queries.Refusal
					if !errors.As(err, &refusal) || !slices.Equal(refusal.Grants,
						[]iam.Grant{iam.GrantAdmin}) {
						t.Fatalf("answered %v, want a refusal naming [admin]", err)
					}
				}
			})
		}
	}
}

// AND A LEAD WITHOUT THE TRACKER READS THEIR AGENT'S TRAIL WITHOUT ITS WORK
// ITEMS: the task a turn was charged to is the tracker's (ADR-0033), so a turn
// row names none to them and a phase record's work-item tags — and the
// `work_item` its own payload states — are withheld, while the rest of the
// payload, which is the seat's body and theirs to read, reaches them whole;
// the same lead holding Worklet reads all of it.
//
// Mutation: drop the tracker rule from `turns` or `phases`, or leave the
// payload's own `work_item` in [maskHistory], and the lead without it is
// shown the item.
func TestATrailNamesItsWorkItemsOnlyToWorklet(t *testing.T) {
	t.Parallel()
	seat := map[string]any{"seat": "eng-payments"}
	for _, c := range []struct {
		name    string
		holding []iam.Grant
		shown   bool
	}{
		{"holding no permission", nil, false},
		{"holding Worklet", []iam.Grant{iam.GrantWorklet}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := queries.NewRegistry()
			queries.Register(r, trailFixture(t))
			ctx := asSeat(t, "lead", c.holding...)
			got, err := r.Answer(ctx, "turns", seat)
			if err != nil {
				t.Fatalf("turns: %v", err)
			}
			turns := got.(map[string]any)["turns"].([]store.Turn)
			if len(turns) != 1 {
				t.Fatalf("the lead's agent has %d turns, want one", len(turns))
			}
			if shown := turns[0].WorkItem != nil; shown != c.shown {
				t.Errorf("the turn names its work item: %v, want %v", shown, c.shown)
			}
			got, err = r.Answer(ctx, "phases", seat)
			if err != nil {
				t.Fatalf("phases: %v", err)
			}
			phases := got.(map[string]any)["phases"].([]store.EventRecord)
			if len(phases) == 0 {
				t.Fatal("the lead's agent has no phases")
			}
			for _, phase := range phases {
				if _, shown := phase.Tags["work_item"]; shown != c.shown {
					t.Errorf("phase %s carries its work item tag: %v, want %v",
						phase.ID, shown, c.shown)
				}
				if len(phase.Payload) == 0 {
					t.Errorf("phase %s reached its seat's lead without its payload", phase.ID)
					continue
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(phase.Payload, &payload); err != nil {
					t.Fatalf("phase %s payload: %v", phase.ID, err)
				}
				if _, shown := payload["work_item"]; shown != c.shown {
					t.Errorf("phase %s payload carries its work item: %v, want %v",
						phase.ID, shown, c.shown)
				}
				if string(payload["response"]) != `"done"` {
					t.Errorf("phase %s payload lost its body: response = %s",
						phase.ID, payload["response"])
				}
			}
		})
	}
}

// AND A TRAIL IS NARROWED TO A WORK ITEM ONLY FOR THE TRACKER: which of an
// agent's turns were on one item is the tracker's fact (ADR-0033), and a
// filter by the item would hand back exactly the item the test above withholds
// from each row. A lead without Worklet is refused naming it (never
// answered the unfiltered page, which is a different question from the one
// they asked), and the same lead holding Worklet is answered the turns on
// that item.
//
// Mutation: drop the tracker check on `work_item=` in `turns` and the lead
// without Worklet is answered their agent's turn on the item.
func TestATrailIsNarrowedToAWorkItemOnlyForWorklet(t *testing.T) {
	t.Parallel()
	params := map[string]any{"seat": "eng-payments", "work_item": "native:task-eng-payments"}
	r := queries.NewRegistry()
	queries.Register(r, trailFixture(t))

	_, err := r.Answer(asSeat(t, "lead"), "turns", params)
	var refusal *queries.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != authz.ReasonNoGrant ||
		!slices.Equal(refusal.Grants, []iam.Grant{iam.GrantWorklet}) {
		t.Fatalf("a lead without Worklet narrowing to a work item = %v, "+
			"want a refusal naming [worklet]", err)
	}

	got, err := r.Answer(asSeat(t, "lead", iam.GrantWorklet), "turns", params)
	if err != nil {
		t.Fatalf("a lead holding Worklet narrowing to a work item: %v", err)
	}
	turns := got.(map[string]any)["turns"].([]store.Turn)
	if len(turns) != 1 || turns[0].WorkItem == nil ||
		turns[0].WorkItem.Ref() != "native:task-eng-payments" {
		t.Errorf("the lead holding Worklet was answered %+v, want the one "+
			"turn on native:task-eng-payments", turns)
	}
}
