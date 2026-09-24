package tracker_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RETRY CARRIES ITS FIRST ATTEMPT'S MINT INSTANT ACROSS AN ADOPTION.
//
// A node that adopted a snapshot holds a scrubbed ledger, so an operation whose
// first record landed before the adoption has no ledger row here. When a retry
// of it goes unanswered and a rival's record sits above the anchor it decided
// against, the ledger's silence reads as "somebody else won" only for an
// operation minted after the adoption; one minted before is answered
// `unknown`, because its own record may be among the rows the adoption
// brought. The retry's own call is after the adoption, so what it is stamped
// with has to be the instant the surface carried.
//
// Mutation: stamp the request with the call's own instant and the retry is not
// answered `unknown` — it is decided again, and its second append meets its
// first record under the same op id.
func TestARetryCarriesItsFirstAttemptsMintInstantAcrossAnAdoption(t *testing.T) {
	t.Parallel()
	lose := &losingAppender{}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		lose.Appender = a
		return lose
	})
	view := aView("v-1", nil)

	// THE FIRST ATTEMPT, before the adoption: it lands and is applied.
	first := time.Now().Add(-time.Hour).UTC()
	r.at = first
	if _, err := r.writer.WriteView(t.Context(), "op-view", view); err != nil {
		t.Fatalf("the first attempt: %v", err)
	}
	r.drain()

	// THE ADOPTION, which brings the rows and not the ledger.
	adopt(t, r)

	// A RIVAL'S RECORD above the anchor the retry decides against, which
	// this node has not applied yet.
	if _, err := r.writer.WriteView(t.Context(), "op-rival", aView("v-1",
		func(v *tracker.View) { v.Name = "Somebody else's" })); err != nil {
		t.Fatalf("the rival's write: %v", err)
	}
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	// THE RETRY, after the adoption, whose own append goes unanswered.
	r.at = time.Now().Add(time.Hour).UTC()
	r.applyWhileWriting()
	lose.next("op-view")
	res, err := r.writer.As("ana", tracker.AuthorHuman, tracker.Provenance{
		MintedAt: first,
	}).WriteView(t.Context(), "op-view", view)
	if err != nil {
		t.Fatalf("the retry: %v — an operation minted before this node adopted "+
			"is one its ledger cannot answer for", err)
	}
	if res.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry's outcome = %q, want unknown: its first record may "+
			"be among the rows the adoption brought, and deciding it again "+
			"writes it twice", res.Outcome)
	}
	if got, err := r.log.End(t.Context()); err != nil || got != end {
		t.Errorf("the log ends at %d (%v) after the retry, want %d — nothing "+
			"of the retry's may land", got, err, end)
	}
}

// A RETRY THE BROKER ACKNOWLEDGES AS ITS FIRST RECORD, ACROSS AN ADOPTION, IS
// APPLIED.
//
// Inside the stream's duplicate window a retry's append is answered with the
// first record's own position, which names it as this operation's. When this
// node adopted a snapshot after the operation was minted, that record is in
// the rows the adoption brought and its ledger row is what the scrub took — so
// the write was applied, and a refusal calling it a record applied without a
// ledger row is a refusal of the operation's own success.
//
// Mutation: answer a known record with no ledger row as a contract violation
// whatever the adoption, and the retry is refused; stamp it with the call's
// own instant and it is refused the same way.
func TestARetryAcknowledgedAsItsFirstRecordAcrossAnAdoptionIsApplied(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	view := aView("v-1", nil)

	first := time.Now().Add(-time.Hour).UTC()
	r.at = first
	if _, err := r.writer.WriteView(t.Context(), "op-view", view); err != nil {
		t.Fatalf("the first attempt: %v", err)
	}
	r.drain()
	landed, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	adopt(t, r)

	r.at = time.Now().Add(time.Hour).UTC()
	r.applyWhileWriting()
	res, err := r.writer.As("ana", tracker.AuthorHuman, tracker.Provenance{
		MintedAt: first,
	}).WriteView(t.Context(), "op-view", view)
	if err != nil {
		t.Fatalf("the retry: %v — the broker named its first record as this "+
			"operation's, and the adoption brought that record's rows", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.Seq != landed {
		t.Fatalf("the retry = %q at %s, want applied at the first record's "+
			"sequence %d", res.Outcome, res.Position, landed)
	}
	if got, err := r.log.End(t.Context()); err != nil || got != landed {
		t.Errorf("the log ends at %d (%v) after the retry, want %d", got, err, landed)
	}
}

// adopt stands in for this node adopting a donated snapshot taken after every
// record it has applied: the rows stay, the ledger goes, and the adoption row
// records when.
func adopt(t *testing.T, r *roundTrip) {
	t.Helper()
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM `+tracker.Domain{}.OpsTable())
		return err
	}); err != nil {
		t.Fatalf("scrub the ledger: %v", err)
	}
	if err := statelog.RecordAdoption(t.Context(), r.db, time.Now(), "node-b",
		statelog.Manifest{SHA256: "donated"}, statelog.AdoptionComplete); err != nil {
		t.Fatalf("record the adoption: %v", err)
	}
}

// losingAppender loses the answer to one append, under one op id, without
// storing it — a request that never reached the broker.
type losingAppender struct {
	statelog.Appender

	mu   sync.Mutex
	lose string
}

// next arms the loss for the next append under opID.
func (a *losingAppender) next(opID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lose = opID
}

func (a *losingAppender) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	a.mu.Lock()
	lost := a.lose != "" && msgID == a.lose
	if lost {
		a.lose = ""
	}
	a.mu.Unlock()
	if lost {
		return 0, false, errors.New("no response from stream")
	}
	return a.Appender.Append(ctx, subject, msgID, expect, body)
}
