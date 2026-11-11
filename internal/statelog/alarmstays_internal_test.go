package statelog

import "testing"

// ONLY A NODE-WIDE ROW CLEARS PAST ITS RAISE. [Evaluate] judges a row's stays
// against [Reading.Up], which names kinds rather than logs or entries, and the
// per-log and per-entry readings carry no Up, so a stays there would be ignored.
func TestOnlyANodeWideAlarmClearsPastItsRaise(t *testing.T) {
	t.Parallel()
	stays := 0
	for _, r := range table {
		if r.stays == nil {
			continue
		}
		stays++
		if r.perLog || r.perEntry() {
			t.Errorf("%s clears past its raise and is evaluated per log or per entry, where "+
				"nothing tells it the alarm is up", r.kind)
		}
	}
	if stays == 0 {
		t.Fatal("no row clears past its raise: volume_low's wider clear is gone")
	}
}
