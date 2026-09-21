package chat_test

import (
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
