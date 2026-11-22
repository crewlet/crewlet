package chatlet_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A reader's paged views end at the bytes the read's context holds
// ([statelog.RowsOf]), and their cursor carries on from the last row kept: a
// node holding no data reads each page as one reply, refused whole past the
// transport's message.

// longLinked builds one message of a whole [chatlet.MaxBody] that carries a
// link.
func longLinked(channelID, messageID, link string) chatlet.MutationRecord {
	return readRecord(chatlet.MessageSubject(channelID), chatlet.OpPost,
		"op-post-"+messageID, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: messageID,
			Body:   strings.Repeat("x", chatlet.MaxBody),
			Author: "ada", AuthorKind: chatlet.AuthorHuman, Links: []string{link},
		}, chatlet.ScopeSet{Subject: true}, "ada")
}

// A ROOM'S SHARED LINKS PAGE AT THE BYTES, AND PAGING ON READS EVERY LINK ONCE.
//
// Five messages of a whole body each, under a bound two of them fit: every
// page is cut short of its limit, and its cursor names the last message it
// holds, so the next page starts at the one refused.
//
// Mutations, each red: drop the byte bound from Links and the first page holds
// all five; set the cursor from the refused message rather than the last kept
// and every refused message's link is lost.
func TestARoomsSharedLinksPageAtTheBytesAndPageOn(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "essays", chatlet.KindPublic, "ada", "jane"))
	var want []string
	for i := 1; i <= 5; i++ {
		link := fmt.Sprintf("https://example.com/%d", i)
		h.apply(longLinked("room-1", fmt.Sprintf("msg-%d", i), link))
		want = append([]string{link}, want...)
	}
	ctx := statelog.WithAnswerBytes(t.Context(), 5*chatlet.MaxBody/2)
	reader := h.reader()

	var got []string
	cursor, pages := "", 0
	for {
		page, err := reader.Links(ctx, "jane", chatlet.LinksQuery{
			ChannelID: "room-1", Cursor: cursor, Limit: chatlet.MaxLimit,
		}, h.session())
		if err != nil {
			t.Fatalf("links page %d: %v", pages, err)
		}
		pages++
		if pages == 1 && len(page.Links) != 2 {
			t.Errorf("the first page holds %d links, want the 2 whose messages "+
				"fit the bound", len(page.Links))
		}
		for _, l := range page.Links {
			got = append(got, l.URL)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
		if pages > len(want) {
			t.Fatal("the cursor never ran out")
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("paging the room's links read %v, want %v", got, want)
	}
	if pages != 3 {
		t.Errorf("five long messages came back in %d pages, want 3 of at most two", pages)
	}
}

// A PAGE OF SAVED MESSAGES IS MEASURED AS THE WIRE ENCODES IT.
//
// A body of `<` is a sixth of what JSON writes for it, so three such saves
// measured by their bodies fit a bound one of them fills when encoded: each
// page holds one, and paging on reads every save once, newest first.
//
// Mutations, each red: measure a save by its body's length and the first page
// holds all three; drop the byte bound from Saved and it does too; set the
// cursor from the refused save and the next page skips it.
func TestAPageOfSavedMessagesIsMeasuredAsTheWireEncodesIt(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-1", "essays", chatlet.KindPublic, "ada", "jane"))
	dense := strings.Repeat("<", chatlet.MaxBody)
	at := time.Date(2031, 5, 6, 9, 0, 0, 0, time.UTC)
	var marks []chatlet.SavedMark
	var want []string
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("msg-%d", i)
		h.apply(readPost("room-1", id, "ada", dense))
		marks = append(marks, chatlet.SavedMark{ChannelID: "room-1", MessageID: id,
			At: at.Add(-time.Duration(i) * time.Minute)})
		want = append(want, id)
	}
	ctx := statelog.WithAnswerBytes(t.Context(), 8*chatlet.MaxBody)
	reader := h.reader()

	var got []string
	cursor, pages := "", 0
	for {
		page, err := reader.Saved(ctx, "jane", chatlet.SavedQuery{
			Saved: marks, Cursor: cursor, Limit: chatlet.MaxLimit,
		}, h.session())
		if err != nil {
			t.Fatalf("saved page %d: %v", pages, err)
		}
		pages++
		if len(page.Saved) != 1 {
			t.Errorf("page %d holds %d saves, want the one its encoded bytes fit",
				pages, len(page.Saved))
		}
		for _, entry := range page.Saved {
			if entry.Gone {
				t.Errorf("save %s is gone, want its message", entry.Mark.MessageID)
			}
			got = append(got, entry.Mark.MessageID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
		if pages > len(marks) {
			t.Fatal("the cursor never ran out")
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("paging the saves read %v, want %v", got, want)
	}
}
