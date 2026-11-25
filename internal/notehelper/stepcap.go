package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// stepCap ends a run at its Nth model response (-step-cap), counted over every
// turn of the run: the adapter counts the responses ([stepCap.step]), and the
// cap writes its marker (-cap-marker) and then asks the harness to interrupt
// the turn, so the run's collection tells the helper's cap from any other
// interrupt. A turn a note opens after the cap fired is interrupted as it
// starts ([stepCap.begin]). PER RUN, never per turn: the engine holds the
// run's whole count to the same cap, so a cap a note restarted would fail an
// honest run. Codex has no cap of its own (CODEX-MEASURE §17.1).
type stepCap struct {
	n      int
	marker string
	fire   chan string

	mu    sync.Mutex
	count int
	fired string
}

func newStepCap(n int, marker string) *stepCap {
	return &stepCap{n: n, marker: marker, fire: make(chan string, 1)}
}

// step counts one model response of turn, never blocking: the adapter calls
// it from the harness's reader. The run's count goes on across its turns.
func (c *stepCap) step(turn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	c.fireLocked(turn)
}

// begin is a turn started, never blocking: one started once the run's cap is
// spent fires as it starts, before its first response is counted.
func (c *stepCap) begin(turn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fireLocked(turn)
}

// fireLocked fires turn where the run's count is at its cap and turn has not
// fired yet; c.mu is held.
func (c *stepCap) fireLocked(turn string) {
	if c.count < c.n || c.fired == turn {
		return
	}
	c.fired = turn
	select {
	case c.fire <- turn:
	default:
	}
}

// unfire lets turn's cap fire again at its next response: the interrupt
// failed, and the cap is a bound, so it is asked again.
func (c *stepCap) unfire(turn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fired == turn {
		c.fired = ""
	}
}

// interrupter is the harness as the cap needs it.
type interrupter interface {
	interrupt(ctx context.Context, turn string) error
}

// run marks and interrupts each turn the cap fires on, until ctx ends.
func (c *stepCap) run(ctx context.Context, h interrupter, logger *log.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case turn := <-c.fire:
			if c.marker != "" {
				if err := writeMarker(c.marker, turn); err != nil {
					logger.Printf("writing the cap marker -cap-marker %s: %v", c.marker, err)
				}
			}
			err := h.interrupt(ctx, turn)
			switch {
			case err == nil, errors.Is(err, errNotNow), ctx.Err() != nil:
			default:
				c.unfire(turn)
				logger.Printf("interrupting turn %s at its step cap of %d, asked again at its next step: %v", turn, c.n, err)
			}
		}
	}
}

// writeMarker writes the cap marker, the turn it ended, by a rename, so a
// reader finds it whole or not at all.
func writeMarker(path, turn string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cap-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.WriteString(turn + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
