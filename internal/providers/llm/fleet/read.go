package fleet

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Kind is how a member signs in. Only plan versus not-plan matters to the
// balancer; the rest names a sit-out in a person's words.
type Kind string

const (
	// KindPlan signs in on a plan (auth.mode subscription).
	KindPlan Kind = "plan"
	// KindAPI signs in with an API key, billed per use.
	KindAPI Kind = "api"
	// KindCloud signs in through a cloud provider's account.
	KindCloud Kind = "cloud"
	// KindGateway signs in through a gateway.
	KindGateway Kind = "gateway"
)

// Kinds is the closed set, in the order the Models page names them.
var Kinds = []Kind{KindPlan, KindAPI, KindCloud, KindGateway}

// Valid reports whether k is one of [Kinds].
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Reading is one of a sign-in's windows as a call last reported it.
type Reading struct {
	llm.LimitReading
	// Seen is when the call that reported it was read.
	Seen time.Time
}

// SitOut is the window a member's calls are held by: when it is tried
// again, and which limit it is.
type SitOut struct {
	Until  time.Time
	Window llm.LimitWindow
}

// Signals is what a node knows of one member's sign-in at one instant.
type Signals struct {
	Key  string
	Kind Kind
	// Readings are the sign-in's windows as its calls last reported them.
	Readings []Reading
	// SitOut is the window its calls are held by, nil for none.
	SitOut *SitOut
	// Refused is when its vendor last refused its sign-in and Answered when a
	// call on it last answered, zero for never.
	Refused, Answered time.Time
	// NotSignedIn is a sign-in known bad without a call: nothing signs it in,
	// its login is missing or expired, or its credential's lifetime passed.
	NotSignedIn bool
	// LastServed and Full are the balancer's own ([Member]).
	LastServed time.Time
	Full       bool
}

// Read is the member s describes, at now (see the package doc for the rule).
func Read(s Signals, now time.Time) Member {
	m := Member{Key: s.Key, Plan: s.Kind == KindPlan, State: Available,
		LastServed: s.LastServed, Full: s.Full}
	fresh := freshReadings(s.Readings, now)
	if m.Plan {
		m.Share, m.Known = share(fresh)
	} else {
		// No spend cap the engine knows: a key's state is its sit-outs alone.
		m.Known, fresh = true, nil
	}
	switch {
	case s.SitOut != nil && s.SitOut.Until.After(now):
		m.State, m.Until, m.Why = Limit, s.SitOut.Until, reachedWhy(s.Kind, s.SitOut.Window, fresh)
	case !s.Refused.IsZero() && s.Refused.After(s.Answered):
		m.State, m.Why = SignedOut, "Its sign-in was refused"
	case s.NotSignedIn:
		m.State, m.Why = SignedOut, "It is not signed in"
	default:
		if r, ok := worst(fresh); ok {
			m.Why = reachedWhy(s.Kind, r.Window, fresh)
			m.State, m.Until = Limit, r.ResetsAt
			if r.State == llm.LimitWarning {
				m.State, m.Until, m.Why = Near, time.Time{}, nearWhy(r, fresh)
			}
		}
	}
	return m
}

// freshReadings is the session and weekly windows heard within
// [llm.LimitStaleAfter], read at now: an older reading says nothing.
func freshReadings(readings []Reading, now time.Time) []Reading {
	var out []Reading
	for _, r := range readings {
		if r.Window != llm.LimitSession && r.Window != llm.LimitWeekly {
			continue
		}
		if now.Sub(r.Seen) > llm.LimitStaleAfter {
			continue
		}
		out = append(out, Reading{LimitReading: r.LimitReading.At(now), Seen: r.Seen})
	}
	return out
}

// share is a plan's usage share in whole points: the higher of its windows'
// use. Unknown where no fresh window says how much is used.
func share(fresh []Reading) (int, bool) {
	points, known := 0, false
	for _, r := range fresh {
		if r.Used == nil {
			continue
		}
		if p := int(math.Round(*r.Used * 100)); !known || p > points {
			points, known = p, true
		}
	}
	return points, known
}

// worst is the fresh window furthest along: reached over warning, false where
// every one is ok.
func worst(fresh []Reading) (Reading, bool) {
	var (
		found Reading
		ok    bool
	)
	for _, r := range fresh {
		switch {
		case r.State == llm.LimitReached && (!ok || found.State != llm.LimitReached):
			found, ok = r, true
		case r.State == llm.LimitWarning && !ok:
			found, ok = r, true
		}
	}
	return found, ok
}

// reachedWhy names the limit a member met.
func reachedWhy(kind Kind, window llm.LimitWindow, fresh []Reading) string {
	switch {
	case kind == KindCloud:
		return "Throttled by the cloud account's quota"
	case kind == KindAPI:
		return "Throttled by the key's rate limit"
	case kind == KindGateway:
		return "Throttled by the gateway's rate limit"
	case window == llm.LimitWeekly:
		return "Reached its weekly limit"
	case window == llm.LimitSession:
		return "Reached its " + sessionName(fresh) + " limit"
	case window == llm.LimitModel:
		return "Reached its model limit"
	}
	return "Reached its usage limit"
}

// nearWhy names the window a near member is close to.
func nearWhy(r Reading, fresh []Reading) string {
	name := "weekly"
	if r.Window == llm.LimitSession {
		name = sessionName(fresh)
	}
	return "Near its " + name + " limit"
}

// sessionName is the session window by its length ("5-hour") where a reading
// names one, else "session".
func sessionName(fresh []Reading) string {
	for _, r := range fresh {
		if r.Window == llm.LimitSession && r.Hours > 0 {
			return fmt.Sprintf("%d-hour", r.Hours)
		}
	}
	return "session"
}
