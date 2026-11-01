package chatlet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
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
	system := chatlet.Actor{Handle: "retention", Kind: chatlet.AuthorSystem}
	for _, c := range []struct {
		name  string
		write func() (chatlet.Written, error)
	}{
		{"post", func() (chatlet.Written, error) {
			return r.store.Post(ctx, person("jane"), room,
				chatlet.NewMessage{Body: "anybody there?"})
		}},
		{"post under a key begun now", func() (chatlet.Written, error) {
			return r.store.Post(ctx, keyed(person("jane"),
				statelog.NewOpID(time.Now(), "")), room,
				chatlet.NewMessage{Body: "still there?"})
		}},
		{"create a room", func() (chatlet.Written, error) {
			return r.store.CreateChannel(ctx, person("jane"),
				chatlet.NewChannel{Name: "standup", Kind: chatlet.KindPublic})
		}},
		{"open a direct conversation", func() (chatlet.Written, error) {
			got, _, err := r.store.OpenDirect(ctx, person("jane"),
				[]string{"bob"})
			return got, err
		}},
		{"patch the room", func() (chatlet.Written, error) {
			return r.store.PatchChannel(ctx, person("jane"), room,
				chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{})
		}},
		{"join", func() (chatlet.Written, error) {
			return r.store.Join(ctx, person("bob"), room)
		}},
		{"react", func() (chatlet.Written, error) {
			return r.store.React(ctx, person("jane"), room, said, ":eyes:")
		}},
		{"edit", func() (chatlet.Written, error) {
			return r.store.Edit(ctx, person("jane"), room, said,
				chatlet.EditMessage{Body: "are we shipping today?"})
		}},
		{"prune", func() (chatlet.Written, error) {
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

	stale, err := r.store.Post(ctx, keyed(person("jane"),
		statelog.NewOpID(time.Now().Add(-2*statelog.OpsRetention), "")), room,
		chatlet.NewMessage{Body: "from a month ago"})
	if err != nil {
		t.Fatalf("the stale post: %v", err)
	}
	if stale.Outcome.Outcome != statelog.OutcomeUnknown || !stale.Outcome.Unvouched {
		t.Fatalf("a post keyed to work begun before the loss answered %+v — its "+
			"id carries its key's instant, and this ledger cannot vouch for it",
			stale.Outcome)
	}
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
	system := chatlet.Actor{Handle: "retention", Kind: chatlet.AuthorSystem}
	eraseOp := statelog.NewOpID(time.Now(), "erase")
	pruneOp := statelog.NewOpID(time.Now(), "prune")
	for _, c := range []struct {
		name string
		// keyed is whether the operation is derived from a key, and so
		// carries the key's instant rather than the clock's.
		keyed bool
		write func() (chatlet.Written, error)
		check func(t *testing.T, got chatlet.Written)
	}{
		{"create a room", true, func() (chatlet.Written, error) {
			return r.store.CreateChannel(t.Context(), keyed(person("jane"),
				requested("req-1")), chatlet.NewChannel{
				Name: "standup", Kind: chatlet.KindPublic, Members: members("bob"),
			})
		}, func(t *testing.T, got chatlet.Written) {
			if got.Channel.ID == "" || got.Channel.Name != "standup" ||
				got.Channel.Kind != chatlet.KindPublic {
				t.Fatalf("answered room %+v, want #standup read back", got.Channel)
			}
			room = got.Channel.ID
		}},
		{"open a direct conversation", true, func() (chatlet.Written, error) {
			got, created, err := r.store.OpenDirect(t.Context(),
				keyed(person("jane"), requested("req-2")), []string{"bob"})
			if err == nil && !created {
				t.Fatal("a retry of the operation that opened the conversation " +
					"reported it was already open — this operation made it")
			}
			return got, err
		}, func(t *testing.T, got chatlet.Written) {
			want := chatlet.DirectChannelID([]string{"jane", "bob"}).String()
			if got.Channel.ID != want || got.Channel.Kind != chatlet.KindDM {
				t.Fatalf("answered room %+v, want the conversation %s", got.Channel, want)
			}
		}},
		{"patch the room", true, func() (chatlet.Written, error) {
			return r.store.PatchChannel(t.Context(), keyed(person("jane"),
				requested("req-3")), room, chatlet.ChannelPatch{Topic: &topic},
				chatlet.ChannelAuthority{})
		}, func(t *testing.T, got chatlet.Written) {
			if got.Channel.ID != room || got.Channel.Topic != topic {
				t.Fatalf("answered room %+v, want #standup with its new topic",
					got.Channel)
			}
		}},
		{"leave", true, func() (chatlet.Written, error) {
			return r.store.Leave(t.Context(), keyed(person("bob"), requested("req-4")), room)
		}, nil},
		{"join", true, func() (chatlet.Written, error) {
			return r.store.Join(t.Context(), keyed(person("bob"), requested("req-5")), room)
		}, nil},
		{"set the members", true, func() (chatlet.Written, error) {
			return r.store.SetMembers(t.Context(), keyed(person("jane"),
				requested("req-6")), room, members("jane", "bob", "eng"), nil)
		}, nil},
		{"post", true, func() (chatlet.Written, error) {
			return r.store.Post(t.Context(), keyed(person("jane"), requested("req-7")),
				room, chatlet.NewMessage{Body: "standup in five"})
		}, func(t *testing.T, got chatlet.Written) {
			if got.Message.Body != "standup in five" {
				t.Fatalf("answered message %+v, want the post read back", got.Message)
			}
			said = got.Message.ID
		}},
		{"edit", true, func() (chatlet.Written, error) {
			return r.store.Edit(t.Context(), keyed(person("jane"), requested("req-8")),
				room, said, chatlet.EditMessage{Body: "standup in ten"})
		}, func(t *testing.T, got chatlet.Written) {
			if got.Message.ID != said || got.Message.Body != "standup in ten" ||
				got.Message.EditedAt == nil {
				t.Fatalf("answered message %+v, want the edit read back", got.Message)
			}
		}},
		{"react", true, func() (chatlet.Written, error) {
			return r.store.React(t.Context(), keyed(person("bob"), requested("req-9")),
				room, said, ":eyes:")
		}, nil},
		{"unreact", true, func() (chatlet.Written, error) {
			return r.store.Unreact(t.Context(), keyed(person("bob"), requested("req-10")),
				room, said, ":eyes:")
		}, nil},
		{"delete", true, func() (chatlet.Written, error) {
			return r.store.Delete(t.Context(), keyed(person("jane"), requested("req-11")),
				room, said, chatlet.MessageAuthority{})
		}, func(t *testing.T, got chatlet.Written) {
			if got.Message.ID != said || got.Message.DeletedAt == nil {
				t.Fatalf("answered message %+v, want the tombstone read back",
					got.Message)
			}
		}},
		{"set the retention", true, func() (chatlet.Written, error) {
			days := 90
			return r.store.SetRetention(t.Context(), keyed(credential("token:ops"),
				requested("req-12")), room, &days)
		}, func(t *testing.T, got chatlet.Written) {
			if got.Channel.ID != room || got.Channel.RetentionDays == nil ||
				*got.Channel.RetentionDays != 90 {
				t.Fatalf("answered room %+v, want #standup kept for 90 days",
					got.Channel)
			}
		}},
		{"erase", false, func() (chatlet.Written, error) {
			if doomed == "" {
				doomed = r.say(person("jane"), room, "the root password is hunter2",
					"k-doomed").Message.ID
			}
			return r.store.Erase(t.Context(), viaToken("jane"), eraseOp, room,
				[]string{doomed}, "a credential in a public room")
		}, nil},
		{"prune", false, func() (chatlet.Written, error) {
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
// A created room's id is derived from its operation, which is the key's, so a
// create answered as its first attempt's copy names the room that attempt
// made. But two DIFFERENT callers naming one address must derive two
// different ids: the one that loses the race is refused the name, and it must
// not have written a room under the id the winner chose.
func TestACreatesRoomIDIsTheKeysAndOnlyTheKeys(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))

	create := func(actor chatlet.Actor, seed string) (chatlet.Written, error) {
		return r.store.CreateChannel(t.Context(), keyed(actor, requested(seed)),
			chatlet.NewChannel{Name: "launch", Kind: chatlet.KindPublic})
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

// A KEY SENT WITH ANOTHER ASK IS ANOTHER OPERATION.
//
// The ledger answers an operation it already holds BEFORE the gesture is
// decided, so an id derived from the key, the verb and the object alone would
// make the same key sent with other words, another topic or another emoji the
// first gesture's retry: answered from the ledger with nothing of the second
// written — a change reported as made and silently dropped. What the gesture
// ASKS is in the id ([statelog.KeyedOpID]), so a retry, which resends the
// same ask, is one operation and anything else is another.
func TestAKeySentWithAnotherAskIsAnotherOperation(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch", "bob").Channel.ID
	said := r.say(person("jane"), room, "are we shipping?", "k-said").Message.ID
	key := requested("one-key")
	jane := keyed(person("jane"), key)
	first, second := "the daily", "the weekly"
	keep, drop := 90, 30
	for _, c := range []struct {
		name  string
		write func(ask int) (chatlet.Written, error)
	}{
		{"a post", func(ask int) (chatlet.Written, error) {
			return r.store.Post(t.Context(), jane, room,
				chatlet.NewMessage{Body: []string{"friday", "monday"}[ask]})
		}},
		{"a reply", func(ask int) (chatlet.Written, error) {
			return r.store.Reply(t.Context(), jane, room, said,
				chatlet.NewMessage{Body: []string{"yes", "no"}[ask]})
		}},
		{"an edit", func(ask int) (chatlet.Written, error) {
			return r.store.Edit(t.Context(), jane, room, said,
				chatlet.EditMessage{Body: []string{"shipping?", "shipping today?"}[ask]})
		}},
		{"a patch", func(ask int) (chatlet.Written, error) {
			return r.store.PatchChannel(t.Context(), jane, room, chatlet.ChannelPatch{
				Topic: &[]string{first, second}[ask]}, chatlet.ChannelAuthority{})
		}},
		{"a reaction", func(ask int) (chatlet.Written, error) {
			return r.store.React(t.Context(), jane, room, said,
				[]string{":eyes:", ":tada:"}[ask])
		}},
		{"a retention", func(ask int) (chatlet.Written, error) {
			return r.store.SetRetention(t.Context(), jane, room,
				[]*int{&keep, &drop}[ask])
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.write(0)
			if err != nil {
				t.Fatalf("the first ask: %v", err)
			}
			r.drain()
			other, err := c.write(1)
			switch {
			case err != nil:
				t.Fatalf("the other ask: %v", err)
			case other.ChangeID == got.ChangeID:
				t.Fatalf("the other ask under the same key derived %s, the "+
					"first one's operation — it would be answered from the "+
					"ledger with nothing of it written", other.ChangeID)
			case other.Outcome.Collapsed:
				t.Fatalf("the other ask was answered as the first's copy: %+v",
					other.Outcome)
			}
			r.drain()
		})
	}
}

// A KEY OUTSIDE THE GRAMMAR IS REFUSED, NAMING THE FIELD, before anything is
// read or derived.
//
// Every id a gesture derives from its key carries the key's instant, and a key
// that is not an operation id the engine minted carries none: read as minted at
// the epoch, before every loss the ledger will ever have, so once it had swept
// anything every such gesture would be answered `unknown` and never published.
func TestAKeyOutsideTheGrammarIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch").Channel.ID
	_, err := r.store.Post(t.Context(), keyed(person("jane"), "submit-7"), room,
		chatlet.NewMessage{Body: "are we shipping?"})
	var refusal *chatlet.InvalidError
	if !errors.As(err, &refusal) || refusal.Field != "actor.op_key" {
		t.Fatalf("a key outside the grammar answered %v, want a refusal of "+
			"actor.op_key", err)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE event = ''`); got != 0 {
		t.Fatalf("a refused post left %d message(s)", got)
	}
}
