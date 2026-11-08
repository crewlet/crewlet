// Package executordomaintest is ONE DATA NODE'S COPY OF THE EXECUTOR LOG, for a
// suite that may not name the state log: internal/sandbox's, whose dependency
// gate refuses an import of it, or of anything under it, in every file of the
// package, its tests included. Its cases turn on what the log itself decides
// of the records a coordinator makes — whether a turn's end takes the session
// the turn's row names off it, which capture the row names once a late record
// lands, which turn a conversation's next turn is handed — and a double that
// played those rules would hold the sandbox to its own reading of them rather
// than to the domain's.
//
// THE SHIPPED APPLIER AND THE SHIPPED READER, over a migrated replicated estate
// of its own: every record is applied at the next position on the log, through
// the applier's gate and its apply in one transaction, as the framework's loop
// applies it; every read goes through the domain's reader, served at the
// position of the last record applied, as a node caught up to it serves one.
// What it leaves out is the writer's decide — the refusals it makes before a
// record is published — which the domain's own suite holds over a real broker
// (internal/executordomain's writer_test.go): a case states each record as the
// log carries it, and so states none the writer would refuse.
package executordomaintest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// Log is one data node's copy of the executor log: its replicated estate and
// the next position on the log. Safe for concurrent use, since a coordinator
// records on it from its keeps' goroutines as well as from a case's own.
//
// EVERY METHOD ANSWERS AN ERROR rather than failing its case, because it is
// called from those goroutines too, where a test may not be failed; a case
// fails on what the coordinator then does with the answer.
type Log struct {
	db      store.ReplicatedHandle
	applier *executordomain.Applier

	mu sync.Mutex
	// seq is the position of the last record applied, 0 before the first.
	seq uint64
}

// Open is a Log over a migrated replicated estate of its own, closed with t.
func Open(t testing.TB) *Log {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the executor log's store: %v", err)
		}
	})
	return &Log{db: db, applier: executordomain.NewApplier()}
}

// writer is the data node every record names as the one that published it,
// which the applier's eviction gate reads: a node this log never evicted.
const writer = "data-1"

// firstStored is the broker's instant of the log's first record, and every
// record after it is stored a second later than the one before: the only
// instant a row keeps of its own, fixed so two runs of a case apply the same
// rows, as the framework's own suite applies them.
var firstStored = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// Capture applies c, a capture of a turn's session as its record carries it,
// and answers whether the turn's row names its object afterwards: the one fact
// a capture's answer turns on, read back as the writer reads it.
func (l *Log) Capture(ctx context.Context, c executordomain.Capture) (bool, error) {
	// THE SEAT, as the writer declares it: a capture past the seat's budget
	// releases the seat's oldest ended sessions, which may be any
	// conversation's.
	scope := executordomain.ScopeSet{Terms: []executordomain.ScopeTerm{
		{Kind: executordomain.TermSeat, Seat: c.Handle}}}
	if err := l.apply(ctx, executordomain.ConversationSubject(c.Handle, c.ConversationKey, c.Turn),
		executordomain.OpCapture, scope, c); err != nil {
		return false, err
	}
	read, err := l.Session(ctx, c.Handle, c.Turn)
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		// NO ROW AT ALL: the apply wrote nothing for the capture.
		return false, nil
	case err != nil:
		return false, err
	}
	return read.Session.HasObject() && read.Session.Object.Key.String() == c.Object, nil
}

// End applies e, how a turn ended as its record carries it.
func (l *Log) End(ctx context.Context, e executordomain.End) error {
	return l.apply(ctx, executordomain.ConversationSubject(e.Handle, e.ConversationKey, e.Turn),
		executordomain.OpEnd, executordomain.ScopeSet{Subject: true}, e)
}

// Session is a turn's row as the domain's reader answers it, and the reader's
// own refusal where the log holds none ([executordomain.ErrNotFound]). A turn
// is a run id in its canonical spelling, as the engine mints one, or the
// reader refuses it.
func (l *Log) Session(ctx context.Context, handle, turn string) (executordomain.SessionRead, error) {
	reader, err := l.reader()
	if err != nil {
		return executordomain.SessionRead{}, err
	}
	return reader.Session(ctx, handle, turn, statelog.Freshness{Level: statelog.ReadStale})
}

// Head is a conversation's head as the domain's reader answers it: the
// session that conversation's next turn is handed.
func (l *Log) Head(ctx context.Context, handle, conversationKey string) (executordomain.Head, error) {
	reader, err := l.reader()
	if err != nil {
		return executordomain.Head{}, err
	}
	return reader.Head(ctx, handle, conversationKey, statelog.Freshness{Level: statelog.ReadStale})
}

// apply encodes one record, as a writer of this build stamps it, and applies it
// at the next position: the gate first, then the apply, in one transaction.
func (l *Log) apply(ctx context.Context, subject executordomain.Subject, op executordomain.OpKind,
	scope executordomain.ScopeSet, payload any) error {

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("executordomaintest: encode the %s payload: %w", op, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	next := l.seq + 1
	stored := firstStored.Add(time.Duration(next) * time.Second)
	encoded, err := executordomain.Encode(executordomain.MutationRecord{
		RecordEnvelope: executordomain.RecordEnvelope{
			OpID: statelog.NewOpID(stored, writer), Subject: subject, Op: op,
			CreatedAt: stored, Gen: 1, Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "sandbox", ActorKind: iam.ActorSystem,
	})
	if err != nil {
		return fmt.Errorf("executordomaintest: encode the %s record: %w", op, err)
	}
	domain := executordomain.Domain{}
	env, err := domain.Envelope(encoded)
	if err != nil {
		return fmt.Errorf("executordomaintest: envelope the %s record: %w", op, err)
	}
	record := statelog.Record{Envelope: env, Position: position(next), Payload: encoded, StoredAt: stored}
	if txErr := l.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, gated, gateErr := l.applier.Gated(ctx, tx, record); gateErr != nil || gated {
			return gateErr
		}
		_, applyErr := l.applier.Apply(ctx, tx, record, statelog.ApplyOptions{
			Now: stored, StoredAt: stored, ArbitratedKinds: domain.Stream().ArbitratedKinds,
			// THE PROBED LIMIT, as the framework's loop hands it: an
			// applier sizes its multi-row writes by it.
			MaxVariables: l.db.Caps().MaxVariables,
		})
		return applyErr
	}); txErr != nil {
		return fmt.Errorf("executordomaintest: apply the %s record at %s: %w", op, record.Position, txErr)
	}
	l.seq = next
	return nil
}

// reader is the domain's reader over l's rows, through the framework's read
// authority, served at the position of the last record applied.
func (l *Log) reader() (*executordomain.Reader, error) {
	l.mu.Lock()
	at := position(l.seq)
	l.mu.Unlock()
	authority, err := statelogtest.LocalReader(executordomain.Domain{}, l.db, at)
	if err != nil {
		return nil, fmt.Errorf("executordomaintest: build the read authority: %w", err)
	}
	return executordomain.NewReader(executordomain.ReaderOptions{DB: l.db.Reader(), Log: authority})
}

// position is the place of the record at seq on the log, in its first
// generation.
func position(seq uint64) statelog.Position {
	return statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1, Seq: seq}
}
