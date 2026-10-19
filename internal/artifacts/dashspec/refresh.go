package dashspec

import "slices"

// RefreshMode is how a dataset's rows are kept current (D63).
type RefreshMode string

// The ways a dataset refreshes.
const (
	// RefreshLive is a store dataset: the artifact's datastore pushes every
	// change to an open board, which reads the dataset again.
	RefreshLive RefreshMode = "live"

	// RefreshSignal is an engine dataset whose domain announces a change on
	// a push the dashboard already receives ([EngineSource.Signal]), and
	// on its declared interval as well when it declares one.
	RefreshSignal RefreshMode = "signal"

	// RefreshInterval is an engine dataset read again on the interval it
	// declares, its domain announcing no change.
	RefreshInterval RefreshMode = "interval"

	// RefreshManual is an engine dataset with neither: read when the board
	// opens and when the viewer asks.
	RefreshManual RefreshMode = "manual"

	// RefreshFixed is a file dataset: it changes only with the version.
	RefreshFixed RefreshMode = "fixed"

	// RefreshDerived is a calculated dataset: read again whenever one of
	// its inputs is.
	RefreshDerived RefreshMode = "derived"
)

// Freshness is how one dataset of a board is kept current: the one answer
// the dashboard schedules a refresh by and the Sources rail describes.
type Freshness struct {
	Mode RefreshMode `json:"mode"`

	// Collection is the store collection a live dataset watches.
	Collection string `json:"collection,omitempty"`

	// Signal is the push an engine dataset refetches on.
	Signal Signal `json:"signal,omitempty"`

	// EverySeconds is an engine dataset's declared interval.
	EverySeconds int `json:"every_seconds,omitempty"`

	// Inputs are the datasets a calculated one reads, directly, in order.
	Inputs []string `json:"inputs,omitempty"`
}

// RefreshOf is how dataset d of a validated spec is kept current. A dataset
// an agent keeps on a schedule (Phase 3) is a store dataset whose
// [Freshness] gains the schedule beside its live mode: it is a field here,
// never a second decision.
func RefreshOf(d Dataset) Freshness {
	switch d.Source {
	case SourceStore:
		if d.Store != nil {
			return Freshness{Mode: RefreshLive, Collection: d.Store.Collection}
		}
	case SourceEngine:
		every := 0
		if d.Refresh != nil {
			every = d.Refresh.EverySeconds
		}
		src, ok := Source(engineName(d))
		switch {
		case ok && src.Signal != SignalNone:
			return Freshness{Mode: RefreshSignal, Signal: src.Signal, EverySeconds: every}
		case every > 0:
			return Freshness{Mode: RefreshInterval, EverySeconds: every}
		}
		return Freshness{Mode: RefreshManual}
	case SourceFile:
		return Freshness{Mode: RefreshFixed}
	case SourceCalc:
		return Freshness{Mode: RefreshDerived, Inputs: CalcInputs(d)}
	}
	return Freshness{Mode: RefreshManual}
}

// CalcInputs are the datasets calculated dataset d reads directly: its
// `from`, then every join's `with`, each once, in order.
func CalcInputs(d Dataset) []string {
	if d.Calc == nil {
		return nil
	}
	out := []string{d.Calc.From}
	for _, st := range d.Calc.Steps {
		if st.Op == OpJoin && st.With != "" && !slices.Contains(out, st.With) {
			out = append(out, st.With)
		}
	}
	return out
}

func engineName(d Dataset) string {
	if d.Engine == nil {
		return ""
	}
	return d.Engine.Name
}
