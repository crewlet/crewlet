package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/panics"
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
	})
	backends.Queue.AddPublishListener(e.issueReports.OnPublish)
}

// startIssueSender runs the reporter's sender on every node that built one, and
// hands it every panic this process contains. A node without `data` runs no core,
// and detects as much as any other. Off, the sender waits on a queue nothing fills.
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
	stopObserving := panics.Observe(e.issueReports.Panicked)
	e.endIssueSender = func() {
		stopObserving()
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
// classifier could not attribute, or a message whose deliveries all went on
// hand-backs. A failure it attributes (a provider, a credential, a budget, a box
// that could not be reached) is the environment's, and no maintainer can mend it.
func (e *Engine) reportDeadLetter(ctx context.Context, handle string, result queue.Result) {
	if e.issueReports == nil {
		return
	}
	outcome := issuereport.OutcomeDefer
	if result.Outcome == queue.OutcomeNak {
		if runner.ErrorKind(result.Err) != issueReportEngineKind {
			return
		}
		outcome = issuereport.OutcomeNak
	}
	e.issueReports.DeadLettered(ctx, handle, outcome)
}

// issueReportEngineKind is the classifier's word for a failure it could attribute
// to nothing outside the engine.
const issueReportEngineKind = "error"

// IssueReporter is this node's reporter, nil on an engine with no broker.
func (e *Engine) IssueReporter() *issuereport.Reporter { return e.issueReports }
