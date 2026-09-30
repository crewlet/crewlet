package mattermost_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
)

// A RENAMED SEAT STILL HEARS THE THREADS IT FOLLOWS.
//
// A follow is the seat's own memory of a conversation, and no chat backend
// keeps one on its behalf. Keyed on the handle a delivery came in on, the
// chart renaming `swe` to `platform-swe` made it deaf to every thread it had
// been following — a reply that did not name it again was dropped, and the
// seat never learned the conversation had moved on. A follow is keyed on the
// handle the seat was CREATED under now (ADR-0019), whatever it answers to.
//
// Mutation: key the parser's follows on the delivery's handle again and the
// reply after the rename is dropped.
func TestARenamedSeatStillHearsTheThreadsItFollows(t *testing.T) {
	t.Parallel()
	store := newFollows()
	tracker, err := notify.NewThreadTracker(mattermost.Grammar, store)
	if err != nil {
		t.Fatalf("NewThreadTracker: %v", err)
	}
	renamed := mattermost.Seat{Handle: "platform-swe", Origin: "swe",
		Username: "agent-swe", UserID: "bot-1"}
	p, err := mattermost.NewParser(seats(seat, renamed), tracker,
		func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	reply := func(to, text, id string) types.RawWebhook {
		w := post(func(_, pp map[string]any) {
			pp["root_id"], pp["message"], pp["id"] = "root-1", text, id
		})
		w.Handle = to
		return w
	}

	// BEFORE THE RENAME: named in the thread, so it follows it.
	if _, ok := parseOne(t, p, reply("swe", "@agent-swe any thoughts?", "p2")); !ok {
		t.Fatal("the premise: a mention in the thread did not wake the seat")
	}
	// AFTER IT, the same seat's socket delivers under its new handle, and a
	// reply that does not name it again still reaches it.
	got, ok := parseOne(t, p, reply("platform-swe", "still there?", "p3"))
	if !ok {
		t.Fatal("a renamed seat was deaf to a thread it followed before the rename")
	}
	if got.To.Handle != "platform-swe" {
		t.Errorf("routed to %q, want the seat's current handle", got.To.Handle)
	}
	if store.rows[key(mattermost.Backend, "platform-swe", "C1", "root-1")] != "" {
		t.Error("a follow was filed under the seat's address rather than its identity")
	}
}
