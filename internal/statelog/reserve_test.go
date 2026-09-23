package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE GATE RESERVE: ordinary writes are refused `log_full` at the ceiling less
// the reserve, and the records that install or lift a gate still land above
// it — because the one gesture that unpins a log a gone node has filled is a
// record on that log.

// reserveCeiling is the byte ceiling the unit cases below admit against, and
// softCeiling what ordinary appends are held to under it.
const (
	reserveCeiling = 16 << 20
	softCeiling    = reserveCeiling - reserveCeiling/statelog.GateReserveDivisor
)

// usageOf is a reserve whose every reading answers bytes held of the unit
// cases' ceiling.
func usageOf(t *testing.T, bytes uint64) *statelog.Reserve {
	t.Helper()
	reserve, err := statelog.NewReserve(probeStream,
		func(context.Context) (statelog.Usage, error) {
			return statelog.Usage{Bytes: bytes, MaxBytes: reserveCeiling}, nil
		})
	if err != nil {
		t.Fatalf("NewReserve: %v", err)
	}
	return reserve
}

// logFull reports whether err is the full-log refusal.
func logFull(err error) bool {
	var refusal *statelog.Unavailable
	return errors.As(err, &refusal) && refusal.Reason == statelog.ReasonLogFull
}

// AN ORDINARY APPEND IS REFUSED AT THE SOFT CEILING, not at the broker's.
//
// Exactly at it: an append that brings the log to the soft ceiling is
// admitted, and a byte more is refused `log_full` — naming the ceiling it was
// held to and the verb that raises it. A reserve that admitted to the broker's
// own ceiling would leave an eviction nothing to land in, and one that refused
// short of the soft ceiling would spend capacity nothing ever uses.
func TestAnOrdinaryAppendIsRefusedAtTheSoftCeiling(t *testing.T) {
	t.Parallel()
	const size = 4096
	release, err := usageOf(t, softCeiling-size).Admit(t.Context(), size)
	if err != nil {
		t.Fatalf("an append bringing the log exactly to its soft ceiling was "+
			"refused: %v", err)
	}
	release()

	_, err = usageOf(t, softCeiling-size).Admit(t.Context(), size+1)
	if !logFull(err) {
		t.Fatalf("an append a byte past the soft ceiling answered %v, want "+
			"log_full — the reserve above it is what an eviction lands in", err)
	}
	for _, want := range []string{
		fmt.Sprintf("held to %d of its %d-byte ceiling", softCeiling, reserveCeiling),
		"set-capacity",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if statelog.OrdinaryCeiling(reserveCeiling, true) != softCeiling {
		t.Errorf("the ordinary ceiling of a %d-byte log is %d, want %d",
			reserveCeiling, statelog.OrdinaryCeiling(reserveCeiling, true), softCeiling)
	}
	if statelog.OrdinaryCeiling(reserveCeiling, false) != reserveCeiling {
		t.Error("a log that keeps no reserve holds its ordinary writes short of " +
			"its ceiling, refusing writes to keep room nothing is written into")
	}
}

// THIS NODE'S APPENDS IN FLIGHT ARE COUNTED BESIDE WHAT THE BROKER HOLDS.
//
// A reading cannot see an append that has not landed, so two appends admitted
// against one figure would each fit and both land past the line. Counted, the
// second is refused until the first has been answered and released — and a
// release is spent once however often it is called, since a double release
// would hand the budget bytes nothing holds.
func TestAReserveCountsThisNodesAppendsInFlight(t *testing.T) {
	t.Parallel()
	const size = 4096
	reserve := usageOf(t, softCeiling-2*size)
	first, err := reserve.Admit(t.Context(), size)
	if err != nil {
		t.Fatalf("the first append was refused: %v", err)
	}
	if _, err := reserve.Admit(t.Context(), size+1); !logFull(err) {
		t.Fatalf("a second append that fits only if the first is not counted "+
			"answered %v, want log_full", err)
	}
	first()
	first()
	second, err := reserve.Admit(t.Context(), size+1)
	if err != nil {
		t.Fatalf("with the first append answered, the second was still "+
			"refused: %v", err)
	}
	defer second()
	if _, err := reserve.Admit(t.Context(), size); !logFull(err) {
		t.Fatalf("a released append was released twice — a third append that "+
			"fits only if the first's bytes are counted as freed twice answered "+
			"%v, want log_full", err)
	}
}

// A READING ALREADY OUT WHEN AN ADMISSION ARRIVES IS NOT ITS READING.
//
// It may have been answered before a peer's append this admission has to
// count. The admission that started it takes it; one that arrives while it is
// out waits for the next, which here has seen the log fill.
func TestAReadingAlreadyOutIsNotALaterAdmissionsReading(t *testing.T) {
	t.Parallel()
	const size = 4096
	started, answer := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	reserve, err := statelog.NewReserve(probeStream,
		func(context.Context) (statelog.Usage, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-answer
				return statelog.Usage{Bytes: 0, MaxBytes: reserveCeiling}, nil
			}
			return statelog.Usage{Bytes: softCeiling, MaxBytes: reserveCeiling}, nil
		})
	if err != nil {
		t.Fatalf("NewReserve: %v", err)
	}
	early, late := make(chan error, 1), make(chan error, 1)
	go func() {
		release, err := reserve.Admit(context.Background(), size)
		if err == nil {
			release()
		}
		early <- err
	}()
	<-started
	go func() {
		release, err := reserve.Admit(context.Background(), size)
		if err == nil {
			release()
		}
		late <- err
	}()
	// LONG ENOUGH FOR THE LATE ADMISSION TO BE WAITING ON THE READING
	// ALREADY OUT, which is the only way it could wrongly take it. The
	// verdict does not depend on it: an admission that got there later
	// would start the next reading itself.
	time.Sleep(50 * time.Millisecond)
	close(answer)
	if err := <-early; err != nil {
		t.Fatalf("the admission whose own reading found an empty log was "+
			"refused: %v", err)
	}
	if err := <-late; !logFull(err) {
		t.Fatalf("the admission that arrived while a reading was out answered "+
			"%v, want log_full from the reading after it", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the log was read %d times, want 2", n)
	}
}

// AN APPEND THAT LANDS WHILE THE READING IS OUT IS STILL COUNTED.
//
// What this node has in flight is taken when the reading starts, not when it
// answers: an append that lands and releases in between is in neither the
// broker's figure (which was taken before it landed) nor a count taken
// afterwards, so an admission counted that way let it through uncounted.
func TestAnAppendThatLandsDuringTheReadingIsStillCounted(t *testing.T) {
	t.Parallel()
	const size = 4096
	started, answer := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	held := uint64(softCeiling - 2*size)
	reserve, err := statelog.NewReserve(probeStream,
		func(context.Context) (statelog.Usage, error) {
			if calls.Add(1) == 2 {
				close(started)
				<-answer
			}
			// NEITHER READING SEES THE FIRST APPEND LAND: the second
			// was taken before it did.
			return statelog.Usage{Bytes: held, MaxBytes: reserveCeiling}, nil
		})
	if err != nil {
		t.Fatalf("NewReserve: %v", err)
	}
	first, err := reserve.Admit(t.Context(), size)
	if err != nil {
		t.Fatalf("the first append was refused: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		release, err := reserve.Admit(context.Background(), size+1)
		if err == nil {
			release()
		}
		second <- err
	}()
	<-started
	first()
	close(answer)
	if err := <-second; !logFull(err) {
		t.Fatalf("an append admitted against a reading taken while the first "+
			"was in flight answered %v, want log_full — the first landed after "+
			"the broker was read and was released before the count was", err)
	}
}

// A RECORD LARGER THAN THE BUDGET IS REFUSED AS TOO LARGE, and one the reading
// cannot size is refused as that — neither is a full log.
//
// A record above the budget would wait for room the budget can never hold, and
// calling either refusal `log_full` sends an operator to raise a ceiling that
// is not the problem. Either way the budget it took is given back.
func TestAReserveRefusesWhatItCannotAdmitWithoutCallingItFull(t *testing.T) {
	t.Parallel()
	_, err := usageOf(t, 0).Admit(t.Context(), statelog.MaxAppendBytes+1)
	if !errors.Is(err, queue.ErrTooLarge) || logFull(err) {
		t.Fatalf("a record past the budget answered %v, want ErrTooLarge", err)
	}

	unread := errors.New("the stream's state could not be read")
	var fail atomic.Bool
	fail.Store(true)
	reserve, err := statelog.NewReserve(probeStream,
		func(context.Context) (statelog.Usage, error) {
			if fail.Load() {
				return statelog.Usage{}, unread
			}
			return statelog.Usage{MaxBytes: reserveCeiling}, nil
		})
	if err != nil {
		t.Fatalf("NewReserve: %v", err)
	}
	if _, err := reserve.Admit(t.Context(), statelog.MaxAppendBytes); !errors.Is(err, unread) || logFull(err) {
		t.Fatalf("an unreadable log answered %v, want the reading's own error", err)
	}
	fail.Store(false)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	release, err := reserve.Admit(ctx, statelog.MaxAppendBytes)
	if err != nil {
		t.Fatalf("a whole budget's append after a failed reading answered %v — "+
			"the failed admission kept the budget it took", err)
	}
	release()
}

// smallGatingDomain is a log small enough to fill in a test, on which an
// eviction record installs a gate.
type smallGatingDomain struct{ gatingDomain }

// smallLogBytes is its ceiling: sixteen reserves of 64 KiB.
const smallLogBytes = 1 << 20

func (smallGatingDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.MaxBytes = smallLogBytes
	return spec
}

// evictionRecord is a gate record on the probe log: an eviction, which
// [gatingDomain] says installs a gate, carrying body.
func evictionRecord(stamp statelog.Stamp, opID, body string) []byte {
	payload, err := json.Marshal(struct {
		statelog.Envelope
		Body string
	}{
		Envelope: statelog.Envelope{
			V: 1, Kind: "eviction", Op: "evict", OpID: opID,
			Gen: stamp.Gen, Writer: stamp.Writer,
		},
		Body: body,
	})
	if err != nil {
		panic(fmt.Sprintf("encode an eviction record: %v", err))
	}
	return payload
}

// gate publishes one gate record through the harness's publisher.
func (h *harness) gate(node, opID, body string, record func(statelog.Stamp, string, string) []byte) (statelog.Result, error) {
	h.t.Helper()
	subject := statelog.Subject{Kind: "eviction", ID: node}
	return h.pub.Publish(h.t.Context(), statelog.Request{
		Subject:  subject,
		Scope:    statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID:     opID,
		Pattern:  statelog.PatternArbitrated,
		NodeGate: true,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: record(stamp, opID, body), Version: 1}, nil
		},
	})
}

// A LOG FULL FOR ORDINARY WRITES STILL TAKES A GATE RECORD.
//
// Through the whole write authority on a real broker: ordinary writes fill the
// log until one is refused `log_full` — before the broker is asked, and short
// of the broker's own ceiling — and an eviction then lands PAST the ordinary
// ceiling, in the reserve, where every ordinary write after it is still
// refused. And a write that asks for the reserve with a record the domain
// says installs no gate is refused outright, since a flag the caller sets
// alone would let any write spend it.
func TestALogFullForOrdinaryWritesStillTakesAGateRecord(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, smallGatingDomain{})
	h.applier.auto = true
	body := strings.Repeat("x", 60<<10)
	soft := statelog.OrdinaryCeiling(smallLogBytes, true)

	var refused error
	for i := 0; i < 32 && refused == nil; i++ {
		_, refused = h.write(statelog.Subject{Kind: "object", ID: fmt.Sprintf("o%d", i)},
			fmt.Sprintf("fill-%d", i), body)
	}
	if !logFull(refused) {
		t.Fatalf("filling a %d-byte log with ordinary writes ended %v, want "+
			"log_full", smallLogBytes, refused)
	}
	before, err := h.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log's usage: %v", err)
	}
	if before.Bytes > soft {
		t.Fatalf("ordinary writes filled the log to %d bytes, past the %d they "+
			"are held to", before.Bytes, soft)
	}
	appended := h.appends.appends.Load()
	if _, err := h.write(statelog.Subject{Kind: "object", ID: "late"}, "late-1", body); !logFull(err) {
		t.Fatalf("an ordinary write on the full log answered %v, want log_full", err)
	}
	if h.appends.appends.Load() != appended {
		t.Fatal("the refused write was sent to the broker — the reserve refuses " +
			"it on this node's own reading, before anything can land")
	}

	// A GATE RECORD SIZED TO CROSS THE ORDINARY CEILING, so landing at all
	// is landing in the reserve.
	gateBody := strings.Repeat("g", int(soft-before.Bytes)+(1<<10))
	if _, err := h.gate("node-b", "evict-1", gateBody, probeRecord); err == nil ||
		!strings.Contains(err.Error(), "installs no gate") {
		t.Fatalf("a write asking for the reserve with a record that installs no "+
			"gate answered %v, want it refused", err)
	}
	if h.appends.appends.Load() != appended {
		t.Fatal("a record that installs no gate reached the broker through the " +
			"reserve")
	}
	res, err := h.gate("node-b", "evict-1", gateBody, evictionRecord)
	if err != nil {
		t.Fatalf("an eviction on a log full for ordinary writes was refused: %v "+
			"— that is the record that unpins it", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Errorf("the eviction resolved %s, want applied", res.Outcome)
	}
	after, err := h.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log's usage: %v", err)
	}
	if after.Bytes <= soft || after.Bytes > smallLogBytes {
		t.Fatalf("with the eviction landed the log holds %d bytes, want it in "+
			"the reserve between %d and %d", after.Bytes, soft, smallLogBytes)
	}
	if _, err := h.write(statelog.Subject{Kind: "object", ID: "later"}, "later-1", "y"); !logFull(err) {
		t.Fatalf("an ordinary write after the eviction answered %v, want "+
			"log_full — the reserve is not room for ordinary writes", err)
	}
}

// AN EVICTION PASSES THE TRUNCATION FENCE AND THE GATE RESERVE TOGETHER.
//
// The two faults come as a pair more often than apart: a peer whose rows hold
// what a restored log lost stops every write on it (`log_truncated`), and a log
// nothing can write to is a log nothing trims, so it fills. Evicting that peer
// is the way out of both, so an eviction must pass each fence with the other up
// — excused by one and refused by the other, it leaves the operator exactly the
// loop either fence alone was built to end.
func TestAnEvictionPassesTheTruncationFenceAndTheReserveTogether(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, smallGatingDomain{})
	h.applier.auto = true
	body := strings.Repeat("x", 60<<10)
	soft := statelog.OrdinaryCeiling(smallLogBytes, true)

	var refused error
	for i := 0; i < 32 && refused == nil; i++ {
		_, refused = h.write(statelog.Subject{Kind: "object", ID: fmt.Sprintf("o%d", i)},
			fmt.Sprintf("fill-%d", i), body)
	}
	if !logFull(refused) {
		t.Fatalf("filling a %d-byte log with ordinary writes ended %v, want "+
			"log_full", smallLogBytes, refused)
	}
	before, err := h.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log's usage: %v", err)
	}

	h.applier.truncatedBy("node-newer")
	var refusal *statelog.Unavailable
	if _, err := h.write(statelog.Subject{Kind: "object", ID: "late"}, "late-1", "y"); !errors.As(err, &refusal) ||
		refusal.Reason != statelog.ReasonLogTruncated {
		t.Fatalf("an ordinary write on the truncated, full log answered %v, "+
			"want %s", err, statelog.ReasonLogTruncated)
	}

	gateBody := strings.Repeat("g", int(soft-before.Bytes)+(1<<10))
	res, err := h.gate("node-newer", "evict-1", gateBody, evictionRecord)
	if err != nil {
		t.Fatalf("the eviction of the peer holding what the log lost, on a log "+
			"full for ordinary writes, was refused: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Errorf("the eviction resolved %s, want applied", res.Outcome)
	}
	after, err := h.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log's usage: %v", err)
	}
	if after.Bytes <= soft || after.Bytes > smallLogBytes {
		t.Fatalf("with the eviction landed the log holds %d bytes, want it in "+
			"the reserve between %d and %d", after.Bytes, soft, smallLogBytes)
	}
}
