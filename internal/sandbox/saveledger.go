package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A seat's allowance for the sessions its runs save as they work: the package
// doc's "A save taken as its job works" has the argument.

// SaveLedgers is the fleet's ledger of each seat's saves (internal/coord's
// SaveAllowances): one value per seat, read at a revision and written only at
// it. Every error is unknown.
type SaveLedgers interface {
	// SaveLedger reads seat's ledger and its revision: nil and 0 where none.
	SaveLedger(ctx context.Context, seat string) ([]byte, uint64, error)
	// SwapSaveLedger writes seat's ledger only at revision (0 creates it),
	// answering the revision written; 0 with no error is a lost race.
	SwapSaveLedger(ctx context.Context, seat string, value []byte, revision uint64) (uint64, error)
}

// saveLedgerVersion is the ledger's format this build reads and writes: a
// ledger of a higher one is a newer build's, never rewritten here.
const saveLedgerVersion = 1

// saveLedgerCap bounds a seat's ledger: at the 300 s floor a seat running one
// job at a time saves at most 349 times in [saveLedgerHold], plus its drain
// saves, and 512 entries are 52 KiB, within coord.MaxSaveLedgerBytes, so even
// a clock gone wrong never lets a seat's saves past its allowance by count.
const saveLedgerCap = 512

// saveLedgerRaces is how many lost races a save's charge reads again past: a
// seat's saves are charged by the nodes that launched its few runs, so a race
// is two of those at once, never a crowd.
const saveLedgerRaces = 3

// saveLedgerWait bounds each read of a seat's ledger and each charge of it:
// one record's budget, for [keepReadRoom]'s reason, since a small read not
// answered by then is an outage its own alarms name.
const saveLedgerWait = sessionRecordBudget

// collectPendingGrace and collectInterval restate internal/objstore/collect's
// PendingGrace and CollectInterval, which this package must not link; a test
// holds them equal.
const (
	collectPendingGrace = 24 * time.Hour
	collectInterval     = time.Hour
)

// saveLedgerHold is how long a save's entry counts against its seat: the
// take's life before its object is stored, the collector's grace and two of
// its passes, and twice the engine's assumed skew for an end written on one
// node's clock and read on another's (the package doc's "A save taken as its
// job works"). Twenty-nine hours, forty-five and a half minutes at today's
// bounds: FOURTEEN AND A HALF MINUTES under the uploads bucket's age
// (coord.UploadRetention, 30 h), which a test holds, so a raise of the keep's
// or the take's budget past that margin goes red there first.
var saveLedgerHold = takeLife + collectPendingGrace + 2*collectInterval + 2*objstore.AssumedClockSkew

// saveMarginFloor is the least margin a save is counted at beyond its packed
// archive, for what the scrub's re-tar and redaction add (countedFor).
const saveMarginFloor = 64 << 10

// countedFor is the room a take packed at size bytes spools in: sealed, with a
// margin of an eighth or 64 KiB for what the scrub changes, and never past the
// most its bound's objects hold sealed ([takeBound]). The scrub's rewrite measured
// within 1% of the box's pack for media and 2% under it for text
// (internal/sandbox/executor's keptGzipLevel), so the margin is the markers'
// and the re-tar's room; a spool sealing past it is refused. Its seat is
// charged it less the media its keep will most likely carry, and then what
// its keep uploads ([Coordinator.amendSave]).
func countedFor(size, sealed int64) int64 {
	return min(secrets.SealedStreamSize(size+max(size/8, saveMarginFloor),
		strings.Repeat("k", secrets.MaxStreamKeyIDBytes)), sealed)
}

// saveLedger is a seat's ledger as written: every save counted against the
// seat, each until its end.
type saveLedger struct {
	V       int          `json:"v"`
	Entries []savedEntry `json:"entries"`
}

// savedEntry is one save counted against its seat: its id, minted once per
// save so an append asked again counts once, the instant its count ends on
// its writer's clock, and the bytes it was counted at.
type savedEntry struct {
	ID    uuid.UUID `json:"id"`
	Until time.Time `json:"until"`
	Bytes int64     `json:"bytes"`
}

// errNewerLedger is a ledger of a format past [saveLedgerVersion].
var errNewerLedger = errors.New("the seat's save ledger is of a newer build's format, which this build " +
	"neither reads nor rewrites")

// readSaveLedger decodes a seat's ledger: an empty one where none is written.
func readSaveLedger(raw []byte) (saveLedger, error) {
	if len(raw) == 0 {
		return saveLedger{V: saveLedgerVersion}, nil
	}
	var ledger saveLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return saveLedger{}, fmt.Errorf("the seat's save ledger does not decode: %w", err)
	}
	switch {
	case ledger.V > saveLedgerVersion:
		return saveLedger{}, fmt.Errorf("%w (version %d)", errNewerLedger, ledger.V)
	case ledger.V < saveLedgerVersion:
		return saveLedger{}, fmt.Errorf("the seat's save ledger names version %d, which no build writes",
			ledger.V)
	}
	return ledger, nil
}

// live is every entry still counted as of now.
func (l saveLedger) live(now time.Time) []savedEntry {
	var live []savedEntry
	for _, e := range l.Entries {
		if e.Until.After(now) {
			live = append(live, e)
		}
	}
	return live
}

// ledgerHeld is the bytes the entries count.
func ledgerHeld(entries []savedEntry) int64 {
	var sum int64
	for _, e := range entries {
		sum += e.Bytes
	}
	return sum
}

// counts reports whether the ledger holds the entry id names.
func (l saveLedger) counts(id uuid.UUID) bool {
	for _, e := range l.Entries {
		if e.ID == id {
			return true
		}
	}
	return false
}

// seatSaveRoom is rule 8's read: what seat's live saves leave of budget, or
// why no save of the seat is taken now (its ledger unreadable, unanswered or a
// newer build's, [errNewerLedger]).
func (c *Coordinator) seatSaveRoom(ctx context.Context, seat string, budget int64) (int64, error) {
	read, cancel := context.WithTimeout(ctx, saveLedgerWait)
	defer cancel()
	raw, _, err := c.saves.ledger.SaveLedger(read, seat)
	if err != nil {
		return 0, fmt.Errorf("the seat's save ledger could not be read: %w", err)
	}
	ledger, err := readSaveLedger(raw)
	if err != nil {
		return 0, err
	}
	return budget - ledgerHeld(ledger.live(c.now())), nil
}

// chargeSave appends entry to seat's ledger before the save's stamp, the
// compare-and-swap of the package doc's "A save taken as its job works":
// answering "" where it is counted (or already was), [savePaced] where the
// seat's room or the cap refuses it, and [saveNotYet] where the store did not
// answer within [saveLedgerWait], the ledger is a newer build's, or a race was
// lost past [saveLedgerRaces]. An unanswered swap may have been written, which
// over-counts: the safe direction.
func (c *Coordinator) chargeSave(ctx context.Context, seat string, entry savedEntry,
	budget int64) (saveClass, string) {

	return c.swapLedger(ctx, seat, func(raw []byte) ([]byte, saveClass, string) {
		return chargedLedger(raw, entry, budget, c.now())
	})
}

// swapLedger is a charge's compare-and-swap of seat's ledger, decide's
// decision of each value read ([Coordinator.chargeSave] answers as it does).
func (c *Coordinator) swapLedger(ctx context.Context, seat string,
	decide func(raw []byte) ([]byte, saveClass, string)) (saveClass, string) {

	ask, cancel := context.WithTimeout(ctx, saveLedgerWait)
	defer cancel()
	races := 0
	for attempt := 1; ; attempt++ {
		class, why, err := c.chargeOnce(ask, seat, decide)
		switch {
		case err == nil && class == saveRaced:
			if races++; races >= saveLedgerRaces {
				return saveNotYet, fmt.Sprintf("the seat's save ledger moved under %d charges in a row",
					saveLedgerRaces)
			}
			continue
		case err == nil:
			return class, why
		}
		wait := time.NewTimer(backoff.Doubling(attempt, c.sessionRetry, sessionRetryCeiling))
		select {
		case <-ask.Done():
			wait.Stop()
			return saveNotYet, fmt.Sprintf("the seat's save ledger did not answer the save's charge: %v", err)
		case <-wait.C:
		}
	}
}

// chargeOnce is one read, decision and swap of [Coordinator.swapLedger]:
// [saveRaced] for a lost race, an error where the store did not answer.
func (c *Coordinator) chargeOnce(ctx context.Context, seat string,
	decide func(raw []byte) ([]byte, saveClass, string)) (saveClass, string, error) {

	raw, revision, err := c.saves.ledger.SaveLedger(ctx, seat)
	if err != nil {
		return "", "", err
	}
	value, class, why := decide(raw)
	if value == nil {
		return class, why, nil
	}
	wrote, err := c.saves.ledger.SwapSaveLedger(ctx, seat, value, revision)
	switch {
	case err != nil:
		return "", "", err
	case wrote == 0:
		return saveRaced, "", nil
	}
	return "", "", nil
}

// chargedLedger is the decision of a charge of entry to the ledger raw as of
// now: the ledger to write with entry counted, or nil with what the charge
// comes to without a write: "" where entry is counted already, [savePaced]
// where the seat's room or the cap refuses it, and [saveNotYet] where raw is
// unreadable or a newer build's.
func chargedLedger(raw []byte, entry savedEntry, budget int64, now time.Time) ([]byte, saveClass, string) {
	ledger, err := readSaveLedger(raw)
	if err != nil {
		return nil, saveNotYet, err.Error()
	}
	if ledger.counts(entry.ID) {
		return nil, "", ""
	}
	live := ledger.live(now)
	switch {
	case len(live) >= saveLedgerCap:
		return nil, savePaced, fmt.Sprintf("the seat's save ledger counts %d saves, its cap", len(live))
	case ledgerHeld(live)+entry.Bytes > budget:
		return nil, savePaced, fmt.Sprintf("the seat's saves hold %d bytes, and this one is counted at %d: "+
			"past the seat's budget of %d (turn_engine.conversation_session.max_bytes_per_seat)",
			ledgerHeld(live), entry.Bytes, budget)
	}
	value, err := json.Marshal(saveLedger{V: saveLedgerVersion, Entries: append(live, entry)})
	if err != nil {
		return nil, saveNotYet, fmt.Sprintf("the seat's save ledger could not be encoded: %v", err)
	}
	return value, "", ""
}

// saveCharge is a save's entry on its seat's ledger, and the budget it was
// charged against.
type saveCharge struct {
	seat   string
	budget int64
	entry  savedEntry
}

// amendSave has a save's entry count what its keep uploads, its sealed session
// and the media it does not carry: "" where it does, or the keep is no save's;
// else why the save is refused, the amended charge past the seat's budget, or
// one raised that the ledger did not take. A lowered charge the ledger did not
// take over-counts, the safe direction, and refuses nothing.
func (c *Coordinator) amendSave(ctx context.Context, spooled *spooledCapture) string {
	charge := spooled.charge
	if charge == nil || charge.entry.Bytes == spooled.uploads() {
		return ""
	}
	amended := charge.entry
	amended.Bytes = spooled.uploads()
	class, why := c.swapLedger(ctx, charge.seat, func(raw []byte) ([]byte, saveClass, string) {
		return amendedLedger(raw, charge.entry.Bytes, amended, charge.budget, c.now())
	})
	switch {
	case class == "":
		charge.entry = amended
		return ""
	case class == savePaced:
		return why
	case amended.Bytes < charge.entry.Bytes:
		return ""
	}
	return fmt.Sprintf("the save's charge could not be raised to the %d bytes it uploads: %s", amended.Bytes, why)
}

// amendedLedger is the decision of moving entry's charge on the ledger raw as
// of now, from was to entry's: the ledger to write, or nil with what it comes
// to without a write, as [chargedLedger] answers. A raise is held to budget;
// an entry the ledger no longer counts is charged again.
func amendedLedger(raw []byte, was int64, entry savedEntry, budget int64, now time.Time) ([]byte, saveClass,
	string) {

	ledger, err := readSaveLedger(raw)
	if err != nil {
		return nil, saveNotYet, err.Error()
	}
	live := ledger.live(now)
	at := slices.IndexFunc(live, func(e savedEntry) bool { return e.ID == entry.ID })
	switch {
	case at < 0:
		return chargedLedger(raw, entry, budget, now)
	case live[at].Bytes == entry.Bytes:
		return nil, "", ""
	}
	live[at].Bytes = entry.Bytes
	if held := ledgerHeld(live); entry.Bytes > was && held > budget {
		return nil, savePaced, fmt.Sprintf("the seat's saves hold %d bytes with this one's upload of %d: past "+
			"the seat's budget of %d (turn_engine.conversation_session.max_bytes_per_seat)", held, entry.Bytes,
			budget)
	}
	value, err := json.Marshal(saveLedger{V: saveLedgerVersion, Entries: live})
	if err != nil {
		return nil, saveNotYet, fmt.Sprintf("the seat's save ledger could not be encoded: %v", err)
	}
	return value, "", ""
}
