package artifactupload

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE LEDGER: every hold the company has outstanding, in one coordination
// record, so a reservation checking the company's total and adding to it is
// one compare-and-set and two intents cannot both win the last of a budget.
//
// A hold is the bytes one upload may have put in the object store that no
// record names: every file it minted a key for and has not seen named or
// deleted. It lasts until the collector could have deleted them ([holdTail]
// past the last instant one can be stored), so unnamed bytes never exceed the
// holds. It is released early, never late: after the record naming the bytes
// or the engine deleting them; one not released lapses on its own.

// ledgerVersion is the ledger's format.
const ledgerVersion = 1

// hold is one upload's reservation.
type hold struct {
	Upload    string `json:"upload"`
	Principal string `json:"principal"`
	Bytes     int64  `json:"bytes"`

	// Until is when the hold lapses on its own.
	Until time.Time `json:"until"`

	// Expires is when the hold may next be released early: the upload's
	// transfer window, past which an abandoned upload the engine streamed is
	// released, moved [earlyRecheck] on each time its record is read and not
	// releasable ([Service.releaseAbandoned]).
	Expires time.Time `json:"expires"`

	// Direct reports a client putting bytes straight to the bucket, whose
	// puts the engine never sees: such a hold is never released early.
	Direct bool `json:"direct,omitempty"`

	// Attempt is the intent attempt the hold reserves for, drawn afresh
	// where one reserves it or a retry takes it up, and changed by nothing
	// else: what tells a released hold from one a retry took up since.
	Attempt uint32 `json:"attempt"`
}

// remains reports whether h is the reservation o was, or what a release left
// of it: the same attempt's, its directness unchanged, no more bytes, since
// [Service.release] lowers them, and no later lapse, which a later intent's
// hold has whatever attempt it drew. Its window is not compared, since any
// change's recheck moves it ([earlyRecheck]).
func (h hold) remains(o hold) bool {
	return h.Upload == o.Upload && h.Principal == o.Principal && h.Direct == o.Direct &&
		h.Attempt == o.Attempt && h.Bytes <= o.Bytes && !h.Until.After(o.Until)
}

// newAttempt draws an attempt other than old.
func newAttempt(old uint32) uint32 {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:]) // crypto/rand.Read never fails: it crashes rather than answer short.
		if a := binary.BigEndian.Uint32(b[:]); a != old {
			return a
		}
	}
}

// live reports a hold that still counts.
func (h hold) live(now time.Time) bool { return h.Until.After(now) && h.Bytes > 0 }

// ledger is every hold.
type ledger struct {
	V     int    `json:"v"`
	Holds []hold `json:"holds"`
}

// totals is what a ledger holds for one principal and for the company.
type totals struct {
	principalBytes, companyBytes   int64
	principalCount, companyCount   int64
	principalUntil, companyEarlier time.Time
}

// totals sums the live holds, principal's apart.
func (l *ledger) totals(principal string, now time.Time) totals {
	var t totals
	for _, h := range l.Holds {
		if !h.live(now) {
			continue
		}
		t.companyBytes += h.Bytes
		t.companyCount++
		if t.companyEarlier.IsZero() || h.Until.Before(t.companyEarlier) {
			t.companyEarlier = h.Until
		}
		if h.Principal == principal {
			t.principalBytes += h.Bytes
			t.principalCount++
			if t.principalUntil.IsZero() || h.Until.Before(t.principalUntil) {
				t.principalUntil = h.Until
			}
		}
	}
	return t
}

// holdOf is the index of the upload's hold, -1 for none.
func (l *ledger) holdOf(upload string) int {
	return slices.IndexFunc(l.Holds, func(h hold) bool { return h.Upload == upload })
}

// admit refuses more bytes for principal's upload than the budgets allow:
// the principal's pending bytes and uploads, the company's
// [artifacts.CompanyPendingFactor] times those, and the company's quota over
// what its artifacts already name. adding reports a new hold rather than a
// grown one, which counts against the uploads bound.
func (l *ledger) admit(principal string, bytes int64, adding bool, limits artifacts.Limits,
	named int64, now time.Time) error {

	t := l.totals(principal, now)
	factor := artifacts.CompanyPendingFactor
	switch {
	case t.principalBytes+bytes > limits.PendingBytes:
		return &UploadsOutstanding{Scope: "principal", Limit: limits.PendingBytes,
			Held: t.principalBytes, Until: t.principalUntil}
	case t.companyBytes+bytes > factor*limits.PendingBytes:
		return &UploadsOutstanding{Scope: "company", Limit: factor * limits.PendingBytes,
			Held: t.companyBytes, Until: t.companyEarlier}
	case adding && t.principalCount+1 > MaxOutstandingUploads:
		return &UploadsOutstanding{Scope: "principal", Limit: MaxOutstandingUploads,
			Held: t.principalCount, Uploads: true, Until: t.principalUntil}
	case adding && t.companyCount+1 > factor*MaxOutstandingUploads:
		return &UploadsOutstanding{Scope: "company", Limit: factor * MaxOutstandingUploads,
			Held: t.companyCount, Uploads: true, Until: t.companyEarlier}
	case named+t.companyBytes+bytes > limits.CompanyBytes:
		// THE QUOTA COUNTS WHAT IS RESERVED BESIDE WHAT IS NAMED: bytes
		// on their way to a record are bytes the store holds, and two
		// intents that each fit beside the named total could not both
		// be recorded.
		return &artifactsdomain.QuotaExceeded{Limit: limits.CompanyBytes,
			Usage: named + t.companyBytes, Key: artifactsdomain.QuotaKeyCompanyBytes}
	}
	return nil
}

// ledgerAttempts bounds one change's compare-and-set rounds on the ledger.
//
// THIRTY-TWO: the ledger is the company's one reservation record, so every
// intent and every release in the fleet contends on it, and a round is one
// store round trip. A company publishing tens of times a second loses a few
// rounds; thirty-two lost in a row is a store that is not answering.
const ledgerAttempts = 32

// errLostLedger is a ledger write that lost every round.
var errLostLedger = errors.New("artifactupload: the reservation ledger kept changing under this " +
	"write; try again")

// earlyReads bounds how many upload records one ledger change reads, across
// its rounds, to release abandoned uploads: each read is a record's crossing
// (coord.MaxUploadRecordBytes) inside the request that happened to find them,
// whether or not it releases anything, so the change pays a bounded cost.
const earlyReads = 4

// earlyRecheck is how long a hold whose record was read and not releasable
// (claimed by a publish, a file still streaming, or gone) waits to be read
// again, so the holds behind it are read in the meantime: [claimLease], the
// longest a live publish keeps a record it claimed from being released.
const earlyRecheck = claimLease

// changeLedger applies change to the current ledger and writes it, reading
// again on a lost race. Before each round it releases what has lapsed or been
// abandoned. change may refuse, which writes nothing of it; the round still
// writes the abandoned uploads it released and the holds it moved on, whose
// records and objects it has already acted on, and answers the refusal however
// that write ends: early release is best effort, and a hold it could not drop
// lapses on its own.
func (s *Service) changeLedger(ctx context.Context, change func(l *ledger, now time.Time) error) error {
	// READ AND RELEASED ACROSS ROUNDS: an upload released in a round that
	// then lost its write has no record left to find in the next, and one
	// read and kept is not read again. A released hold is the one released,
	// so a later round drops it only while it is unchanged: a retried intent
	// may have taken it up since ([Service.reserve]).
	read, released := map[string]bool{}, map[string]hold{}
	var refusal error
	for range ledgerAttempts {
		raw, rev, err := s.res.UploadLedger(ctx)
		if err != nil {
			return s.decided(ctx, refusal, fmt.Errorf("artifactupload: read the reservation ledger: %w", err))
		}
		var l ledger
		if raw != nil {
			if err := json.Unmarshal(raw, &l); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return fmt.Errorf("artifactupload: the reservation ledger is not one this build reads: %w", err)
			}
			if l.V > ledgerVersion {
				return fmt.Errorf("artifactupload: the reservation ledger was written by a newer build "+
					"(format %d)", l.V)
			}
		}
		now := s.now()
		s.releaseAbandoned(ctx, &l, now, read, released)
		acted, drop := false, map[string]bool{}
		for i, h := range l.Holds {
			if seen, ok := released[h.Upload]; ok {
				drop[h.Upload] = h.remains(seen)
				acted = acted || drop[h.Upload]
				continue
			}
			if read[h.Upload] && !h.Expires.After(now) {
				l.Holds[i].Expires, acted = now.Add(earlyRecheck), true
			}
		}
		l.Holds = slices.DeleteFunc(l.Holds, func(h hold) bool {
			return !h.Until.After(now) || h.Bytes <= 0 || drop[h.Upload]
		})
		kept := slices.Clone(l.Holds)
		refusal = change(&l, now)
		if refusal != nil {
			if !acted {
				return refusal
			}
			l.Holds = kept
		}
		for i, h := range l.Holds {
			// A HOLD THIS CHANGE MADE for an upload whose hold it released
			// draws an attempt of its own, so no later round takes it for that.
			if seen, ok := released[h.Upload]; ok && drop[h.Upload] && h.Attempt == seen.Attempt {
				l.Holds[i].Attempt = newAttempt(seen.Attempt)
			}
		}
		l.Holds = slices.DeleteFunc(l.Holds, func(h hold) bool { return h.Bytes <= 0 })
		l.V = ledgerVersion
		encoded, err := json.Marshal(l)
		if err != nil {
			return fmt.Errorf("artifactupload: encode the reservation ledger: %w", err)
		}
		wrote, err := s.res.SwapUploadLedger(ctx, encoded, rev)
		if err != nil {
			return s.decided(ctx, refusal, fmt.Errorf("artifactupload: write the reservation ledger: %w", err))
		}
		if wrote != 0 {
			s.gauge(metrics.ArtifactsUploadReservedBytes, float64(l.totals("", now).companyBytes))
			return refusal
		}
	}
	return s.decided(ctx, refusal, errLostLedger)
}

// decided is a change's answer where its ledger write failed with err: the
// refusal it decided, if it decided one, the failed write of its early
// releases logged; err otherwise.
func (s *Service) decided(ctx context.Context, refusal, err error) error {
	if refusal == nil {
		return err
	}
	log.WarnContext(ctx, "artifact_upload_release_failed", "error", err.Error())
	return refusal
}

// releaseAbandoned releases, ahead of their lapse, the holds of uploads whose
// transfer window has closed with no publish having claimed them, where the
// engine knows every put and no write of unknown outcome was offered a file
// ([record.keeps]): it deletes the upload's record first (so no transfer or
// finalize can take it after), then every object it stored, and adds the holds
// that may go to released, as it saw them. A hold whose objects could not all
// be deleted is kept, and lapses on its own. It reads at most [earlyReads]
// records not in read, adding each to it; the change moves each kept one's
// Expires on, so a hold it cannot release never pins the reads.
func (s *Service) releaseAbandoned(ctx context.Context, l *ledger, now time.Time, read map[string]bool,
	released map[string]hold) {

	for _, h := range l.Holds {
		if len(read) >= earlyReads {
			return
		}
		if read[h.Upload] || h.Direct || !h.live(now) || h.Expires.After(now) {
			continue
		}
		read[h.Upload] = true
		r, rev, err := s.readRecord(ctx, h.Upload)
		// A RECORD KEEPING BYTES A WRITE OF UNKNOWN OUTCOME MAY NAME keeps its
		// hold until it lapses, which no early release may shorten.
		if err != nil || r == nil || r.State != stateOpen || r.keeps() {
			continue
		}
		if slices.ContainsFunc(r.Files, func(f file) bool { return f.State == fileStreaming }) {
			continue
		}
		gone, err := s.res.DeleteUploadRecord(ctx, r.ID, rev)
		if err != nil || !gone {
			continue
		}
		if s.deleteStored(ctx, r.Files) {
			released[h.Upload] = h
		}
	}
}

// deleteStored deletes the object of every file that may hold bytes, reporting
// whether every delete succeeded. Deleting what is not there is not an error.
func (s *Service) deleteStored(ctx context.Context, files []file) bool {
	ok := true
	for _, f := range files {
		if !f.held() {
			continue
		}
		if err := s.objects.Delete(context.WithoutCancel(ctx), f.Key); err != nil {
			log.WarnContext(ctx, "artifact_upload_delete_failed", "key", f.Key.String(),
				"error", err.Error())
			ok = false
		}
	}
	return ok
}

// reserve adds a hold, refused past the budgets.
func (s *Service) reserve(ctx context.Context, h hold, named int64) error {
	return s.changeLedger(ctx, func(l *ledger, now time.Time) error {
		if i := l.holdOf(h.Upload); i >= 0 {
			// A RETRIED INTENT whose first attempt wrote its hold and no
			// record, or whose record an early release deleted: the hold
			// becomes this intent's too, and never weaker than either's.
			// Either attempt's record may be the one written, so a direct
			// one keeps it from early release, and the later window and
			// lapse, and the more bytes, stand.
			old := l.Holds[i]
			if h.Bytes > old.Bytes {
				if err := l.admit(h.Principal, h.Bytes-old.Bytes, false, s.limits(), named, now); err != nil {
					return err
				}
			}
			l.Holds[i] = hold{Upload: old.Upload, Principal: old.Principal, Bytes: max(h.Bytes, old.Bytes),
				Until: maxTime(h.Until, old.Until), Expires: maxTime(h.Expires, old.Expires),
				Direct: old.Direct || h.Direct, Attempt: newAttempt(old.Attempt)}
			return nil
		}
		if err := l.admit(h.Principal, h.Bytes, true, s.limits(), named, now); err != nil {
			return err
		}
		h.Attempt = newAttempt(0)
		l.Holds = append(l.Holds, h)
		return nil
	})
}

// maxTime is the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// grow adds bytes to an upload's hold, or adds the hold, extending it to
// until: the reservation for the objects a publish writes itself.
func (s *Service) grow(ctx context.Context, upload, principal string, bytes int64, until time.Time, named int64) error {
	return s.changeLedger(ctx, func(l *ledger, now time.Time) error {
		i := l.holdOf(upload)
		if err := l.admit(principal, bytes, i < 0, s.limits(), named, now); err != nil {
			return err
		}
		if i < 0 {
			l.Holds = append(l.Holds, hold{Upload: upload, Principal: principal, Bytes: bytes,
				Until: until, Expires: until, Attempt: newAttempt(0)})
			return nil
		}
		l.Holds[i].Bytes += bytes
		if until.After(l.Holds[i].Until) {
			l.Holds[i].Until = until
		}
		return nil
	})
}

// release sets an upload's hold to the bytes its record still holds, as the
// record stands now, dropping the hold at zero (or where no record is left).
//
// THE RECORD IS READ AGAIN rather than taken from the caller: every byte a
// hold answers for is a file of the record, written there before it moved, so
// the record as it stands is the one account two runs sharing a reservation
// agree on, where a caller's own copy may be missing a file another run added.
//
// A release that cannot be written is logged and left to lapse: the bytes it
// would have released are named or deleted already, so a hold kept too long
// refuses a little early and never lets a byte through.
func (s *Service) release(ctx context.Context, id string) {
	ctx = context.WithoutCancel(ctx)
	var remaining int64
	r, _, err := s.readRecord(ctx, id)
	if err == nil && r != nil {
		remaining = r.heldBytes()
	}
	if err == nil {
		err = s.changeLedger(ctx, func(l *ledger, _ time.Time) error {
			if i := l.holdOf(id); i >= 0 && l.Holds[i].Bytes > remaining {
				l.Holds[i].Bytes = remaining
			}
			return nil
		})
	}
	if err != nil {
		log.WarnContext(ctx, "artifact_upload_release_failed", "upload", id, "error", err.Error())
	}
}
