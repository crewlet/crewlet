package slack_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/slack"
)

// A RENAMED SEAT STILL HEARS THE THREADS IT FOLLOWS.
//
// A follow is the seat's own memory of a conversation, and Slack keeps none on
// a bot's behalf. Keyed on the handle a delivery came in on, the chart
// renaming `swe` to `platform-swe` made it deaf to every thread it had been
// following. A follow is keyed on the handle the seat was CREATED under now
// (ADR-0019), whatever it answers to.
//
// Mutation: key the parser's follows on the delivery's handle again and the
// reply after the rename is dropped.
func TestARenamedSeatStillHearsTheThreadsItFollows(t *testing.T) {
	t.Parallel()
	store := newFollows()
	threads, err := notify.NewThreadTracker(slack.Grammar, store)
	if err != nil {
		t.Fatal(err)
	}
	both := func(handle string) (slack.Seat, bool) {
		switch handle {
		case "swe":
			return slack.Seat{Handle: "swe", BotUserID: botUser, AppID: botApp}, true
		case "platform-swe":
			return slack.Seat{Handle: "platform-swe", Origin: "swe",
				BotUserID: botUser, AppID: botApp}, true
		}
		return slack.Seat{}, false
	}
	p, err := slack.NewParser(both, threads, func() time.Time { return pinned })
	if err != nil {
		t.Fatal(err)
	}

	// BEFORE THE RENAME: named in the thread, so it follows it.
	mention := event("message", map[string]any{
		"thread_ts": "1700000000.000000", "ts": "1700000003.000300",
		"text": "<@" + botUser + "> can you look",
	})
	if got := route(t, p, mention); len(got) != 1 {
		t.Fatalf("the premise: a mention in a thread did not reach the seat: %+v", got)
	}
	// AFTER IT, the same app's deliveries arrive under the new handle, and a
	// reply that does not name it again still reaches it.
	reply := event("message", map[string]any{
		"thread_ts": "1700000000.000000", "ts": "1700000004.000400",
	})
	reply.Handle = "platform-swe"
	got := route(t, p, reply)
	if len(got) != 1 {
		t.Fatalf("a renamed seat was deaf to a thread it followed before the "+
			"rename: %+v", got)
	}
	if got[0].To.Handle != "platform-swe" {
		t.Errorf("routed to %q, want the seat's current handle", got[0].To.Handle)
	}
	if store.reason("platform-swe", "C0ENG", "1700000000.000000") != "" {
		t.Error("a follow was filed under the seat's address rather than its identity")
	}
}
