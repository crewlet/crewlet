package prefetch_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ownAsks answers a seat's open asks to each person from a fixed set, and
// records whom it was asked about.
type ownAsks struct {
	mu    sync.Mutex
	open  map[string]int
	asked []string
}

func (o *ownAsks) OwnAsks(_ context.Context, asker, person string, limit int) (worklet.OwnAsks, error) {
	o.mu.Lock()
	o.asked = append(o.asked, asker+"->"+person)
	o.mu.Unlock()
	n := o.open[person]
	out := worklet.OwnAsks{Asks: []worklet.OwnAskRow{}, Total: worklet.ClaimTotal{Total: n}}
	for i := range min(n, limit) {
		key := fmt.Sprintf("ACME-%d", i+1)
		out.Asks = append(out.Asks, worklet.OwnAskRow{
			TaskRow: worklet.TaskRow{ID: "id-" + key, Key: key, Title: "Ship the release"},
			Comment: fmt.Sprintf("c-%d", i+1), AskedOf: person,
			AskedAt:  time.Now().Add(-50 * time.Hour),
			Question: "Merge the backend pull request now?",
			Withdraw: fmt.Sprintf("withdraw_ask(item: %q, ask: \"c-%d\", reason: \"…\")", key, i+1),
		})
	}
	return out, nil
}

// A TURN WITH A PERSON LISTS THE SEAT'S OPEN ASKS TO THEM (ADR-0124): each with
// its item, its question, its age and the call that withdraws it, for the
// people among the turn's senders, never an agent colleague or a sender the
// roster does not hold, at most [prefetch.MaxOpenAsksListed] across the turn
// with the rest counted.
//
// Mutation: drop the human-seat filter and the block reads the agent's asks
// too; drop the cap and the block lists all twelve.
func TestATurnWithAPersonListsTheSeatsOpenAsksToThem(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Units: []*org.Unit{{
		Name: "Engineering", Lead: "lead",
		Roles: []*org.Role{
			{Name: "Tech Lead", DeclaredHandle: "lead"},
			{Name: "Quality", DeclaredHandle: "qa"},
			{Name: "Ana Lind", DeclaredHandle: "ana", Kind: org.KindHuman},
			{Name: "Bo Reyes", DeclaredHandle: "bo", Kind: org.KindHuman},
		},
	}}}
	o.Normalize()
	asks := &ownAsks{open: map[string]int{"ana": 12, "bo": 1, "qa": 4}}
	r := prefetch.Request{Seat: o.Role("lead"), AgentID: "agent-1", Org: o,
		Boundary: provenance.NewBoundary(),
		Senders: []learning.Subject{
			{Handle: "ana", Name: "Ana Lind"}, {Handle: "qa", Name: "Quality"},
			{Handle: "ana", Name: "Ana Lind"}, {ExternalID: "U9", Platform: "chat", Name: "Stranger"},
			{Handle: "bo", Name: "Bo Reyes"},
		}}
	got := fetch(t, prefetch.Sources{Asks: asks}, r).OpenAsks

	if strings.Join(asks.asked, ",") != "lead->ana,lead->bo" {
		t.Errorf("read the asks to %v, want the two people once each", asks.asked)
	}
	for _, want := range []string{prefetch.OpenAsksLead[:40], "To Ana Lind (ana), 12 open:",
		`ACME-1 "Ship the release": "Merge the backend pull request now?"`, "(2 days ago)",
		`withdraw_ask(item: "ACME-1", ask: "c-1"`, "- and 2 more: list them with list_work_items(" +
			`asked_by: "lead", asked_of: "ana")`, "To Bo Reyes (bo), 1 open:", "- and 1 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("the block lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "Withdraw with"); n != prefetch.MaxOpenAsksListed {
		t.Errorf("the block lists %d asks, want the cap of %d", n, prefetch.MaxOpenAsksListed)
	}
	if strings.Contains(got, "Quality") {
		t.Errorf("the block names an agent colleague's asks:\n%s", got)
	}
	none := fetch(t, prefetch.Sources{Asks: &ownAsks{open: map[string]int{}}}, r).OpenAsks
	if none != "" {
		t.Errorf("a seat with no open asks was handed %q", none)
	}
}
