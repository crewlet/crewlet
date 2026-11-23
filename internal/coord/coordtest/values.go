package coordtest

import (
	"errors"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- what one key holds ------------------------------------------------- //

// valueCases hold every backend to ONE bound on a value, [coord.MaxValueBytes],
// and to one answer past it, [coord.ErrValueTooLarge]: refused before
// anything is written, and never an outage. The KV backend refuses a value its
// leader's read could not carry back across a leaf's link; a twin that stored
// it would certify a write the store refuses, and a backend that called the
// refusal unavailable would have its caller retry a write no retry lands.
var valueCases = []fleetCase{
	{"a value past what a key holds is refused as too large, never as an outage, and nothing is written", func(h *fleetHarness) {
		past := make([]byte, coord.MaxValueBytes+1)
		created, err := h.f.CreateSandboxRun(h.ctx, "turn-too-large", past, nil)
		h.tooLarge("a run's create", err)
		if created {
			h.t.Error("a run whose value is past what a key holds reads as created")
		}
		if _, found := h.run("turn-too-large"); found {
			h.t.Error("a run whose value is past what a key holds was stored")
		}

		h.createRun("turn-1", `{"status":"running"}`)
		_, changed, err := h.f.MutateSandboxRun(h.ctx, "turn-1",
			func(coord.Record) (coord.SandboxRunChange, error) {
				return coord.SandboxRunChange{Put: past}, nil
			})
		h.tooLarge("a run's update", err)
		if record, _ := h.run("turn-1"); changed || string(record.Value) != `{"status":"running"}` {
			h.t.Errorf("a run's update past what a key holds changed it (%t) to %d bytes, want it "+
				"as it was", changed, len(record.Value))
		}
		_, changed, err = h.f.MutateSandboxRunPart(h.ctx, "turn-1", coord.RunLog,
			func(coord.Record, bool) ([]byte, error) { return past, nil })
		h.tooLarge("a run's part", err)
		if changed {
			h.t.Error("a run's part past what a key holds reads as written")
		}
	}},
	{"a value at what a key holds is stored whole", func(h *fleetHarness) {
		at := []byte(`"` + strings.Repeat("x", coord.MaxValueBytes-2) + `"`)
		if !h.createRun("turn-at-the-bound", string(at)) {
			h.t.Fatal("a run whose value is what a key holds was not created")
		}
		if record, found := h.run("turn-at-the-bound"); !found || len(record.Value) != len(at) {
			h.t.Errorf("a run whose value is what a key holds reads back as %d bytes (found %t), "+
				"want its %d", len(record.Value), found, len(at))
		}
	}},
	{"an activation whose pointer is past what a key holds is refused as too large, and the pointer stays", func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("activate the first revision: %v", err)
		}
		body := []byte(`{"v":"` + strings.Repeat("x", coord.MaxValueBytes) + `"}`)
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: body, At: h.now(), Expect: "rev-1", Origin: anOrigin})
		h.tooLarge("an activation", err)
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-1" {
			h.t.Errorf("the pointer after an activation past what a key holds = %+v (found %t, %v), "+
				"want rev-1 as it was", target, found, err)
		}
	}},
	{"an activation whose pointer is past its bound is refused as too large, and the pointer stays", func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("activate the first revision: %v", err)
		}
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Summary: strings.Repeat("s", coord.MaxPointerBytes), Payload: []byte(`{"v":2}`),
			At: h.now(), Expect: "rev-1", Origin: anOrigin})
		h.tooLarge("an activation with a pointer past its bound", err)
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-1" {
			h.t.Errorf("the pointer after an activation past its bound = %+v (found %t, %v), "+
				"want rev-1 as it was", target, found, err)
		}
	}},
	// THE BODY IS HELD TO WHAT ONE KEY HOLDS, not to the pointer's bound:
	// the document rides its own key, so a company at the store's bound is
	// activated and read back whole.
	{"an activation whose body is at what a key holds is stored and read back whole", func(h *fleetHarness) {
		body := []byte(`{"v":"` + strings.Repeat("x", coord.MaxValueBytes-len(`{"v":""}`)) + `"}`)
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: body, At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("an activation whose body is what a key holds was refused: %v", err)
		}
		got, found, err := h.f.Body(h.ctx, published)
		if err != nil || !found || len(got) != len(body) {
			h.t.Errorf("its body reads back as %d bytes (found %t, %v), want its %d", len(got), found, err, len(body))
		}
	}},
}

// positionsValueCases hold every backend to the bound the positions
// register's class declares for each opaque record: an access review, a
// seat's access record and an atlas wake one byte past theirs are refused as
// too large, and at it are written. A twin left at coord.MaxValueBytes would
// certify a write the KV backend refuses.
var positionsValueCases = []fleetCase{
	{"a positions record past its class's bound is refused as too large, and at it is written", func(h *fleetHarness) {
		for _, c := range []struct {
			what  string
			bound int
			write func(value []byte) error
		}{
			{"an access review", coord.MaxAccessReviewBytes, func(value []byte) error {
				_, err := h.f.CreateAccessReview(h.ctx, "review-"+itoa(len(value)), value)
				return err
			}},
			{"a seat's access record", coord.MaxSeatAccessBytes, func(value []byte) error {
				return h.f.PutSeatAccessRecord(h.ctx, "gitlab", "seat-"+itoa(len(value)), value)
			}},
			{"an atlas wake", coord.SmallValueBytes, func(value []byte) error {
				_, err := h.f.CreateAtlasWake(h.ctx, "sre", "entry-"+itoa(len(value)), value)
				return err
			}},
		} {
			h.tooLarge(c.what, c.write(paddedValue(c.bound+1)))
			if err := c.write(paddedValue(c.bound)); err != nil {
				h.t.Errorf("%s at its bound, %d bytes = %v, want it written", c.what, c.bound, err)
			}
		}
	}},
}

// uploadsValueCases hold every backend to the bound each class of the uploads
// bucket declares: the reservation ledger, an upload's record and a seat's
// save ledger one byte past theirs are refused as too large, created or
// changed, and at it are written. A twin left at coord.MaxValueBytes would
// certify a record the KV backend refuses.
var uploadsValueCases = []fleetCase{
	{"an uploads value past its class's bound is refused as too large, and at it is written", func(h *fleetHarness) {
		for _, c := range []struct {
			what  string
			bound int
			swap  func(value []byte, revision uint64) (uint64, error)
		}{
			{"the reservation ledger", coord.MaxUploadLedgerBytes, func(value []byte, revision uint64) (uint64, error) {
				return h.f.SwapUploadLedger(h.ctx, value, revision)
			}},
			{"an upload's record", coord.MaxUploadRecordBytes, func(value []byte, revision uint64) (uint64, error) {
				return h.f.SwapUploadRecord(h.ctx, "u-bound", value, revision)
			}},
			{"a seat's save ledger", coord.MaxSaveLedgerBytes, func(value []byte, revision uint64) (uint64, error) {
				return h.f.SwapSaveLedger(h.ctx, "sre", value, revision)
			}},
		} {
			_, err := c.swap(paddedValue(c.bound+1), 0)
			h.tooLarge(c.what+" created", err)
			wrote, err := c.swap(paddedValue(c.bound), 0)
			if err != nil || wrote == 0 {
				h.t.Errorf("%s at its bound, %d bytes = %d, %v, want it written", c.what, c.bound, wrote, err)
				continue
			}
			_, err = c.swap(paddedValue(c.bound+1), wrote)
			h.tooLarge(c.what+" changed", err)
		}
	}},
}

// chatReadsValueCases hold every backend to the record the chat reads
// bucket's class bounds: the widest a writer can make, every set at its cap
// and every id a UUID, is written and read back whole, and a flush naming an
// id Chatlet did not mint, of any length, is refused as the caller's before
// anything is written. A record past coord.MaxChatReadBytes is one no flush
// can make, so its refusal is held where both backends encode it
// (coord.TestAChatReadRecordPastItsBoundIsRefusedWhereItIsEncoded) and in
// the twin (memory.TestTheTwinRefusesAChatReadRecordPastItsBound).
//
// Mutation: declare the chat read class at coord.SmallValueBytes, or have the
// twin hold its record to it, and that backend refuses the widest; drop
// checkDeltaIDs and the flush naming "eng" is written.
var chatReadsValueCases = []fleetCase{
	{"the widest chat read record is written and read back whole", func(h *fleetHarness) {
		at := time.Date(2026, 3, 14, 15, 9, 26, 123456789, time.UTC)
		written, err := h.f.AdvanceChatRead(h.ctx, "founder", WidestChatReadDelta(at))
		if err != nil {
			h.t.Fatalf("flush the widest record: %v", err)
		}
		if exists, stampErr := h.f.SetChatReadAbsence(h.ctx, "founder", at); stampErr != nil || !exists {
			h.t.Fatalf("stamp the widest record absent = %v, %v", exists, stampErr)
		}
		written, _ = written.WithAbsence(at)
		read, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("read the widest record back: %v", err)
		}
		want, err := coord.EncodeChatRead("founder", written)
		if err != nil {
			h.t.Fatal(err)
		}
		got, err := coord.EncodeChatRead("founder", read)
		if err != nil || string(got) != string(want) {
			h.t.Errorf("the widest record read back as %d bytes (%v), want the %d written", len(got), err,
				len(want))
		}
	}},
	{"a flush naming an id Chatlet did not mint is refused as the caller's, and nothing is written", func(h *fleetHarness) {
		for _, delta := range []coord.ChatReadDelta{
			{Cursors: map[string]int64{"eng": 1}},
			{Mute: []string{strings.Repeat("r", coord.MaxChatReadBytes)}},
		} {
			delta.At = h.now()
			_, err := h.f.AdvanceChatRead(h.ctx, "founder", delta)
			if !errors.Is(err, coord.ErrInvalidReadState) || errors.Is(err, coord.ErrValueTooLarge) ||
				errors.Is(err, coord.ErrUnavailable) {
				h.t.Errorf("a flush naming an id Chatlet did not mint = %v, want coord.ErrInvalidReadState "+
					"alone", err)
			}
		}
		if state, err := h.f.ChatRead(h.ctx, "founder"); err != nil || state.Revision != 0 {
			h.t.Errorf("after the refused flushes the record reads %+v, %v; want none", state, err)
		}
	}},
}

// paddedValue is a JSON string of exactly size bytes.
func paddedValue(size int) []byte {
	return []byte(`"` + strings.Repeat("x", size-2) + `"`)
}

// tooLarge fails the case unless err is a refusal of a value past what a key
// holds, and nothing that reads as an outage or as a write that may have
// landed.
func (h *fleetHarness) tooLarge(what string, err error) {
	h.t.Helper()
	switch {
	case !errors.Is(err, coord.ErrValueTooLarge):
		h.t.Errorf("%s past what a key holds = %v, want coord.ErrValueTooLarge", what, err)
	case errors.Is(err, coord.ErrUnavailable), errors.Is(err, coord.ErrWriteUnknown):
		h.t.Errorf("%s past what a key holds = %v: a refusal made before anything was sent read "+
			"as an outage", what, err)
	}
}
