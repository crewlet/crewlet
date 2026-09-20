package chat_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
)

// THE COMPANY'S THREAD SETTING REACHES A WOKEN SEAT, which is the property
// `chat.native.thread_context_messages` is a setting AT ALL for.
//
// It was config nothing read: the field validated, had a default, a ceiling
// and a documented accessor, and no line in the tree called it. An operator
// could set it, `crewlet validate` would accept it, the docs described what it
// did, and a seat's prompt carried none of the thread either way. That is the
// worst shape a knob can be in — it is indistinguishable, from outside, from a
// setting that works.
func TestAThreadRidesIntoTheSnapshotAtTheCompanysOwnCount(t *testing.T) {
	t.Parallel()
	lines := make([]chat.ThreadLine, 0, 12)
	for i := range 12 {
		lines = append(lines, chat.ThreadLine{
			Author: "dana", AuthorKind: chat.AuthorAgent,
			Excerpt: "line " + string(rune('a'+i)),
		})
	}

	// THE NEWEST ARE KEPT, not the first. A thread's opening matters less
	// to the message being answered than the exchange just before it, and
	// a cap applied from the wrong end would hand the model the start of a
	// conversation that has moved on.
	got := chat.ThreadContextOf(lines, 4)
	if len(got) != 4 {
		t.Fatalf("a company asking for 4 lines got %d", len(got))
	}
	if got[0].Excerpt != "line i" || got[3].Excerpt != "line l" {
		t.Fatalf("kept %q..%q, want the last four (line i..line l) — a cap "+
			"taken from the front keeps the opening of a thread and drops the "+
			"exchange the newest message is answering",
			got[0].Excerpt, got[3].Excerpt)
	}

	// AND IT IS STILL OLDEST FIRST, because that is reading order and a
	// prompt renders it verbatim.
	for i := 1; i < len(got); i++ {
		if got[i-1].Excerpt >= got[i].Excerpt {
			t.Fatalf("line %d (%q) does not follow line %d (%q): the block is "+
				"rendered into a prompt as it stands, so a reversed thread is "+
				"a conversation the model reads backwards",
				i, got[i].Excerpt, i-1, got[i-1].Excerpt)
		}
	}
}

// THE BYTE BUDGET BINDS BEFORE THE COUNT DOES, which is the half a line count
// cannot do on its own.
//
// Fifty lines at the excerpt cap would be 30 KiB on top of a snapshot already
// sized at 54, against a 64 KiB record. So the budget is what actually decides
// the size, and a thread of long messages comes back SHORTER than the company
// asked for rather than overflowing the record.
func TestALongThreadIsCutByBytesRatherThanOverflowingTheRecord(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", chat.MaxExcerpt)
	lines := make([]chat.ThreadLine, 0, chat.MaxThreadContext)
	for range chat.MaxThreadContext {
		lines = append(lines, chat.ThreadLine{
			Author: "dana", AuthorKind: chat.AuthorAgent, Excerpt: long,
		})
	}

	got := chat.ThreadContextOf(lines, chat.MaxThreadContext)
	if len(got) == 0 {
		t.Fatal("a thread of long messages carried nothing at all: the budget " +
			"is meant to shorten the block, not to empty it")
	}
	if len(got) >= chat.MaxThreadContext {
		t.Fatalf("all %d lines survived at %d bytes each against a %d-byte "+
			"budget — the count is bounding this and the bytes are not",
			len(got), chat.MaxExcerpt, chat.MaxThreadContextBytes)
	}
	size := 0
	for _, line := range got {
		size += len(line.Excerpt) + len(line.Author)
	}
	if size > chat.MaxThreadContextBytes {
		t.Fatalf("the thread is %d bytes against a %d cap; a record built from "+
			"it is past what MaxRecordBytes was sized for and an external "+
			"broker's max_payload is the next thing to refuse it",
			size, chat.MaxThreadContextBytes)
	}

	// AND THE SNAPSHOT AGREES, so a producer that built the block some
	// other way is refused rather than published.
	if err := (&chat.Notify{
		MessageID: "m", ChannelID: "c", ChannelKind: chat.KindPublic,
		AuthorKind: chat.AuthorAgent, ThreadContext: lines,
	}).Validate(); err == nil {
		t.Fatal("a snapshot carrying every line at full length validated: the " +
			"cap is enforced only where the block is built, so any other " +
			"producer reaches the broker with it")
	}
}

// ARCHIVING A ROOM LETS A COMPANY AT THE CAP MAKE ANOTHER, which is what the
// refusal has always told people to do.
//
// It counted every row in the table and there is no channel delete in this
// vocabulary, so a company that had ever created [chat.MaxChannels] rooms
// could never create another for the life of the deployment — while the
// refusal advised archiving, which changed the count by nothing. A cap nobody
// can get under is not a cap; it is a permanent stop with a remedy that reads
// like an oversight.
func TestArchivingARoomReleasesTheChannelCap(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))

	// Filled to the cap with rooms that are already ARCHIVED, which is
	// the state the old count could not tell from a live one. Written as
	// rows rather than through the store, because a thousand records
	// through a real broker is a minute of wall clock to establish a
	// precondition that is not the property under test.
	r.fillArchived(chat.MaxChannels)
	if got := r.count(`SELECT COUNT(*) FROM chat_channels`); got != chat.MaxChannels {
		t.Fatalf("the fixture wrote %d rooms, want %d", got, chat.MaxChannels)
	}

	if _, err := r.store.CreateChannel(r.t.Context(), chat.Actor{
		Handle: "jane", Kind: chat.AuthorHuman,
	}, chat.NewChannel{Name: "one-more", Kind: chat.KindPublic}); err != nil {
		t.Fatalf("a company whose every room is archived could not create "+
			"another: %v\n\tThe cap is counting rows rather than live rooms, "+
			"so the archive its own refusal recommends changes nothing and "+
			"the company is stopped for the life of the deployment", err)
	}
}

// fillArchived writes n archived rooms straight into the replicated estate,
// in one statement per batch.
func (r *writeRound) fillArchived(n int) {
	r.t.Helper()
	err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		for i := range n {
			id := fmt.Sprintf("arch-%04d", i)
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO chat_channels
					(id, name, name_norm, kind, private, topic, purpose, unit,
					 retention_days, message_seq, created_at, created_by,
					 created_by_kind, archived_at, version, scoped_through, document)
				VALUES (?, ?, ?, 'public', 0, '', '', '', NULL, 0, 1, 'jane',
				        'human', 1, ?, 0, X'')`,
				id, id, id, int64(i+1)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.t.Fatalf("fill the company with archived rooms: %v", err)
	}
}
