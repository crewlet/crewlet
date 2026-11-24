package coordtest

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// fleetRecordCases hold every backend to the bound on each name a small fleet
// record takes from its writer (coord's fleetrecords.go), as stored: a record
// naming each at its bound, every byte one JSON writes as six where a name may
// hold one, is written, and one past is refused as too large before anything
// is written. A twin that stored the longer name would certify a record the
// KV backend's class refuses.
//
// Mutation: drop a backend's CheckLedgerDetail, or a channel's Check, and its
// past case is written.
var fleetRecordCases = []fleetCase{
	{"each name a small record holds is written at its bound and refused past it", func(h *fleetHarness) {
		at := h.now()
		var err error
		over := func(s string) string { return s + "<" }

		detail := escapedText(coord.MaxLedgerDetailBytes)
		if err = h.f.Record(h.ctx, "seat", "key-at", detail, at); err != nil {
			h.t.Errorf("a completion whose detail is at its bound = %v, want it recorded", err)
		}
		h.tooLarge("a completion's detail", h.f.Record(h.ctx, "seat", "key-past", over(detail), at))
		if by, byErr := h.f.WorkedBy(h.ctx, "seat", []string{"key-at", "key-past"}); byErr != nil ||
			by["key-at"] != detail || len(by) != 1 {
			h.t.Errorf("the ledger holds %d of the two completions (%v), want the one at its bound whole",
				len(by), byErr)
		}

		mark := strings.Repeat("m", coord.MaxSeenMarkBytes)
		if ok, markErr := h.f.MarkSeenThrough(h.ctx, "seat", "room", mark, 0); markErr != nil || !ok {
			h.t.Errorf("a seen-through mark at its bound = %v, %v, want it written", ok, markErr)
		}
		_, revision, _ := h.f.SeenThrough(h.ctx, "seat", "room")
		_, err = h.f.MarkSeenThrough(h.ctx, "seat", "room", mark+"m", revision)
		h.tooLarge("a seen-through mark", err)

		reason := escapedText(coord.MaxFollowReasonBytes)
		follow := func(r string) error {
			_, ferr := h.f.Follow(h.ctx, "chatlet", "ceo", "room", "thread", at,
				func(string, bool) string { return r })
			return ferr
		}
		if err = follow(reason); err != nil {
			h.t.Errorf("a follow whose reason is at its bound = %v, want it written", err)
		}
		h.tooLarge("a follow's reason", follow(over(reason)))
		if held, _, _ := h.f.Following(h.ctx, "chatlet", "ceo", "room", "thread"); held != reason {
			h.t.Errorf("the follow holds %q after a refused one, want the one at its bound", held)
		}

		party := escapedText(iam.MaxLogin)
		if err = h.f.OpenChannel(h.ctx, coord.Channel{ID: "ch-at", Requester: party, Target: party,
			OpenedAt: at, LastAt: at}); err != nil {
			h.t.Errorf("a channel naming its parties at their bound = %v, want it opened", err)
		}
		h.tooLarge("a channel's requester", h.f.OpenChannel(h.ctx, coord.Channel{ID: "ch-past",
			Requester: over(party), Target: party, OpenedAt: at, LastAt: at}))
		if _, found, _ := h.f.Channel(h.ctx, "ch-past"); found {
			h.t.Error("a channel whose requester is past its bound was opened")
		}

		seat := uuid.MustParse("01938a6e-7a3e-7cd1-9b2a-0f1e2d3c4b5a")
		_, _, err = h.f.CreateMailbox(h.ctx, coord.MailboxRecord{Seat: seat, Handle: over(party)})
		h.tooLarge("a mailbox record's handle", err)
		if _, created, mailErr := h.f.CreateMailbox(h.ctx, coord.MailboxRecord{Seat: seat, Handle: party,
			AbsentSince: at, RetiringSince: at}); mailErr != nil || !created {
			h.t.Errorf("a mailbox record whose handle is at its bound = %v, %v, want it created", created,
				mailErr)
		}

		name := escapedText(coord.MaxApplyNameBytes)
		apply := coord.NodeApply{NodeID: "node-a", Epoch: 1, RevisionID: name, Serving: name, Boots: name,
			Status: name, Error: escapedText(3 * coord.MaxApplyErrorLength), UpdatedAt: at}
		if err = h.f.RecordApply(h.ctx, apply); err != nil {
			h.t.Errorf("an apply status naming each name at its bound = %v, want it recorded", err)
		}
		apply.NodeID, apply.Serving = "node-b", over(name)
		h.tooLarge("an apply status's serving revision", h.f.RecordApply(h.ctx, apply))

		part := strings.Repeat("s", coord.MaxSessionClaimPartBytes)
		if ok, claimErr := h.f.CreateSessionClaim(h.ctx, coord.SessionClaim{Session: part, Turn: part,
			Launch: part}); claimErr != nil || !ok {
			h.t.Errorf("a session claim naming each part at its bound = %v, %v, want it written", ok,
				claimErr)
		}
		_, err = h.f.CreateSessionClaim(h.ctx, coord.SessionClaim{Session: "session-past", Turn: "turn",
			Launch: part + "s"})
		h.tooLarge("a session claim's job", err)

		node := strings.Repeat("n", coord.MaxNodeIDBytes)
		if keeper, custodyErr := h.f.ClaimCustody(h.ctx, "batch-at", node); custodyErr != nil || keeper != node {
			h.t.Errorf("a custody claim naming a node id at its bound = %q, %v, want it kept", keeper, custodyErr)
		}
		_, err = h.f.ClaimCustody(h.ctx, "batch-past", node+"n")
		h.tooLarge("a custody claim's node", err)

		forged := coord.WindowsAt(at, time.UTC)
		forged[0].Label = strings.Repeat("9", 4096)
		if _, err := h.f.Charge(h.ctx, coord.ChargeRequest{Seat: "agent:ceo", Tokens: 1,
			Windows: forged}); err == nil {
			h.t.Error("a charge naming a window by a label period.At did not cut was counted")
		}
	}},
}

// escapedText is text JSON writes in exactly n bytes, every byte it can as
// six: the widest a field of n bytes as stored holds.
func escapedText(n int) string {
	return strings.Repeat("<", n/6) + strings.Repeat("a", n%6)
}
