package code

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE REF LIFECYCLE'S RULE KEEPS EVERY BRANCH THAT IS NO LONGER IDLE, each
// with its own sentence, in the order it asks — an earlier push of the batch,
// a hold recorded past the page, the default branch, a live change, a writer
// that is not the seat, the window — and lets an idle branch go. A hold
// recorded AT the page's position was taken before it, and the pass's own read
// of the holds answered it: it keeps nothing here.
//
// Mutations: drop the held term → the held row is deleted, red; compare it
// with >= → the row held at the page is kept, red; drop the default term → the
// default is deleted, red; judge the window by the deciding node's clock
// rather than the proposal's — the case's `now` is the pass's — and the window
// rows flip, red.
func TestExpiryRuleKeepsWhatIsNoLongerIdle(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	const through = 1000
	idle := ExpiryRow{Target: strings.Repeat("a", 40), Seat: "dev", At: now.Add(-RefExpiry - time.Hour)}
	with := func(f func(*ExpiryRow)) ExpiryRow {
		r := idle
		f(&r)
		return r
	}
	for _, c := range []struct {
		name    string
		row     ExpiryRow
		touched bool
		want    string
	}{
		{"idle", idle, false, ""},
		{"set by an earlier push of the batch", idle, true, expiryKeptTouched},
		{"held after the page", with(func(r *ExpiryRow) { r.Held = through + 1 }), false, expiryKeptHeld},
		{"held at the page", with(func(r *ExpiryRow) { r.Held = through }), false, ""},
		{"held before the page", with(func(r *ExpiryRow) { r.Held = through - 1 }), false, ""},
		{"the default branch", with(func(r *ExpiryRow) { r.Default = true }), false, expiryKeptDefault},
		{"a live change's", with(func(r *ExpiryRow) { r.Change = true }), false, expiryKeptChange},
		{"set last by somebody else", with(func(r *ExpiryRow) { r.Seat = "" }), false, expiryKeptSeat},
		{"set within the window", with(func(r *ExpiryRow) { r.At = now.Add(-RefExpiry + time.Hour) }), false,
			expiryKeptRecent},
		{"set at the window's edge", with(func(r *ExpiryRow) { r.At = now.Add(-RefExpiry) }), false, ""},
		// THE FIRST REASON WINS, so a sentence says the one that matters
		// most: a branch somebody else set and a turn holds is held.
		{"held and set by somebody else", with(func(r *ExpiryRow) { r.Held, r.Seat = through+1, "" }), false,
			expiryKeptHeld},
	} {
		refusal, why := ExpiryRule(c.row, c.touched, now, through)
		switch {
		case c.want == "" && (refusal != "" || why != ""):
			t.Errorf("%s: kept (%s, %q), want it deleted", c.name, refusal, why)
		case c.want != "" && (refusal != RefusalStale || why != c.want):
			t.Errorf("%s: answered (%s, %q), want stale %q", c.name, refusal, why, c.want)
		}
	}
}

// EVERY SENTENCE THE RULE KEEPS A BRANCH WITH FITS A COMMAND'S DETAIL — the
// receiver-details test's bound, which a committer's answer is held to as
// well.
//
// Mutation: lengthen one past maxDetailBytes → red.
func TestEveryExpirySentenceFitsADetail(t *testing.T) {
	t.Parallel()
	for _, s := range []string{expiryKeptTouched, expiryKeptHeld, expiryKeptDefault, expiryKeptChange,
		expiryKeptSeat, expiryKeptRecent} {
		if len(s) > maxDetailBytes || s == "" {
			t.Errorf("%q is %d bytes, want one sentence within %d", s, len(s), maxDetailBytes)
		}
	}
}

// AN EXPIRY IS CUT TO WHAT ITS ENTRY HOLDS, as well as to MaxCommandsPerPush:
// a page of plain refs is one push, and a page of the longest refs JSON
// spells six bytes a character is cut where the next deletion would not fit —
// each push passing Proposal.Validate, the next deletion past it refused
// TooWideError — so no expiry is refused every pass for its width.
//
// Mutation: cut by MaxCommandsPerPush alone → the wide page is one push,
// refused, red.
func TestAnExpiryIsCutToWhatItsEntryHolds(t *testing.T) {
	t.Parallel()
	seen := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 9}
	base := Proposal{PushID: uuid.Must(uuid.NewV7()).String(), RepoID: uuid.NewString(),
		Principal: Principal{Kind: PrincipalSystem}, Via: ViaExpiry, Node: strings.Repeat("n", 64),
		ReceivedAt: time.Now(), Seen: seen}
	page := func(width int) []Command {
		cmds := make([]Command, MaxCommandsPerPush)
		for i := range cmds {
			ns := SeatNamespace("dev") + fmt.Sprintf("%06d", i)
			name := ns + strings.Repeat("<", width-len(ns))
			cmds[i] = Command{Ref: name, Old: strings.Repeat("a", 40), New: ZeroOID}
		}
		return cmds
	}
	plain := page(40)
	if n, err := expiryCut(base, plain); err != nil || n != len(plain) {
		t.Fatalf("a page of plain refs is cut at %d (%v), want one push of all %d", n, err, len(plain))
	}
	wide := page(MaxRefNameBytes)
	n, err := expiryCut(base, wide)
	if err != nil || n < 1 || n >= len(wide) {
		t.Fatalf("a page of the longest refs is cut at %d (%v), want fewer than %d", n, err, len(wide))
	}
	p := base
	p.Commands = wide[:n]
	if err := p.Validate(); err != nil {
		t.Errorf("the cut push of %d deletions: %v, want it valid", n, err)
	}
	p.Commands = wide[:n+1]
	var tooWide *TooWideError
	if err := p.Validate(); !errors.As(err, &tooWide) {
		t.Errorf("one deletion more: %v, want it too wide", err)
	}
	t.Logf("a page of %d-byte refs is cut at %d deletions a push", MaxRefNameBytes, n)
}
