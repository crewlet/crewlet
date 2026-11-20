package queries

import (
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
)

// A FINDING AS AN ANALYST STATES IT (ADR-0166): a headline that carries its
// number, the stats behind it, a chart where it has one, the seats it is
// about inside it, and one primary action with any others beside it. Filled
// here from what every finding already keeps, so each finding reads this way
// whatever produced it; a class or an investigation that states more fills
// the finding's own fields, which win.

// AdvisorFindingInsight is the part of a finding the Advisor's page leads with.
type AdvisorFindingInsight struct {
	// Headline is one line carrying the finding's number; the class's title
	// where the finding states none.
	Headline string         `json:"headline"`
	Stats    []advisor.Stat `json:"stats"`
	Chart    *advisor.Chart `json:"chart"`
	// Seats are the seats the finding is about, each with its own number
	// where it has one.
	Seats   []AdvisorSeatShare `json:"seats"`
	Actions AdvisorActions     `json:"actions"`
	// PremiseCheckedAt is when the fact the finding rests on last held, nil
	// until its premise is re-checked.
	PremiseCheckedAt *time.Time `json:"premise_checked_at"`
}

// AdvisorSeatShare is one seat a finding is about, with its share of the number.
type AdvisorSeatShare struct {
	Handle string        `json:"handle"`
	Name   string        `json:"name"`
	Value  *float64      `json:"value"`
	Unit   *advisor.Unit `json:"unit"`
}

// AdvisorActions is what a person can do about a finding: one primary, nil
// where there is nothing to do, and the others.
type AdvisorActions struct {
	Primary   *AdvisorAction  `json:"primary"`
	Secondary []AdvisorAction `json:"secondary"`
}

// AdvisorAction is one thing a person can do about a finding. Every key is
// sent; those its kind does not use are null.
type AdvisorAction struct {
	// Kind is "apply" (re-check and send fix option Option), "reassign" (move
	// Items to the seat To), "open" (Page and Target, or Setting, in the
	// dashboard), "step" (outside the engine, at URL where one is named),
	// "ask" (Ask Crewlet drafts it from Question) or "fix" (the finding's fix
	// as it stands, for a kind no action names yet).
	Kind     string   `json:"kind"`
	Label    string   `json:"label"`
	Option   *int     `json:"option"`
	Items    []string `json:"items"`
	To       *string  `json:"to"`
	Page     *string  `json:"page"`
	Target   *string  `json:"target"`
	Setting  *string  `json:"setting"`
	URL      *string  `json:"url"`
	Question *string  `json:"question"`
}

// The action kinds.
const (
	ActionApply    = "apply"
	ActionReassign = "reassign"
	ActionOpen     = "open"
	ActionStep     = "step"
	ActionAsk      = "ask"
	ActionFix      = "fix"
)

// insightOf is f's insight, its stats read from facts as the finding shows them.
func insightOf(f advisor.Finding, facts []AdvisorFact) AdvisorFindingInsight {
	out := AdvisorFindingInsight{Headline: f.Headline, Stats: []advisor.Stat{}, Seats: []AdvisorSeatShare{},
		Actions: actionsOf(f.Fix)}
	if out.Headline == "" {
		out.Headline = f.Title
	}
	for _, fact := range facts {
		out.Stats = append(out.Stats, advisor.Stat{Key: fact.Key, Label: fact.Label, Value: float64(fact.Value),
			Unit: fact.Unit})
	}
	for _, s := range f.Seats {
		unit := s.Unit
		out.Seats = append(out.Seats, AdvisorSeatShare{Handle: s.Seat, Name: s.Name, Value: &s.Value, Unit: &unit})
	}
	if len(f.Seats) == 0 && f.Subject.Kind == advisor.SubjectSeat && f.Subject.Seat != "" {
		name := f.Subject.Name
		if name == "" {
			name = f.Subject.Seat
		}
		out.Seats = append(out.Seats, AdvisorSeatShare{Handle: f.Subject.Seat, Name: name})
	}
	return out
}

// actionsOf is a fix's actions: the first option of a configuration change
// primary and the others beside it, or the one step its kind takes.
func actionsOf(fix advisor.Fix) AdvisorActions {
	out := AdvisorActions{Secondary: []AdvisorAction{}}
	some := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	switch fix.Kind {
	case advisor.FixConfigChange:
		for i, o := range fix.Options {
			a := AdvisorAction{Kind: ActionApply, Label: o.Label, Option: &i}
			if i == 0 {
				out.Primary = &a
				continue
			}
			out.Secondary = append(out.Secondary, a)
		}
	case advisor.FixOpenSetting:
		out.Primary = &AdvisorAction{Kind: ActionOpen, Label: "Open the setting", Setting: some(fix.Setting)}
	case advisor.FixOpenPage:
		out.Primary = &AdvisorAction{Kind: ActionOpen, Label: "Open it", Page: some(string(fix.Page)),
			Target: some(fix.Target)}
	case advisor.FixExternalStep:
		out.Primary = &AdvisorAction{Kind: ActionStep, Label: "Take the step", Setting: some(fix.Setting),
			URL: some(fix.URL)}
	case advisor.FixAskCrewlet:
		out.Primary = &AdvisorAction{Kind: ActionAsk, Label: "Ask Crewlet to draft it", Question: some(fix.Prompt)}
	case advisor.FixNone:
	default:
		out.Primary = &AdvisorAction{Kind: ActionFix, Label: "See the fix"}
	}
	return out
}
