package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// deadLettered is the one feed_dead_lettered alarm of a report over the given
// logs, and whether it was raised.
func deadLettered(domains ...statelog.DomainInputs) (statelog.Alarm, bool) {
	rep := statelog.NewReport(statelog.ReportInputs{NodeID: "node-1", At: reportAt,
		RegisterReadable: true, ReplayWindow: replayWindow, Domains: domains})
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindFeedDeadLettered {
			return a, true
		}
	}
	return statelog.Alarm{}, false
}

// FEED_DEAD_LETTERED IS RAISED FROM A LOG'S FIRST DEAD LETTER AND CLEARS WITH
// ITS LAST.
//
// Every entry is a wake nobody has had, so one is enough: the alarm names the
// log whose feed holds it, and a log whose feed holds none — every entry
// replayed, or let go — raises nothing.
//
// Mutation: raise it past a count of entries rather than from the first, or
// raise it for a log that holds none, and this is red.
func TestFeedDeadLetteredIsRaisedFromTheFirstEntryAndClearsWithTheLast(t *testing.T) {
	t.Parallel()
	quiet := healthyDomain("tracker", true)
	if a, raised := deadLettered(quiet); raised {
		t.Fatalf("a log whose feed holds no dead letter raised %q", a.Detail)
	}
	held := healthyDomain("tracker", true)
	held.FeedDeadLetters = []statelog.FeedDeadLetter{{Age: 2 * time.Hour, ExpiresIn: 30*24*time.Hour - 2*time.Hour}}
	a, raised := deadLettered(held, healthyDomain("pages", true))
	if !raised {
		t.Fatal("a log whose feed holds one dead letter raised no feed_dead_lettered")
	}
	if len(a.Domains) != 1 || logOf(a.Domains[0]) != "tracker" {
		t.Errorf("the alarm stands on %+v, want the tracker's log alone", a.Domains)
	}
	for _, want := range []string{"1 dead letter(s)", "0 of them expiring within a day",
		"one kept 2h ago, expiring in 29d 22h"} {
		if !strings.Contains(a.Detail, want) {
			t.Errorf("the detail %q does not say %q", a.Detail, want)
		}
	}
}

// ITS DETAIL NAMES EACH ENTRY'S AGE AND THE TIME IT HAS LEFT, the soonest to
// expire first, AND COUNTS WHAT EXPIRES WITHIN A DAY — from the reading of
// now, so the count moves as the entries age.
//
// An entry within a day of its retention is a wake about to be lost for good,
// which is what an operator acts on first; past the five it names, the detail
// says how many more there are rather than burying them in one line.
//
// Mutation: count what expires within a day off the kept instant rather than
// the time left, or name the entries in the order kept rather than by expiry,
// and this is red.
func TestFeedDeadLetteredNamesTheSoonestToExpireAndCountsWhatExpiresWithinADay(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	d := healthyDomain("chat", true)
	d.FeedDeadLetters = []statelog.FeedDeadLetter{
		{Age: time.Hour, ExpiresIn: 30*day - time.Hour},
		{Age: 29*day + 21*time.Hour, ExpiresIn: 3 * time.Hour},
		{Age: 5 * day, ExpiresIn: 25 * day},
		{Age: 29*day + 4*time.Hour, ExpiresIn: 20 * time.Hour},
		{Age: 10 * day, ExpiresIn: 20 * day},
		{Age: 2 * day, ExpiresIn: 28 * day},
	}
	a, raised := deadLettered(d)
	if !raised {
		t.Fatal("a log holding six dead letters raised nothing")
	}
	for _, want := range []string{"6 dead letter(s)", "2 of them expiring within a day",
		"one kept 29d 21h ago, expiring in 3h", "one kept 29d 4h ago, expiring in 20h", "and 1 more"} {
		if !strings.Contains(a.Detail, want) {
			t.Errorf("the detail %q does not say %q", a.Detail, want)
		}
	}
	if soon, later := strings.Index(a.Detail, "expiring in 3h"), strings.Index(a.Detail, "expiring in 20h"); soon > later {
		t.Errorf("the detail names the entry with 20h left before the one with 3h: %q", a.Detail)
	}
	if strings.Contains(a.Detail, "expiring in 29d 23h") {
		t.Errorf("the detail names the entry with the most time left, past the five soonest: %q", a.Detail)
	}
}
