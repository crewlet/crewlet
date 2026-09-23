package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/org"
)

// ProberForTest is the deactivation probe exactly as this node's duty builds
// it, over a provider and a session store the case supplies.
//
// EXPORTED FOR A TEST ONLY, because the duty that builds it in production
// needs a provider Tier A names — an https issuer this engine's own transport
// dials — and a session a real sign-in opened, and neither a fake identity
// provider nor a test certificate can stand in for those through the
// configuration. What the case asserts is the half that is this package's:
// that the probe the node builds says what it ends on the node's own feed.
func ProberForTest(e *Engine, provider *oidc.Provider, sessions oidc.Sessions) *oidc.Prober {
	return e.newProber(provider, sessions)
}

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
