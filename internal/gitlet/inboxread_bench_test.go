package gitlet_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/inbox"
)

// benchInboxChanges is the live changes the benchmark's inbox page names: one
// whole page of notices ([inbox.MaxRows]), each about a change of its own, so
// every notice's card is judged.
const benchInboxChanges = inbox.MaxRows

// BenchmarkACodeInbox times the two reads a person's code inbox costs
// (inboxread.go's head) over one repository whose main asks one counted
// approval, holding benchInboxChanges live changes dev opened, each
// mentioning founder — so founder's inbox holds a page of notices, each about
// a live change whose merge gate its card judges:
//
//   - bare: one `inbox` read of founder's page, as every count of what waits
//     on them, Home's Updates and the Snoozed link's count read it;
//   - cards: that read and the `notice_cards` read of the page's changes, as
//     the Inbox screen's list reads them — the readiness judged, the
//     descriptions and the open findings read, for every change.
//
// The figure inboxread.go's head names is each's time an iteration.
func BenchmarkACodeInbox(b *testing.B) {
	n := newNode(b)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(b, n, n.pub)
	repo := changeRepo(b, n, "web")
	branches := make([]gitlet.Command, benchInboxChanges)
	for i := range branches {
		branches[i] = set(fmt.Sprintf("refs/heads/c%02d", i), gitlet.ZeroOID, oidOf(fmt.Sprintf("c%02d", i)))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), branches...))
	for i := range benchInboxChanges {
		open := opening(repo, fmt.Sprintf("refs/heads/c%02d", i), mainRef, dev)
		open.Open.Mentions = []string{"founder"}
		open.Open.Body = "Retry the upload once, not twice."
		res, err := c.ProposeChange(context.Background(), open)
		requireChanged(b, res, err)
	}
	ctx := context.Background()
	page := func() gitlet.ReadAnswer {
		answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadInbox, Asker: ada, Fresh: stale,
			Limit: inbox.MaxRows, Inbox: &gitlet.InboxQuery{Handle: "founder", Snoozed: inbox.SnoozeExclude,
				Now: harnessAt}})
		if err != nil || answer.Inbox == nil {
			b.Fatalf("read founder's code inbox: %v", err)
		}
		return answer
	}
	cards := func(answer gitlet.ReadAnswer) gitlet.ReadAnswer {
		ids := make([]string, 0, len(answer.Inbox.Notices))
		for _, notice := range answer.Inbox.Notices {
			ids = append(ids, notice.ChangeID)
		}
		carded, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadNoticeCards, Asker: ada, Fresh: stale,
			NoticeCards: ids})
		if err != nil {
			b.Fatalf("read the cards of founder's page: %v", err)
		}
		return carded
	}
	// THE SCALE IS WHAT IT SAYS: a page of benchInboxChanges notices, every
	// card's readiness judged.
	first := page()
	if got := len(first.Inbox.Notices); got != benchInboxChanges {
		b.Fatalf("founder's code inbox holds %d notices, want %d", got, benchInboxChanges)
	}
	for id, card := range cards(first).NoticeCards {
		if card.Readiness == nil || card.Body == "" {
			b.Fatalf("the card of %s carries %+v, want its readiness and its description", id, card)
		}
	}
	b.Run("bare", func(b *testing.B) {
		for b.Loop() {
			page()
		}
	})
	b.Run("cards", func(b *testing.B) {
		for b.Loop() {
			cards(page())
		}
	})
}
