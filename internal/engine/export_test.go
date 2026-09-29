package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// RefreshChartForTest brings this node's chart view up to its applier's cursor
// and reports the position the published view carries.
//
// EXPORTED FOR A TEST ONLY, because the four things that call it in production
// are a hook, a timer and two boot steps — none of which a test can drive
// deterministically, and all of which would turn a case about the DERIVATION
// into a case about a timer. What is asserted through it is exactly what those
// four share: the no-op at an equal cursor, and the view a rebuild publishes.
func RefreshChartForTest(ctx context.Context, e *Engine) (org.ViewPosition, error) {
	return e.refreshChart(ctx)
}

// CollectChartSealsForTest runs the orphan sweep's collection once, as of now.
//
// EXPORTED FOR A TEST ONLY: in production it runs under the retention sweep's
// duty on a fifteen-minute tick, and what a case asserts is the judgement and
// the delete, not the timer — so it hands in the instant the sweep would run
// at, which is how a case steps past the grace without waiting an hour.
func CollectChartSealsForTest(ctx context.Context, e *Engine, now time.Time) (int64, error) {
	return e.collectChartSeals(ctx, now)
}

// SeatBoxSetupForTest is the provisioning a seat's box is given at launch, as
// this node would hand it over: the provider-wide steps passed in, then the
// seat's own from the running company, every file's body read through this
// node's resolver.
//
// EXPORTED FOR A TEST ONLY: in production it is one step of a launch that
// needs a manager, a pending-run store and a budget, none of which is what a
// case about how a file reaches the box is about.
func SeatBoxSetupForTest(e *Engine, defaults []sandbox.SetupStep, handle string) []sandbox.SetupStep {
	return e.boxSetup(defaults, handle, seatSandbox(e.Company(), handle))
}

// ChartRosterForTest is the mailbox sweep's roster past its settings-epoch
// gate: the agent seats of the company this node publishes, or why this node
// cannot vouch that it holds every hire.
//
// EXPORTED FOR A TEST ONLY: in production it is read by the retention sweep's
// duty after the activation pointer and the reconciler agree, and a case about
// the CHART's half would otherwise have to stand up a reconciler and an
// activation to reach it.
func ChartRosterForTest(ctx context.Context, e *Engine) ([]placement.Seat, error) {
	return e.chartRoster(ctx)
}

// PublishForTest makes c the company this engine serves.
//
// EXPORTED FOR A TEST ONLY: what it builds is the window between a chart
// record committing and the view that carries it being published, which in
// production is a coalesced rebuild no test can hold open.
func PublishForTest(e *Engine, c *Company) { e.epoch.current.Store(c) }

// ForgetPersonBlinderForTest drops this node's resolved blinder, so the next
// use resolves the company's key again.
//
// EXPORTED FOR A TEST ONLY: in production the cache is right for the life of
// the process, because the key never changes while a company runs. What a
// test needs is the state a restarted node is in after somebody deleted the
// key — and an engine cannot be restarted over the same store in one process.
func ForgetPersonBlinderForTest(e *Engine) {
	e.personBlinds.mu.Lock()
	defer e.personBlinds.mu.Unlock()
	e.personBlinds.held = nil
}

// ViewPosition is the chart position the published company was derived from,
// and whether this engine has read a chart at all.
//
// EXPORTED FOR A TEST ONLY. What it exists for is the one comparison that
// makes the activation stamp mean something: a case asserting the recorded
// position against a constant would pass for a stamp that wrote any number.
func (e *Engine) ViewPosition() (org.ViewPosition, bool) { return e.epoch.viewAt() }

// eachAuthoredSeat walks every seat a DOCUMENT declares, at any depth.
//
// A fixture that edits a company file wants the FILE's walk, and config's own
// is unexported for the reason this comment exists: outside that package,
// "walk the document" and "walk the company" are different questions, and
// only one of them is answered by `roles:` and `units:`. A test about the
// running company reads [Company.Org] instead.
func eachAuthoredSeat(c *config.Company, visit func(*config.Role)) {
	for i := range c.Roles {
		visit(&c.Roles[i])
	}
	var walk func([]config.Unit)
	walk = func(units []config.Unit) {
		for i := range units {
			for j := range units[i].Roles {
				visit(&units[i].Roles[j])
			}
			walk(units[i].Children)
		}
	}
	walk(c.Units)
}
