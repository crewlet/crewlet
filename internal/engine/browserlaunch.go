package engine

import (
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// browserLaunch is the browser a seat's launch readies, nil for a seat with no
// role.sandbox.browser: Chromium's own sandbox as c's catalogue reads it, from
// the turn's pinned revision as every launch setting is. It is handed whatever
// the run is and judged only by the launch's step ([sandbox.EnsureBrowser]),
// the one place that knows the box the run got: a revision another build
// admitted, a default a reload moved or a kept box can land a run outside
// what config admitted.
func browserLaunch(c *Company, settings *org.RoleSandbox) *sandbox.BrowserLaunch {
	if settings == nil || !settings.Browser {
		return nil
	}
	return &sandbox.BrowserLaunch{Sandbox: c.Config.Providers.Sandbox.ChromiumSandbox()}
}
