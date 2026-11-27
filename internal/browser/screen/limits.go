package screen

import (
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/costlimit"
)

// The rates a node's hubs take posts at, a job's and the node's: the package
// doc's "The limits" says why each is what it is.
const (
	// frameRate and frameBurst are a job's frames: twice the fastest a box
	// sends, as a bucket, so frames bunched in transit are all taken.
	frameRate  = 20
	frameBurst = 4
	// reportBurst is a job's states' burst, and its check-ins', each at
	// browser.ReportRate: a relay meeting a new hub restates both and
	// retries each.
	reportBurst = 4
	// nodeRate and nodeBurst are a node's, each verb's: ten times a job's
	// frames, which the browser jobs one node's memory holds never reach.
	nodeRate  = 200
	nodeBurst = 40
)

// nodeScope is the one scope every hub charges on its limiters: the node's
// bucket, which every job's hub on the node pays into, each job's own keyed
// by its launch beneath it.
const nodeScope = "node"

// Limits are a node's limits on what its hubs take, one limiter a verb, each
// post paid by its job's bucket and by the node's (the package doc's "The
// limits"). Built once a node ([NewLimits]) and handed to every hub there, so
// the node's buckets are one.
type Limits struct {
	Frames, States, Checkins *costlimit.Limiter
}

// NewLimits builds a node's limits on a clock (nil is the wall clock).
func NewLimits(now func() time.Time) Limits {
	report := costlimit.Rates{PrincipalRate: browser.ReportRate, PrincipalBurst: reportBurst,
		ScopeRate: nodeRate, ScopeBurst: nodeBurst}
	return Limits{
		Frames: costlimit.New(costlimit.Rates{PrincipalRate: frameRate, PrincipalBurst: frameBurst,
			ScopeRate: nodeRate, ScopeBurst: nodeBurst}, now),
		States:   costlimit.New(report, now),
		Checkins: costlimit.New(report, now),
	}
}

// complete reports whether every limiter is set: a hub with one missing
// would take that verb without a bound.
func (l Limits) complete() bool { return l.Frames != nil && l.States != nil && l.Checkins != nil }

// charge pays for one post of launch's on limiter, before anything of the
// post is read, its body included ([Hub.admit]), so a box posting in a loop
// pays before it costs a read or a parse; a refusal is browser.TooFast with
// the refusing bucket's wait.
func charge(limiter *costlimit.Limiter, launch string) error {
	if refused := limiter.Take(nodeScope, launch, 1); refused != nil {
		return &browser.TooFast{RetryAfter: refused.RetryAfter}
	}
	return nil
}
