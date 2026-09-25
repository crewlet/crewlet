package livestate

import (
	"time"
)

// A SEAT A PERSON PAUSED, as the dashboard draws it.
//
// The pause itself is a coordination record every node reads (coord.SeatPause);
// what this projection holds is the account of it a screen needs — who paused
// the seat, when, why, and whether they also stopped its turn — moved by the
// `seat_paused` and `seat_resumed` events the winning writer publishes, and
// seeded from the record itself at boot, because a pause taken last week is in
// no event this process will ever hear.
//
// It is an INPUT to the seat's state, not the state: whether a paused seat
// reads as stopped, and above what, is the one seat-state vocabulary's to say,
// and a screen reads it from there rather than deriving it here.

// Paused is who paused a seat, when, and why.
type Paused struct {
	// By is the person who paused it — the seat their credential is bound
	// to — or the credential itself when nobody is bound to it.
	By string `json:"by"`

	// At is when the seat was paused, RFC 3339 in UTC.
	At string `json:"at"`

	// Reason is why, in the pauser's words; empty when none was given.
	Reason string `json:"reason,omitempty"`

	// StopRunning is whether the pause also ended the turn the seat was on
	// rather than letting it finish.
	StopRunning bool `json:"stop_running"`
}

func (p *Paused) clone() *Paused {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// SeedPause is one paused seat as the coordination record holds it, named by
// the ROLE this projection keys seats by.
type SeedPause struct {
	Role   string
	Paused Paused
}

// applyPause moves a seat's pause for one event, reporting whether it did.
//
// ORDERED BY THE EVENT'S OWN INSTANT against the last one that moved it, as
// every other part of a seat is: a pause and its resume travel on different
// subjects, and a resume that arrived first must not be undone by the pause it
// lifted.
func (s *LiveState) applyPause(agent *agentLive, env Envelope, payload map[string]any) bool {
	var next *Paused
	switch env.Type {
	case "seat_paused":
		by := str(payload, "paused_by_seat")
		if by == "" {
			by = str(payload, "paused_by")
		}
		next = &Paused{
			By: by, At: str(payload, "paused_at"), Reason: str(payload, "reason"),
			StopRunning: flag(payload, "stop_running"),
		}
	case "seat_resumed":
	default:
		return false
	}
	at := newStamp(env.Timestamp)
	if !at.empty() && !agent.pausedAt.empty() && at.before(agent.pausedAt) {
		return false
	}
	if !at.empty() {
		agent.pausedAt = at
	}
	before := agent.paused
	agent.paused = next
	return !samePaused(before, next)
}

// SeedPauses sets each named seat's pause from the coordination record,
// reporting the roles it moved.
//
// A seat the stream has already moved keeps what the stream said: that is
// newer than any record read before it arrived.
func (s *LiveState) SeedPauses(pauses []SeedPause) Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	var change Change
	for _, p := range pauses {
		if p.Role == "" {
			continue
		}
		agent := s.ensureAgent(p.Role)
		if !agent.pausedAt.empty() {
			continue
		}
		paused := p.Paused
		agent.paused = &paused
		if at, err := time.Parse(time.RFC3339Nano, paused.At); err == nil {
			agent.pausedAt = newStamp(at.UTC().Format(time.RFC3339Nano))
		}
		change.agentMoved(p.Role)
	}
	return change
}

func samePaused(a, b *Paused) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
