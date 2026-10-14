package chat_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A GESTURE MINTED NOW IS PUBLISHED ON A NODE WHOSE LEDGER HAS LOST ROWS.
//
// Every operation ledger loses rows: its retention sweep deletes what is older
// than [statelog.OpsRetention], and that sweep is the one thing that moves the
// ledger's watermark. From then on the publisher answers `unknown` — and
// publishes NOTHING — for any operation minted before that loss whose row it
// does not hold, and it reads the mint instant off the id itself. An id
// carrying no instant is read as minted before every loss, so a chat whose ids
// carried none would have stopped speaking on every node the day its first
// sweep deleted a row, and no test with a fresh ledger would ever have
// noticed. The loss here is where a real sweep leaves it.
//
// AND A KEYED GESTURE CARRIES ITS KEY'S INSTANT, not the call's: the same post
// whose unit of work began before the loss is answered `unknown`, because a
// re-run of it after the loss is exactly what the ledger can no longer tell
// from its own first copy.
func TestAFreshGestureIsPublishedWhereTheLedgerLostRows(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "eng").Channel.ID
	said := r.say(person("jane"), room, "are we shipping?", "k-1").Message.ID
	r.loseLedgerBefore(time.Now().Add(-statelog.OpsRetention))

	ctx := t.Context()
	topic := "ship it"
	system := chat.Actor{Handle: "retention", Kind: chat.AuthorSystem}
	for _, c := range []struct {
		name  string
		write func() (chat.Written, error)
	}{
		{"post", func() (chat.Written, error) {
			return r.store.Post(ctx, person("jane"), room,
				chat.NewMessage{Body: "anybody there?"})
		}},
		{"post under a key begun now", func() (chat.Written, error) {
			return r.store.Post(ctx, person("jane"), room,
				chat.NewMessage{Body: "still there?", CallKey: statelog.CallKey{
					Seed: "req-now", Since: time.Now(), Tool: "post_message",
				}})
		}},
		{"create a room", func() (chat.Written, error) {
			return r.store.CreateChannel(ctx, person("jane"),
				chat.NewChannel{Name: "standup", Kind: chat.KindPublic})
		}},
		{"open a direct conversation", func() (chat.Written, error) {
			got, _, err := r.store.OpenDirect(ctx, person("jane"),
				[]string{"bob"}, statelog.CallKey{})
			return got, err
		}},
		{"patch the room", func() (chat.Written, error) {
			return r.store.PatchChannel(ctx, person("jane"), room,
				chat.ChannelPatch{Topic: &topic}, statelog.CallKey{})
		}},
		{"join", func() (chat.Written, error) {
			return r.store.Join(ctx, person("bob"), room, statelog.CallKey{})
		}},
		{"react", func() (chat.Written, error) {
			return r.store.React(ctx, person("jane"), room, said, ":eyes:",
				statelog.CallKey{})
		}},
		{"edit", func() (chat.Written, error) {
			return r.store.Edit(ctx, person("jane"), room, said,
				chat.EditMessage{Body: "are we shipping today?"})
		}},
		{"prune", func() (chat.Written, error) {
			return r.store.Prune(ctx, system, statelog.NewOpID(time.Now(), "prune"),
				room, chatAt.Add(-time.Hour))
		}},
	} {
		before, err := r.log.End(ctx)
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		got, err := c.write()
		switch {
		case err != nil:
			t.Fatalf("%s: %v", c.name, err)
		case got.Outcome.Outcome == statelog.OutcomeUnknown:
			t.Fatalf("%s: answered %+v under operation %s — a gesture minted "+
				"now is after every loss this ledger has had, and one answered "+
				"`unknown` here is one this node can never publish", c.name,
				got.Outcome, got.ChangeID)
		}
		if minted, ok := statelog.OpMintedAt(got.ChangeID); !ok {
			t.Fatalf("%s: operation %q carries no mint instant", c.name, got.ChangeID)
		} else if time.Since(minted) > time.Minute {
			t.Fatalf("%s: operation %s was minted at %s, not now", c.name,
				got.ChangeID, minted)
		}
		if after, err := r.log.End(ctx); err != nil || after <= before {
			t.Fatalf("%s: the log's end moved from %d to %d (err %v) — it was "+
				"never published", c.name, before, after, err)
		}
		r.drain()
	}

	stale, err := r.store.Post(ctx, person("jane"), room,
		chat.NewMessage{Body: "from a month ago", CallKey: statelog.CallKey{
			Seed: "req-old", Since: time.Now().Add(-2 * statelog.OpsRetention),
			Tool: "post_message",
		}})
	if err != nil {
		t.Fatalf("the stale post: %v", err)
	}
	if stale.Outcome.Outcome != statelog.OutcomeUnknown || !stale.Outcome.Unvouched {
		t.Fatalf("a post keyed to work begun before the loss answered %+v — its "+
			"id carries its key's instant, and this ledger cannot vouch for it",
			stale.Outcome)
	}
}

// keyed is a key whose unit of work began at [requestedAt], for a call made
// through tool.
func keyed(seed, tool string) statelog.CallKey {
	return statelog.CallKey{Seed: seed, Since: requestedAt, Tool: tool}
}

// EVERY GESTURE MADE AGAIN UNDER ITS KEY IS ONE RECORD, ANSWERED FROM THE ROWS.
//
// The post is not special. A person whose patch, join or reaction answered
// `unknown` sends the same request again, and a turn redelivered after a crash
// makes the same calls again: each has to land under the operation its first
// attempt took, or the second copy is a second record — a reaction toggled
// twice, an edit applied over a later one. And because the ledger answers such
// a retry BEFORE a decision runs, nothing the decision would have read is in
// hand — the room, the message, the topic — so an answer built from the call
// would name a room with no id. Every one of them is read back.
func TestEveryKeyedGestureMadeAgainIsOneRecordAnsweredFromTheRows(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	topic := "the daily"

	var room, said, doomed string
	system := chat.Actor{Handle: "retention", Kind: chat.AuthorSystem}
	eraseOp := statelog.NewOpID(time.Now(), "erase")
	pruneOp := statelog.NewOpID(time.Now(), "prune")
	for _, c := range []struct {
		name string
		// keyed is whether the operation is derived from a key, and so
		// carries the key's instant rather than the clock's.
		keyed bool
		write func() (chat.Written, error)
		check func(t *testing.T, got chat.Written)
	}{
		{"create a room", true, func() (chat.Written, error) {
			return r.store.CreateChannel(t.Context(), person("jane"), chat.NewChannel{
				Name: "standup", Kind: chat.KindPublic,
				Members: members("bob"), CallKey: keyed("req-1", "create_channel"),
			})
		}, func(t *testing.T, got chat.Written) {
			if got.Channel.ID == "" || got.Channel.Name != "standup" ||
				got.Channel.Kind != chat.KindPublic {
				t.Fatalf("answered room %+v, want #standup read back", got.Channel)
			}
			room = got.Channel.ID
		}},
		{"open a direct conversation", true, func() (chat.Written, error) {
			got, created, err := r.store.OpenDirect(t.Context(), person("jane"),
				[]string{"bob"}, keyed("req-2", "send_dm"))
			if err == nil && !created {
				t.Fatal("a retry of the operation that opened the conversation " +
					"reported it was already open — this operation made it")
			}
			return got, err
		}, func(t *testing.T, got chat.Written) {
			want := chat.DirectChannelID([]string{"jane", "bob"}).String()
			if got.Channel.ID != want || got.Channel.Kind != chat.KindDM {
				t.Fatalf("answered room %+v, want the conversation %s", got.Channel, want)
			}
		}},
		{"patch the room", true, func() (chat.Written, error) {
			return r.store.PatchChannel(t.Context(), person("jane"), room,
				chat.ChannelPatch{Topic: &topic}, keyed("req-3", "patch_channel"))
		}, func(t *testing.T, got chat.Written) {
			if got.Channel.ID != room || got.Channel.Topic != topic {
				t.Fatalf("answered room %+v, want #standup with its new topic",
					got.Channel)
			}
		}},
		{"leave", true, func() (chat.Written, error) {
			return r.store.Leave(t.Context(), person("bob"), room, keyed("req-4", "leave_channel"))
		}, nil},
		{"join", true, func() (chat.Written, error) {
			return r.store.Join(t.Context(), person("bob"), room, keyed("req-5", "join_channel"))
		}, nil},
		{"set the members", true, func() (chat.Written, error) {
			return r.store.SetMembers(t.Context(), person("jane"), room,
				members("jane", "bob", "eng"), keyed("req-6", "set_members"))
		}, nil},
		{"post", true, func() (chat.Written, error) {
			return r.store.Post(t.Context(), person("jane"), room, chat.NewMessage{
				Body: "standup in five", CallKey: keyed("req-7", "post_message"),
			})
		}, func(t *testing.T, got chat.Written) {
			if got.Message.Body != "standup in five" {
				t.Fatalf("answered message %+v, want the post read back", got.Message)
			}
			said = got.Message.ID
		}},
		{"edit", true, func() (chat.Written, error) {
			return r.store.Edit(t.Context(), person("jane"), room, said, chat.EditMessage{
				Body: "standup in ten", CallKey: keyed("req-8", "edit_message"),
			})
		}, func(t *testing.T, got chat.Written) {
			if got.Message.ID != said || got.Message.Body != "standup in ten" ||
				got.Message.EditedAt == nil {
				t.Fatalf("answered message %+v, want the edit read back", got.Message)
			}
		}},
		{"react", true, func() (chat.Written, error) {
			return r.store.React(t.Context(), person("bob"), room, said, ":eyes:",
				keyed("req-9", "react_to_message"))
		}, nil},
		{"unreact", true, func() (chat.Written, error) {
			return r.store.Unreact(t.Context(), person("bob"), room, said, ":eyes:",
				keyed("req-10", "react_to_message"))
		}, nil},
		{"delete", true, func() (chat.Written, error) {
			return r.store.Delete(t.Context(), person("jane"), room, said,
				keyed("req-11", "delete_message"))
		}, func(t *testing.T, got chat.Written) {
			if got.Message.ID != said || got.Message.DeletedAt == nil {
				t.Fatalf("answered message %+v, want the tombstone read back",
					got.Message)
			}
		}},
		{"erase", false, func() (chat.Written, error) {
			if doomed == "" {
				doomed = r.say(person("jane"), room, "the root password is hunter2",
					"k-doomed").Message.ID
			}
			return r.store.Erase(t.Context(), operatorFor("jane"), eraseOp, room,
				[]string{doomed}, "a credential in a public room")
		}, nil},
		{"prune", false, func() (chat.Written, error) {
			return r.store.Prune(t.Context(), system, pruneOp, room, chatAt.Add(-time.Hour))
		}, nil},
	} {
		first, err := c.write()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if first.Outcome.Collapsed || first.Outcome.Position.Seq == 0 {
			t.Fatalf("%s: the first attempt answered %+v — it should have "+
				"published a record", c.name, first.Outcome)
		}
		if c.keyed {
			if minted, ok := statelog.OpMintedAt(first.ChangeID); !ok ||
				!minted.Equal(requestedAt) {
				t.Fatalf("%s: operation %s carries %v (minted: %v), want the "+
					"key's own %s", c.name, first.ChangeID, minted, ok, requestedAt)
			}
		}
		r.drain()
		before, err := r.log.End(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}

		again, err := c.write()
		switch {
		case err != nil:
			t.Fatalf("%s made again: %v", c.name, err)
		case again.ChangeID != first.ChangeID:
			t.Fatalf("%s made again derived %s, and the first attempt %s — the "+
				"same call is the same operation", c.name, again.ChangeID,
				first.ChangeID)
		case again.Outcome.Outcome != statelog.OutcomeApplied || !again.Outcome.Collapsed:
			t.Fatalf("%s made again answered %+v, want applied and collapsed",
				c.name, again.Outcome)
		case again.Outcome.Position != first.Outcome.Position:
			t.Fatalf("%s made again answered at %s, and its record is at %s",
				c.name, again.Outcome.Position, first.Outcome.Position)
		case again.Channel.ID == "":
			t.Fatalf("%s made again answered a room with no id — the ledger "+
				"answered it before a decision read one, so it is read back",
				c.name)
		case again.Revision == 0:
			t.Fatalf("%s made again answered revision 0", c.name)
		}
		if after, err := r.log.End(t.Context()); err != nil || after != before {
			t.Fatalf("%s made again moved the log's end from %d to %d (err %v) "+
				"— a second record of one gesture", c.name, before, after, err)
		}
		if c.check != nil {
			c.check(t, again)
		}
	}
}

// ONE KEY AND ONE ADDRESS ARE ONE ROOM; TWO KEYS ARE TWO CLAIMS ON IT.
//
// A created room's id is derived from the key, so a create answered as its
// first attempt's copy names the room that attempt made. But two
// DIFFERENT callers naming one address must derive two different ids: the one
// that loses the race is refused the name, and it must not have written a
// room under the id the winner chose.
func TestACreatesRoomIDIsTheKeysAndOnlyTheKeys(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))

	create := func(actor chat.Actor, seed string) (chat.Written, error) {
		return r.store.CreateChannel(t.Context(), actor, chat.NewChannel{
			Name: "launch", Kind: chat.KindPublic,
			CallKey: keyed(seed, "create_channel"),
		})
	}
	first, err := create(person("jane"), "req-jane")
	if err != nil {
		t.Fatalf("the create: %v", err)
	}
	r.drain()
	again, err := create(person("jane"), "req-jane")
	switch {
	case err != nil:
		t.Fatalf("the create made again: %v — it is the same operation, not a "+
			"second claim on the name", err)
	case !again.Outcome.Collapsed || again.Channel.ID != first.Channel.ID:
		t.Fatalf("the create made again answered %+v in room %s, want the first "+
			"attempt's %s, collapsed", again.Outcome, again.Channel.ID,
			first.Channel.ID)
	}

	if _, err := create(person("bob"), "req-bob"); err == nil {
		t.Fatal("a second caller took a name the company already holds")
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE name = ?`,
		"launch"); got != 1 {
		t.Fatalf("the company holds %d rooms called #launch, want 1", got)
	}
}
