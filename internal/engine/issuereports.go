package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/version"
)

// startIssueReports builds the reporter and puts it on this node's publishes,
// on every node with a broker and a fleet store (ADR-0101). It starts off: the
// company's revision says otherwise ([Engine.applyIssueReports]).
func (e *Engine) startIssueReports(boot *config.Bootstrap, backends *Backends) {
	if backends == nil || backends.Queue == nil || backends.Fleet == nil {
		return
	}
	var stream, coordination string
	if boot != nil {
		stream, coordination = string(boot.Stream.Type), string(boot.Coordination.Type)
	}
	e.issueReports = issuereport.New(issuereport.Config{
		Store: backends.Fleet,
		// THE BINARY'S OWN WORD, and nothing a document or a variable reaches.
		Origin: issuereport.OriginOf(version.String()),
		Topology: func(ctx context.Context) issuereport.Topology {
			return e.reportTopology(ctx, stream, coordination)
		},
		ExecutorContextWindow: sandbox.ExecutorContextWindow,
		Fleet:                 e.reportFleet,
	})
	backends.Queue.AddPublishListener(e.issueReports.OnPublish)
}

// startIssueSender runs the reporter's sender on every node that built one. A
// node without `data` runs no core, and detects as much as any other. Off, the
// sender waits on a queue nothing fills.
func (e *Engine) startIssueSender(ctx context.Context) {
	if e.issueReports == nil {
		return
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.issueReports.Run(runCtx)
	}()
	e.endIssueSender = func() {
		cancel()
		<-done
	}
}

// stopIssueSender ends the sender and waits for a send under way, which settles
// on its own bound. Safe where none started: a failed boot reaches it too.
func (e *Engine) stopIssueSender() {
	if e.endIssueSender != nil {
		e.endIssueSender()
	}
}

// applyIssueReports holds the reporter to the revision this node runs, a
// revision that turns it off included, and to no company at all as off.
func (e *Engine) applyIssueReports(c *Company) {
	if e.issueReports != nil {
		e.issueReports.SetEnabled(c != nil && c.Config != nil && c.Config.Diagnostics.IssueReports.Enabled)
	}
}

// reportFleet is the whole fleet as the reporter's shadow reads it: every
// alarm kind standing, this node's own evaluation and each node's as its last
// beat carried it (a node without `data` evaluates none, so on one the beats
// are the whole answer), and whether seats are moving by design: a node
// draining, or live nodes on more than one build. An unreadable lease table is
// an error, never a quiet fleet.
func (e *Engine) reportFleet(ctx context.Context) (issuereport.Fleet, error) {
	var fleet issuereport.Fleet
	if own, ok := e.Alarms(); ok {
		for _, kind := range own {
			fleet.Alarms = append(fleet.Alarms, string(kind))
		}
	}
	if n := e.node; n != nil {
		if host := n.Host(); host != nil && host.Draining() {
			fleet.Moving = true
		}
	}
	if e.backends == nil || e.backends.Coord == nil {
		return fleet, nil
	}
	nodes, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return issuereport.Fleet{}, err
	}
	builds := map[string]bool{}
	for _, lease := range nodes {
		status, ok := coord.StatusFromMeta(lease.Meta)
		if !ok || status.Build == "" {
			// A NODE THAT DOES NOT SAY ITS BUILD may run another one.
			fleet.Moving = true
		} else {
			builds[status.Build] = true
		}
		fleet.Moving = fleet.Moving || status.Draining
		for _, mark := range status.Alarms {
			fleet.Alarms = append(fleet.Alarms, mark.Kind)
		}
	}
	fleet.Moving = fleet.Moving || len(builds) > 1
	return fleet, nil
}

// reportTopology reads how this engine runs, for a report being built.
func (e *Engine) reportTopology(ctx context.Context, stream, coordination string) issuereport.Topology {
	nodes := 1
	if live, err := e.presenceHolders(ctx); err == nil {
		nodes = len(live)
	}
	cell := "none"
	if c := e.epoch.current.Load(); c != nil && c.Config != nil {
		if placement := c.Config.Providers.Sandbox.RunIn(); placement != "" {
			cell = string(placement)
		}
	}
	return issuereport.NewTopology(nodes, stream, coordination, cell)
}

// reportDeadLetter tells the reporter of a seat's message going to the
// dead-letter subject, WHERE THE CAUSE IS THE ENGINE'S: a Nak's failure the
// classifier could not attribute, which is a lost message at the place its
// stamp names, or a message whose budget went on hand-backs, by the cause of
// the last. A failure it attributes (a provider, a credential, a budget, a box,
// the environment) is not the engine's, and no maintainer can mend it.
func (e *Engine) reportDeadLetter(ctx context.Context, handle string, result queue.Result) {
	if e.issueReports == nil {
		return
	}
	switch result.Outcome {
	case queue.OutcomeDefer:
		if result.Cause.Valid() {
			e.issueReports.DeadLettered(ctx, handle, result.Cause)
		}
	case queue.OutcomeNak:
		if runner.ErrorKind(result.Err) == types.ErrorKindUnclassified {
			at, _ := origin.Of(result.Err)
			e.issueReports.MessageLost(ctx, handle, at.Function, at.File)
		}
	}
}

// IssueReporter is this node's reporter, nil on an engine with no broker.
func (e *Engine) IssueReporter() *issuereport.Reporter { return e.issueReports }
