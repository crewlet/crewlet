package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
)

// The chat prune's horizon, as the fleet's revision states it.
//
// WHAT THEY PROTECT: that a prune, which deletes on every data node and has no
// undo, is published only by the horizon of the revision the fleet's
// activation pointer names, and only by a node that runs that very
// activation; that a node which cannot say it does neither publishes nor
// claims the duty, so the lease lapses to a peer that can; and that it says
// why, naming what it serves and the fleet's epoch.
//
// Every case drives [chatPrune.tick] with the ENGINE's own policy source
// ([Engine.chatRetentionPolicy]) over a fleet store whose pointer the case
// moves, and the rooms and the publisher stubbed as the duty's other cases
// stub them.

// pruneCompanyDoc is a company on Chatlet with a thirty-day horizon, the
// shortest `chat.chatlet.message_retention_days` takes, so the node's own
// document would prune every room the cases hold.
const pruneCompanyDoc = `
name: Nimbus
providers:
  llm:
    scripted:
      model: claude-x
roles:
  - name: CEO
    handle: ceo
    llm: scripted
chat:
  chatlet:
    message_retention_days: 30
`

// pointedPrune is a prune duty whose horizon is the engine's own over a fleet
// store the case moves, with its claims counted: a claim is what renews the
// lease, so a node that may not prune must not make one.
type pointedPrune struct {
	duty   *chatPrune
	engine *Engine
	pruner *fakePruner
	rooms  *fakeRooms
	claims int
}

// newPointedPrune wires the duty on a node serving company, which the case
// stamps with the activation it was applied from.
func newPointedPrune(fleet *rosterFleet, company *Company, rooms ...chatRoom) *pointedPrune {
	p := &pointedPrune{engine: &Engine{backends: &Backends{Fleet: fleet}}}
	p.engine.epoch.current.Store(company)
	p.duty, p.pruner, p.rooms = pruneHarness(chatRetentionPolicy{}, anchor, rooms...)
	p.duty.policy = p.engine.chatRetentionPolicy
	p.duty.claim = func(context.Context) (bool, error) {
		p.claims++
		return true, nil
	}
	return p
}

// A PRUNE IS DECIDED ONLY ON A NODE SERVING THE FLEET'S ACTIVATION, AND NOWHERE
// ELSE IS THE DUTY EVEN CLAIMED.
//
// The horizon is the revision the fleet's pointer names, read on a node whose
// company was applied from that very activation ([Engine.fleetCompany]). A
// duty holder that refused the fleet's revision serves the activation before
// it, a node booted on an offline offer serves one no activation named, and a
// pointer that cannot be read or names nothing says nothing either: each
// publishes no record and makes no claim, so a lease it held lapses to a peer
// that runs the fleet's revision rather than standing behind a holder that
// prunes nothing. The node serving the fleet's activation prunes at exactly
// its document's thirty days. Mutation: read the horizon off [Engine.Company]
// again and every stale case prunes; ask for it after the claim and every
// stale case renews the duty.
func TestTheChatPruneDecidesOnlyOnANodeServingTheFleetsRevision(t *testing.T) {
	t.Parallel()
	horizon := anchor.Add(-config.MinMessageRetentionDays * 24 * time.Hour)
	// Inside one window of the thirty days, so the room's only rung IS the
	// horizon, and a node pruning by any other number publishes another
	// cutoff, or none.
	floor := horizon.Add(-chatPruneWindow / 2)
	theFleets := func(_, current coord.Activation) time.Time { return current.At }
	for _, tc := range []struct {
		name string
		// stamp is the activation the node's company was applied from,
		// given the fleet's earlier and current ones; nil activates
		// nothing at all.
		stamp func(earlier, current coord.Activation) time.Time
		// down makes the pointer unreadable.
		down   bool
		prunes bool
	}{
		{name: "a node serving the fleet's activation", stamp: theFleets, prunes: true},
		{name: "a duty holder serving an activation the fleet has replaced",
			stamp: func(earlier, _ coord.Activation) time.Time { return earlier.At }},
		{name: "a node booted on an offer no activation named",
			stamp: func(coord.Activation, coord.Activation) time.Time { return time.Time{} }},
		{name: "a node that cannot read the pointer", stamp: theFleets, down: true},
		{name: "a fleet that has activated nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fleet := &rosterFleet{memoryFleet: coordmem.NewFleet()}
			company := companyWith(t, pruneCompanyDoc)
			if tc.stamp != nil {
				earlier := activate(t, fleet, "rev-1")
				company.ActivatedAt = tc.stamp(earlier, activate(t, fleet, "rev-2"))
			}
			fleet.down = tc.down
			p := newPointedPrune(fleet, company, room("general", nil, floor))

			p.duty.tick(t.Context())

			if !tc.prunes {
				if len(p.pruner.calls) != 0 {
					t.Fatalf("a node that cannot say it runs the fleet's revision "+
						"published %d prune records: %v", len(p.pruner.calls),
						p.pruner.calls)
				}
				if p.claims != 0 {
					t.Errorf("a node that may not prune claimed the duty %d "+
						"time(s), renewing the lease a peer that runs the fleet's "+
						"revision needs", p.claims)
				}
				if p.rooms.reads != 0 {
					t.Errorf("a node that may not prune read the rooms %d time(s)",
						p.rooms.reads)
				}
				return
			}
			if p.claims != 1 {
				t.Errorf("a node serving the fleet's activation claimed the duty "+
					"%d time(s), want once a tick", p.claims)
			}
			if got := p.pruner.cutoffsFor("general"); !slices.EqualFunc(got,
				[]time.Time{horizon}, time.Time.Equal) {
				t.Fatalf("a node serving the fleet's activation published %v, want "+
					"one cutoff at the thirty days its document states, %s", got, horizon)
			}
		})
	}
}

// A NODE THAT DECLINES SAYS WHAT IT SERVES AND WHAT THE FLEET IS ON.
//
// A horizon nobody enforces is otherwise invisible: the messages simply stay.
// So every tick that declines logs `chat_prune_declined` at warn, its reason
// naming the activation this node serves and the fleet's epoch, while a fleet
// that has activated nothing, an unconfigured deployment rather than a fault,
// is not warned about at all.
//
// NOT PARALLEL: it swaps the process-wide logger, which this package otherwise
// leaves at its boot default, and a parallel case's lines would be counted.
func TestADeclinedPruneNamesWhatThisNodeServesAndTheFleetsEpoch(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	declined := func() []string {
		var reasons []string
		for _, record := range logs.records(t) {
			if record["msg"] == "chat_prune_declined" {
				reason, _ := record["err"].(string)
				reasons = append(reasons, reason)
			}
		}
		return reasons
	}
	fleet := &rosterFleet{memoryFleet: coordmem.NewFleet()}
	company := companyWith(t, pruneCompanyDoc)
	p := newPointedPrune(fleet, company, room("general", nil, anchor.Add(-365*24*time.Hour)))

	p.duty.tick(t.Context())
	if got := declined(); len(got) != 0 {
		t.Errorf("a fleet that has activated nothing was warned about: %q", got)
	}

	earlier := activate(t, fleet, "rev-1")
	current := activate(t, fleet, "rev-2")
	company.ActivatedAt = earlier.At
	p.duty.tick(t.Context())
	got := declined()
	if len(got) != 1 {
		t.Fatalf("a node serving a replaced activation logged %d declines, want "+
			"one for its one tick: %q", len(got), got)
	}
	for _, want := range []string{
		earlier.At.Format(time.RFC3339Nano),
		fmt.Sprintf("epoch %d", current.Epoch),
		current.At.Format(time.RFC3339Nano),
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the decline says %q, which does not name %q", got[0], want)
		}
	}
	if len(p.pruner.calls) != 0 || p.claims != 0 {
		t.Errorf("a declining node published %d records and claimed %d time(s)",
			len(p.pruner.calls), p.claims)
	}
}
