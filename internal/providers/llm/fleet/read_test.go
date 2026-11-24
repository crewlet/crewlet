package fleet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

func reading(window llm.LimitWindow, state llm.LimitState, used float64, seenAgo time.Duration) fleet.Reading {
	u := used
	return fleet.Reading{
		LimitReading: llm.LimitReading{Window: window, Hours: 5, State: state, Used: &u,
			ResetsAt: t0.Add(2 * time.Hour)},
		Seen: t0.Add(-seenAgo),
	}
}

// TestReadDerivesStateFromWhatTheSignInReported holds a member's state and
// share to the sign-in's own reports: sit-outs, readings and refusals, with a
// reading older than half an hour saying nothing.
func TestReadDerivesStateFromWhatTheSignInReported(t *testing.T) {
	sitOut := &fleet.SitOut{Until: t0.Add(40 * time.Minute), Window: llm.LimitSession}
	cases := []struct {
		name  string
		in    fleet.Signals
		state fleet.State
		share int
		known bool
		until time.Time
		why   string
	}{
		{
			name: "a plan's share is the higher of its session and weekly use",
			in: fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{
				reading(llm.LimitSession, llm.LimitOK, 0.314, time.Minute),
				reading(llm.LimitWeekly, llm.LimitOK, 0.526, time.Minute)}},
			state: fleet.Available, share: 53, known: true,
		},
		{
			name: "a window from 80% used is near",
			in: fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{
				reading(llm.LimitSession, llm.LimitOK, 0.81, time.Minute)}},
			state: fleet.Near, share: 81, known: true, why: "Near its 5-hour limit",
		},
		{
			name: "a reached window is at its limit until it resets",
			in: fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{
				reading(llm.LimitWeekly, llm.LimitReached, 1, time.Minute),
				reading(llm.LimitSession, llm.LimitWarning, 0.9, time.Minute)}},
			state: fleet.Limit, share: 100, known: true, until: t0.Add(2 * time.Hour),
			why: "Reached its weekly limit",
		},
		{
			name: "sitting out is at its limit until it is tried again",
			in: fleet.Signals{Kind: fleet.KindPlan, SitOut: sitOut, Readings: []fleet.Reading{
				reading(llm.LimitSession, llm.LimitReached, 1, time.Minute)}},
			state: fleet.Limit, share: 100, known: true, until: sitOut.Until,
			why: "Reached its 5-hour limit",
		},
		{
			name: "a sit-out that ended holds nothing",
			in: fleet.Signals{Kind: fleet.KindPlan,
				SitOut: &fleet.SitOut{Until: t0.Add(-time.Second), Window: llm.LimitSession}},
			state: fleet.Available,
		},
		{
			name: "a reading older than half an hour says nothing",
			in: fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{
				reading(llm.LimitSession, llm.LimitWarning, 0.95, 31*time.Minute)}},
			state: fleet.Available,
		},
		{
			name: "a window that turned over since it was heard reads ok",
			in: fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{{
				LimitReading: llm.LimitReading{Window: llm.LimitSession, State: llm.LimitReached,
					ResetsAt: t0.Add(-time.Minute)},
				Seen: t0.Add(-10 * time.Minute)}}},
			state: fleet.Available,
		},
		{
			name: "a refusal newer than the last answer is signed out",
			in: fleet.Signals{Kind: fleet.KindPlan, Refused: t0.Add(-time.Minute),
				Answered: t0.Add(-time.Hour)},
			state: fleet.SignedOut, why: "Its sign-in was refused",
		},
		{
			name:  "a sign-in known bad without a call is signed out",
			in:    fleet.Signals{Kind: fleet.KindPlan, NotSignedIn: true, Answered: t0.Add(-time.Minute)},
			state: fleet.SignedOut, why: "It is not signed in",
		},
		{
			name: "an answer after the refusal signs it back in",
			in: fleet.Signals{Kind: fleet.KindPlan, Refused: t0.Add(-time.Hour),
				Answered: t0.Add(-time.Minute)},
			state: fleet.Available,
		},
		{
			name: "a key's share is 0 whatever its readings say",
			in: fleet.Signals{Kind: fleet.KindAPI, Readings: []fleet.Reading{
				reading(llm.LimitSession, llm.LimitWarning, 0.95, time.Minute)}},
			state: fleet.Available, share: 0, known: true,
		},
		{
			name:  "a cloud account sitting out is throttled by its quota",
			in:    fleet.Signals{Kind: fleet.KindCloud, SitOut: sitOut},
			state: fleet.Limit, known: true, until: sitOut.Until,
			why: "Throttled by the cloud account's quota",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fleet.Read(tc.in, t0)
			if got.State != tc.state || got.Share != tc.share || got.Known != tc.known ||
				!got.Until.Equal(tc.until) || got.Why != tc.why {
				t.Errorf("Read = %s share %d known %v until %v why %q, want %s share %d known %v until %v why %q",
					got.State, got.Share, got.Known, got.Until, got.Why,
					tc.state, tc.share, tc.known, tc.until, tc.why)
			}
		})
	}
}
