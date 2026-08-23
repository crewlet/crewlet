package turn_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turn"
)

func TestDepthIsCheckedAtTheLimitNotPastIt(t *testing.T) {
	t.Parallel()
	// Off by one here means every delegation chain runs one hop longer or
	// shorter than configured, and neither is visible from outside.
	if err := turn.CheckDepth(2, 3); err != nil {
		t.Errorf("depth 2 under a limit of 3 was refused: %v", err)
	}
	err := turn.CheckDepth(3, 3)
	if err == nil {
		t.Fatal("depth 3 at a limit of 3 was allowed")
	}
	var de *turn.DepthError
	if !errors.As(err, &de) || de.Depth != 3 || de.Limit != 3 {
		t.Errorf("err = %v, want a *DepthError carrying both numbers", err)
	}
	if err := turn.CheckDepth(4, 3); err == nil {
		t.Error("depth past the limit was allowed")
	}
}

func TestANonPositiveLimitDisablesTheCap(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		if err := turn.CheckDepth(9999, limit); err != nil {
			t.Errorf("limit %d did not disable the cap: %v", limit, err)
		}
	}
}

func TestTheStallGuardNeedsTwoIdenticalRounds(t *testing.T) {
	t.Parallel()
	var s turn.StallDetector
	s.Observe("a")
	if s.ShouldAbort() {
		t.Error("one observation aborted; a single sample cannot show sameness")
	}
	s.Observe("a")
	if !s.ShouldAbort() {
		t.Error("two identical observations did not abort")
	}
}

func TestChangedWorkIsNotAStall(t *testing.T) {
	t.Parallel()
	// The counterfactual that keeps the guard from being "abort after two".
	var s turn.StallDetector
	s.Observe("a")
	s.Observe("b")
	if s.ShouldAbort() {
		t.Error("two DIFFERENT artifacts read as a stall")
	}
	// And the run must be CONSECUTIVE: an early repeat that was broken by
	// progress is not a stall.
	s.Observe("a")
	if s.ShouldAbort() {
		t.Error("a repeat separated by progress read as a stall")
	}
}

func TestAStallThatStartsLaterIsStillCaught(t *testing.T) {
	t.Parallel()
	// The common shape, and the one that says the guard reads the LATEST
	// rounds rather than the first: round one produced something, and then
	// the turn got stuck.
	//
	// Found by mutation — reading the first N instead of the last N gives
	// the same answer for every other case in this file, so which end the
	// guard looks at was asserted nowhere.
	var s turn.StallDetector
	s.Observe("first attempt")
	s.Observe("stuck")
	s.Observe("stuck")
	if !s.ShouldAbort() {
		t.Error("a stall that began after the first round was not caught")
	}
}

func TestAHigherThresholdWaitsLonger(t *testing.T) {
	t.Parallel()
	s := turn.StallDetector{Threshold: 3}
	s.Observe("a")
	s.Observe("a")
	if s.ShouldAbort() {
		t.Error("a threshold of 3 aborted after 2")
	}
	s.Observe("a")
	if !s.ShouldAbort() {
		t.Error("a threshold of 3 did not abort after 3")
	}
}

func TestTheZeroDetectorDefaultsToTwo(t *testing.T) {
	t.Parallel()
	// A zero threshold must not mean "abort immediately" — that would kill
	// every turn on its first self_iterate.
	var s turn.StallDetector
	if s.ShouldAbort() {
		t.Fatal("a detector that has observed nothing wants to abort")
	}
	s.Observe("a")
	if s.ShouldAbort() {
		t.Error("the zero detector aborted after one observation")
	}
}

func TestResetClearsTheRun(t *testing.T) {
	t.Parallel()
	var s turn.StallDetector
	s.Observe("a")
	s.Reset()
	s.Observe("a")
	if s.ShouldAbort() {
		t.Error("Reset did not clear the earlier observation")
	}
}

func TestTheArtifactHashIsStableAndDiscriminating(t *testing.T) {
	t.Parallel()
	if turn.ArtifactHash("x") != turn.ArtifactHash("x") {
		t.Error("the same text hashed differently")
	}
	if turn.ArtifactHash("x") == turn.ArtifactHash("y") {
		t.Error("different text hashed the same")
	}
	// Invalid UTF-8 must hash rather than panic or normalise: a tool result
	// can carry arbitrary bytes, and two different broken outputs read as
	// one stall if they collapse to the same replacement character.
	a := turn.ArtifactHash(string([]byte{0xff, 0xfe}))
	b := turn.ArtifactHash(string([]byte{0xff, 0xfd}))
	if a == b {
		t.Error("two different invalid-UTF-8 artifacts hashed identically")
	}
	if len(a) != 16 {
		t.Errorf("hash length = %d, want 16", len(a))
	}
}
