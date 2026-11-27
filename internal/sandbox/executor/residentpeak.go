package executor

import (
	"cmp"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// RESIDENT PEAK. On a `direct` or unbounded `container` cell a run's harness
// lives in the node's memory beside the engine, so each runner declares the
// most its harness's process tree held ([Harness.ResidentPeak]) on the release
// its profile is written for: headless against a stand-in model API, summed
// VmRSS sampled every 20 ms, once as a one-turn run (the larger of a 31-byte
// and a 400 KB brief) and once resuming a session padded to 16, 64, 128 and
// 256 MiB of transcript in many lines, each point its runs' largest. The
// growth is no one slope (Kimi Code's flattens, Muse Code's steepens), so the
// figure is the curve. docs/concepts/code-sandbox.md, "Each runner's
// memory", lists them.

// ResidentPeak is one harness's declared peak memory as a function of the
// session its run resumes.
type ResidentPeak struct {
	// Release is the harness release the points were measured on, "" for a
	// stand-in.
	Release string

	// Points are the measured peaks, ascending by Session, the first at
	// Session zero (a run that resumes nothing).
	Points []PeakPoint

	// StandIn is why no point was measured on this harness, "" for a measured
	// one. A stand-in declares no points: [Runner.ResidentPeak] gives it the
	// heaviest measured curve ([heaviestMeasured]).
	StandIn string
}

// PeakPoint is one measurement: the run's peak resident bytes, resuming a
// session of Session bytes of plaintext.
type PeakPoint struct {
	Session  int64
	Resident int64
}

// Base is the peak of a run that resumes nothing.
func (p ResidentPeak) Base() int64 { return p.Points[0].Resident }

// At is the peak of a run resuming a session of session bytes: the upper
// envelope of the points (each the largest at or below its session, since a
// larger session never holds less), interpolated linearly between them. Every
// declared curve reaches [executorkeep.SessionPlainMaxBytes], the largest
// session a take keeps, so no carried session is past its last point; one
// that were is held at the last segment's slope.
func (p ResidentPeak) At(session int64) int64 {
	env := p.envelope()
	i, _ := slices.BinarySearchFunc(env, session, func(e PeakPoint, s int64) int {
		return cmp.Compare(e.Session, s)
	})
	switch {
	case i < len(env) && env[i].Session == session:
		return env[i].Resident
	case i == 0:
		return env[0].Resident
	case i == len(env):
		i--
	}
	a, b := env[i-1], env[i]
	return a.Resident + (b.Resident-a.Resident)*(session-a.Session)/(b.Session-a.Session)
}

// Most is the peak of a run resuming the largest session a take keeps.
func (p ResidentPeak) Most() int64 { return p.At(executorkeep.SessionPlainMaxBytes) }

func (p ResidentPeak) envelope() []PeakPoint {
	env := slices.Clone(p.Points)
	for i := 1; i < len(env); i++ {
		env[i].Resident = max(env[i].Resident, env[i-1].Resident)
	}
	return env
}

// measuredPeak is a harness's curve from its measurements in MiB: the
// one-turn peak, then the resume peaks at 16, 64, 128 and 256 MiB.
func measuredPeak(release string, base, at16, at64, at128, at256 int64) ResidentPeak {
	return ResidentPeak{Release: release, Points: []PeakPoint{
		{0, base << 20}, {16 << 20, at16 << 20}, {64 << 20, at64 << 20},
		{128 << 20, at128 << 20}, {executorkeep.SessionPlainMaxBytes, at256 << 20}}}
}

// heaviestMeasured is the curve a stand-in takes: at each point the largest
// any measured runner declares, so a harness nobody measured is never reckoned
// lighter than the heaviest one somebody did.
var heaviestMeasured = sync.OnceValue(func() []PeakPoint {
	var out []PeakPoint
	for _, h := range Registered() {
		p := h.ResidentPeak()
		switch {
		case p.StandIn != "":
		case out == nil:
			out = slices.Clone(p.Points)
		default:
			for i := range out {
				out[i].Resident = max(out[i].Resident, p.Points[i].Resident)
			}
		}
	}
	return out
})

// ResidentPeak is this runner's harness's declared peak memory, a stand-in's
// being the heaviest measured curve ([Harness.ResidentPeak]).
func (r *Runner) ResidentPeak() ResidentPeak {
	p := r.harness.ResidentPeak()
	if p.StandIn != "" {
		p.Points = slices.Clone(heaviestMeasured())
	}
	return p
}
