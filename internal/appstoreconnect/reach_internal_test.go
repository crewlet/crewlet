package appstoreconnect

import (
	"testing"
	"time"
)

// A FRESH READ IS NEVER ANSWERED BY ONE THAT BEGAN BEFORE IT WAS ASKED: Check
// now pressed while a read is in flight reads again, so an app added at Apple
// just before the press is in its answer; a read that began after the press
// answers it, and any other caller takes a list read within reachTTL.
//
// Mutation: answer a fresh caller with any read that ended while it waited,
// and the press takes the read already in flight.
func TestAFreshReadIsAnsweredOnlyByALaterRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	// One read had begun when the caller asked, and it ended while it waited.
	inFlight := &team{started: 1, listed: 1, apps: []appInfo{{ID: "111"}}, appsAt: now}
	if inFlight.answers(true, 1, now) {
		t.Error("a fresh caller took the read already in flight when it asked")
	}
	if !inFlight.answers(false, 1, now) {
		t.Error("a caller that is not fresh read again inside reachTTL")
	}
	// A read that began after the caller asked ended while it waited.
	later := &team{started: 2, listed: 2, apps: []appInfo{{ID: "111"}}, appsAt: now}
	if !later.answers(true, 1, now) {
		t.Error("a fresh caller read again after a read it waited for began and ended")
	}
	if stale := (&team{started: 1, listed: 1, appsAt: now.Add(-reachTTL)}); stale.answers(false, 1, now) {
		t.Error("a list older than reachTTL answered")
	}
}
