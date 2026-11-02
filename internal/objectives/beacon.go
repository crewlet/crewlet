package objectives

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// ReadyOutcome is how a frame ended, as the viewer's browser reports it.
type ReadyOutcome string

const (
	// ReadyOK is a frame whose page said its bridge hello.
	ReadyOK ReadyOutcome = "ready"

	// ReadyFailed is a frame that failed to load, or whose page never said
	// hello before the dashboard gave up on it.
	ReadyFailed ReadyOutcome = "failed"
)

// ReadyOutcomes are every outcome a report may carry, the closed set of the
// `outcome` attribute of [metrics.ArtifactsReadyDuration].
var ReadyOutcomes = []ReadyOutcome{ReadyOK, ReadyFailed}

// Valid reports whether o is an outcome a report may carry.
func (o ReadyOutcome) Valid() bool { return slices.Contains(ReadyOutcomes, o) }

// The beacon's abuse limits.
const (
	// ReadyReportsPerSecond is the sustained rate of reports one credential
	// may send: ONE A SECOND, well past any person's rate of opening
	// artifacts (each report needs a frame, and each frame a ticket the
	// viewer asked for), and low enough that one credential's reports
	// cannot outweigh a busy node's real traffic within an hour.
	ReadyReportsPerSecond = 1

	// ReadyReportsBurst is how many reports one credential may send at once:
	// thirty, a dashboard reopening every frame it had open after a
	// reconnect.
	ReadyReportsBurst = 30

	// MaxRememberedFrames bounds the frames whose report this node
	// remembers, each until its ticket ends: 65 536, a few megabytes, an
	// hour of a thousand people each opening a frame a minute. Past it a
	// report is refused for a moment rather than remembered loosely, since
	// a frame counted twice skews the objective and a report dropped does
	// not.
	MaxRememberedFrames = 1 << 16

	// maxViewers bounds the credentials whose buckets are kept. Past it,
	// every bucket that has refilled is dropped, which changes no answer: a
	// full bucket and an absent one admit alike.
	maxViewers = 4096
)

// ErrReported refuses a second report of one frame: each frame is one event.
var ErrReported = errors.New("objectives: this frame's readiness was already reported")

// ErrImplausible refuses a report whose numbers could not have been measured:
// a time longer than the frame has existed, a negative time, an outcome
// outside the closed set, or a frame whose ticket has ended.
var ErrImplausible = errors.New("objectives: the report could not have been measured")

// Throttled refuses a report past the beacon's limits, naming when one will be
// admitted again.
type Throttled struct {
	RetryAfter time.Duration
}

func (e *Throttled) Error() string {
	return fmt.Sprintf("objectives: too many readiness reports; send the next in %s",
		e.RetryAfter.Round(time.Millisecond))
}

// ReadyReport is one frame's ready time, as the dashboard reported it and the
// API bound it to a frame this fleet minted.
type ReadyReport struct {
	// Viewer is the credential the frame was minted for, whose limits the
	// report is held to.
	Viewer string

	// Frame identifies the frame (its bridge nonce): one report per frame.
	Frame string

	// Minted is the earliest instant the frame can have been minted, and
	// Expires when its ticket ends. No frame measures a time longer than it
	// has existed, and none is remembered past its ticket.
	Minted, Expires time.Time

	// Outcome is how the frame ended and Elapsed how long after its ticket
	// was minted, by the browser's clock.
	Outcome ReadyOutcome
	Elapsed time.Duration
}

// Beacon admits the browser-reported ready times into the meter.
//
// SAFE FOR CONCURRENT USE. Its memory is per node: a frame reported to two
// nodes is counted once on each, which the per-credential limit bounds and
// which a fleet-wide set would cost a coordination write per page view to
// prevent.
type Beacon struct {
	meter *Meter
	now   func() time.Time

	// maxFrames is [MaxRememberedFrames], held on the beacon so a test can
	// reach the bound without remembering sixty-five thousand frames.
	maxFrames int

	mu      sync.Mutex
	frames  map[string]time.Time
	viewers map[string]*bucket
}

// bucket is one credential's token bucket: tokens held at last.
type bucket struct {
	tokens float64
	last   time.Time
}

// NewBeacon builds the beacon over meter, on the clock now (nil is UTC wall
// time).
func NewBeacon(meter *Meter, now func() time.Time) (*Beacon, error) {
	if meter == nil {
		return nil, errors.New("objectives: the beacon needs the meter its reports are counted into")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Beacon{meter: meter, now: now, maxFrames: MaxRememberedFrames,
		frames: map[string]time.Time{}, viewers: map[string]*bucket{}}, nil
}

// Accept admits one report and counts it: into `crewlet.artifacts.ready.duration`
// by its outcome, and into the meter as one [ArtifactReady] event. It refuses
// an implausible report ([ErrImplausible]), a frame already reported
// ([ErrReported]) and a credential past its rate ([Throttled]), counting
// nothing for any of them.
func (b *Beacon) Accept(r ReadyReport) error {
	now := b.now()
	switch {
	case strings.TrimSpace(r.Viewer) == "" || strings.TrimSpace(r.Frame) == "":
		return fmt.Errorf("%w: it names no viewer or no frame", ErrImplausible)
	case !r.Outcome.Valid():
		return fmt.Errorf("%w: outcome %q is not one of %s", ErrImplausible, r.Outcome, words.Or(ReadyOutcomes))
	case r.Elapsed < 0:
		return fmt.Errorf("%w: a negative time", ErrImplausible)
	case !now.Before(r.Expires):
		return fmt.Errorf("%w: the frame's ticket has ended", ErrImplausible)
	case r.Elapsed > now.Sub(r.Minted):
		return fmt.Errorf("%w: %s is longer than the frame has existed", ErrImplausible, r.Elapsed)
	}

	b.mu.Lock()
	if _, seen := b.frames[r.Frame]; seen {
		b.mu.Unlock()
		return ErrReported
	}
	if wait := b.roomFor(now); wait > 0 {
		b.mu.Unlock()
		return &Throttled{RetryAfter: wait}
	}
	if wait := b.take(r.Viewer, now); wait > 0 {
		b.mu.Unlock()
		return &Throttled{RetryAfter: wait}
	}
	b.frames[r.Frame] = r.Expires
	b.mu.Unlock()

	if b.meter.rec != nil {
		b.meter.rec.Observe(metrics.ArtifactsReadyDuration, r.Elapsed,
			metrics.Attrs{"outcome": string(r.Outcome)})
	}
	b.meter.Record(ArtifactReady, r.Elapsed, r.Outcome == ReadyOK)
	return nil
}

// roomFor makes room for one more remembered frame, forgetting every frame
// whose ticket has ended, and answers how long until there is room when every
// remembered frame's ticket is still live. Callers hold the lock.
func (b *Beacon) roomFor(now time.Time) time.Duration {
	if len(b.frames) < b.maxFrames {
		return 0
	}
	var earliest time.Time
	for frame, expires := range b.frames {
		if !now.Before(expires) {
			delete(b.frames, frame)
			continue
		}
		if earliest.IsZero() || expires.Before(earliest) {
			earliest = expires
		}
	}
	if len(b.frames) < b.maxFrames {
		return 0
	}
	return earliest.Sub(now)
}

// take charges one report to viewer's bucket, answering how long until the
// bucket holds one when it is empty. Callers hold the lock.
func (b *Beacon) take(viewer string, now time.Time) time.Duration {
	if len(b.viewers) >= maxViewers {
		for v, k := range b.viewers {
			if refill(k, now) >= ReadyReportsBurst {
				delete(b.viewers, v)
			}
		}
	}
	k := b.viewers[viewer]
	if k == nil {
		k = &bucket{tokens: ReadyReportsBurst, last: now}
		b.viewers[viewer] = k
	}
	k.tokens, k.last = refill(k, now), now
	if k.tokens < 1 {
		return time.Duration((1 - k.tokens) / ReadyReportsPerSecond * float64(time.Second))
	}
	k.tokens--
	return 0
}

// refill is what k holds at now: its tokens at last plus the sustained rate
// since, never past the burst.
func refill(k *bucket, now time.Time) float64 {
	elapsed := now.Sub(k.last).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	return min(float64(ReadyReportsBurst), k.tokens+elapsed*ReadyReportsPerSecond)
}
