package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
)

// THE COMPANY'S COALESCING KNOBS REACH THE VALUE EVERY SEAT READS.
//
// The regression this exists for: node.Config.BatchOptions was never set, so
// every seat attachment took queue.DefaultBatchOptions and both knobs —
// notification_coalesce_window_seconds and notification_coalesce_max_batch —
// were declared, defaulted, schema'd, validated and documented while being
// read by nothing. Setting either produced a revision that changed nothing an
// operator could observe.
func TestTheCompanysCoalescingKnobsReachTheInbox(t *testing.T) {
	t.Parallel()
	e := &Engine{batch: queue.DefaultBatchOptions()}
	if got := e.batch.EffectiveLinger(); got != 0 {
		t.Fatalf("default linger = %v, want none — the premise", got)
	}

	e.tuneBatching(&Company{Config: &config.Company{
		NotificationCoalesceWindowSeconds: 2.5,
		NotificationCoalesceMaxBatch:      7,
	}})

	if got := e.batch.EffectiveLinger(); got != 2500*time.Millisecond {
		t.Errorf("linger = %v, want the company's 2.5s", got)
	}
	if got := e.batch.EffectiveMaxBatch(); got != 7 {
		t.Errorf("max batch = %d, want the company's 7", got)
	}
}

// AN APPLY MOVES THE VALUE SEATS ARE ALREADY HOLDING.
//
// Every attachment on this node shares one *queue.BatchOptions, which is what
// makes a hot reload land on the next batch instead of only on seats that
// happen to move node afterwards.
func TestAnApplyRetunesTheSeatsAlreadyAttached(t *testing.T) {
	t.Parallel()
	e := &Engine{batch: queue.DefaultBatchOptions()}
	held := e.batch // what a seat attached before the apply is reading

	e.tuneBatching(&Company{Config: &config.Company{
		NotificationCoalesceWindowSeconds: 1,
		NotificationCoalesceMaxBatch:      3,
	}})

	if got := held.EffectiveMaxBatch(); got != 3 {
		t.Errorf("a seat attached before the apply still reads max batch %d", got)
	}
	if got := held.EffectiveLinger(); got != time.Second {
		t.Errorf("a seat attached before the apply still reads linger %v", got)
	}
}

// A NODE WITH NOTHING TO TUNE DOES NOT PANIC. equip runs on every apply,
// including on an engine a test built without a broker.
func TestTuningBatchingWithoutAnythingToTuneIsHarmless(t *testing.T) {
	t.Parallel()
	(&Engine{}).tuneBatching(&Company{Config: &config.Company{}})
	(&Engine{batch: queue.DefaultBatchOptions()}).tuneBatching(nil)
	(&Engine{batch: queue.DefaultBatchOptions()}).tuneBatching(&Company{})
}
