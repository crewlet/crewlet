package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// AN APPLY ASKS FOR PLACEMENT EXACTLY WHEN WHAT PLACEMENT READS MOVED.
//
// Which seats exist and where each may run are the whole of what a sweep reads
// of the company, so a revision that changes either is one the sweep must see
// at once, and one that changes neither — a prompt, a model, a re-activated
// revision rotating a credential — asks for nothing. Order is not a change:
// the seats are walked off the org, and the same seats in another order are
// nothing anybody has to claim.
func TestAnApplyAsksForPlacementExactlyWhenTheSeatsMoved(t *testing.T) {
	t.Parallel()
	company := func(t *testing.T, doc string) *Company {
		t.Helper()
		cfg, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		c, err := NewCompany(cfg)
		if err != nil {
			t.Fatalf("NewCompany: %v", err)
		}
		return c
	}
	const head = `
name: Moves
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
roles:
`
	two := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu}
  - {name: Dev, handle: dev, llm: zulu}
`)
	swapped := company(t, head+`
  - {name: Dev, handle: dev, llm: zulu}
  - {name: Lead, handle: lead, llm: zulu}
`)
	reworded := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu, backstory: "says it differently"}
  - {name: Dev, handle: dev, llm: zulu}
`)
	three := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu}
  - {name: Dev, handle: dev, llm: zulu}
  - {name: Ops, handle: ops, llm: zulu}
`)
	renamed := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu}
  - {name: Dev, handle: developer, llm: zulu}
`)
	pinned := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu, placement: {node: node-b}}
  - {name: Dev, handle: dev, llm: zulu}
`)
	labelled := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu, placement: {labels: {zone: eu}}}
  - {name: Dev, handle: dev, llm: zulu}
`)
	relabelled := company(t, head+`
  - {name: Lead, handle: lead, llm: zulu, placement: {labels: {zone: us}}}
  - {name: Dev, handle: dev, llm: zulu}
`)
	for _, c := range []struct {
		name           string
		previous, next *Company
		moved          bool
	}{
		{"a node's first company", nil, two, true},
		{"the same seats", two, two, false},
		{"the same seats in another order", two, swapped, false},
		{"a change placement does not read", two, reworded, false},
		{"a seat added", two, three, true},
		{"a seat removed", three, two, true},
		{"a seat renamed", two, renamed, true},
		{"a seat pinned to a node", two, pinned, true},
		{"a seat given labels", two, labelled, true},
		{"a seat's labels changed", labelled, relabelled, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := seatsMoved(c.previous, c.next); got != c.moved {
				t.Errorf("seatsMoved = %v, want %v (from %v to %v)", got, c.moved,
					seatsOf(c.previous), seatsOf(c.next))
			}
		})
	}
}

// seatsOf is what a case's failure names.
func seatsOf(c *Company) []placement.Seat { return c.Seats() }
