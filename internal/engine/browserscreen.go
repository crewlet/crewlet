package engine

import (
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/browser/screen"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// browserHub is the browser screen of job, which s is opened for: the hub its
// box's relay posts to (internal/browser/screen), on the node's one set of
// limits ([Engine.screenLimits]). Nil for a job with no browser. A launch's
// waits for its ensure ([sandbox.JobBrowserAsked]), told through the opening
// once ensure answered ready at or above the floor; a job opened again from a
// record whose browser relays its screen serves at once
// ([sandbox.JobBrowserReady]). The one place a job's hub is built.
func (e *Engine) browserHub(s *mcpbridge.Session, job sandbox.OpenedJob) (*screen.Hub, error) {
	if job.Browser == "" {
		return nil, nil
	}
	return screen.NewHub(screen.HubOptions{Launch: job.LaunchID, Turn: s.RunID, Seat: s.Handle,
		Serving: job.Browser == sandbox.JobBrowserReady, Limits: e.screenLimits})
}
