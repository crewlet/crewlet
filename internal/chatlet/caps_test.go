package chatlet_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
)

// TestTheRailIsBoundedByTheCursorsAPersonCanKeep holds a pair of constants
// this package documents as one number.
//
// [chatlet.MaxRailChannels] is how many rooms a person's channel list answers
// for, and it is [coord.MaxReadCursors] — the cap the read-state record puts
// on one person's cursors. Past that cap the record has already dropped its
// stalest cursor to stay inside its own bound, so a room listed beyond it
// could not carry an unread badge that was ever right: the rail would show a
// number derived from a cursor nothing is keeping.
//
// A RAIL SMALLER than the cursor cap is merely wasteful. A rail LARGER is the
// defect, and it is silent: the extra rooms render, their badges read zero,
// and zero is indistinguishable from caught up.
func TestTheRailIsBoundedByTheCursorsAPersonCanKeep(t *testing.T) {
	t.Parallel()
	if chatlet.MaxRailChannels != coord.MaxReadCursors {
		t.Fatalf("the channel rail answers for %d rooms while a person keeps "+
			"%d read cursors. Above the cursor cap a room's badge is derived "+
			"from a cursor the record has already evicted, and it renders as "+
			"caught up rather than as unknown. Move both, or write down here "+
			"why the rail may list a room whose unread count cannot be kept",
			chatlet.MaxRailChannels, coord.MaxReadCursors)
	}
}

// ARCHIVING A ROOM LETS A COMPANY AT THE CAP MAKE ANOTHER, which is what the
// refusal has always told people to do.
//
// It counted every row in the table and there is no channel delete in this
// vocabulary, so a company that had ever created [chatlet.MaxChannels] rooms
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
	r.fillArchived(chatlet.MaxChannels)
	if got := r.count(`SELECT COUNT(*) FROM chat_channels`); got != chatlet.MaxChannels {
		t.Fatalf("the fixture wrote %d rooms, want %d", got, chatlet.MaxChannels)
	}

	if _, err := r.store.CreateChannel(r.t.Context(), chatlet.Actor{
		Handle: "jane", Kind: chatlet.AuthorHuman,
	}, chatlet.NewChannel{Name: "one-more", Kind: chatlet.KindPublic}); err != nil {
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
	err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
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
