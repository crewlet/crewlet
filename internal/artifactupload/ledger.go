package artifactupload

import (
	"context"
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
// record, so a reservation that checks the company's total and adds to it is
// one compare-and-set and two intents racing for the last of a budget cannot
// both win.
//
// # What a hold answers for
//
// A hold is the bytes one upload may have put in the object store that no
// record names: every file it minted a key for and has not yet seen named or
// deleted. It lasts until the object collector could have deleted those bytes
// ([holdTail] past the last instant one of them can be stored), so the bytes
// nothing names never exceed the holds, where they used to be bounded only by
// the collector's day. It is released early, never late: a release follows the
// record naming the bytes or the engine deleting them, and a hold the engine
// could not release lapses on its own.

// ledgerVersion is the ledger's format.
const ledgerVersion = 1

// hold is one upload's reservation.
type hold struct {
	Upload    string `json:"upload"`
	Principal string `json:"principal"`
	Bytes     int64  `json:"bytes"`

	// Until is when the hold lapses on its own.
	Until time.Time `json:"until"`

	// Expires is the upload's transfer window, past which an abandoned
	// upload the engine streamed is released early.
	Expires time.Time `json:"expires"`

	// Direct reports a client putting bytes straight to the bucket, whose
	// puts the engine never sees: such a hold is never released early.
	Direct bool `json:"direct,omitempty"`
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

// earlyReleases bounds how many abandoned uploads one ledger change releases,
// so the change that happens to find them pays a bounded cost.
const earlyReleases = 4

// changeLedger applies change to the current ledger and writes it, reading
// again on a lost race. Before each round it releases what has lapsed or been
// abandoned. change may refuse, which writes nothing.
func (s *Service) changeLedger(ctx context.Context, change func(l *ledger, now time.Time) error) error {
	// RELEASED ACROSS ROUNDS: an upload released in a round that then lost
	// its write has no record left to find in the next.
	released := map[string]bool{}
	for range ledgerAttempts {
		raw, rev, err := s.res.UploadLedger(ctx)
		if err != nil {
			return fmt.Errorf("artifactupload: read the reservation ledger: %w", err)
		}
		var l ledger
		if raw != nil {
			if err := json.Unmarshal(raw, &l); err != nil {
				return fmt.Errorf("artifactupload: the reservation ledger is not one this build reads: %w", err)
			}
			if l.V > ledgerVersion {
				return fmt.Errorf("artifactupload: the reservation ledger was written by a newer build "+
					"(format %d)", l.V)
			}
		}
		now := s.now()
		s.releaseAbandoned(ctx, &l, now, released)
		l.Holds = slices.DeleteFunc(l.Holds, func(h hold) bool {
			return !h.Until.After(now) || h.Bytes <= 0 || released[h.Upload]
		})
		if err := change(&l, now); err != nil {
			return err
		}
		l.Holds = slices.DeleteFunc(l.Holds, func(h hold) bool { return h.Bytes <= 0 })
		l.V = ledgerVersion
		encoded, err := json.Marshal(l)
		if err != nil {
			return fmt.Errorf("artifactupload: encode the reservation ledger: %w", err)
		}
		wrote, err := s.res.SwapUploadLedger(ctx, encoded, rev)
		if err != nil {
			return fmt.Errorf("artifactupload: write the reservation ledger: %w", err)
		}
		if wrote != 0 {
			s.gauge(metrics.ArtifactsUploadReservedBytes, float64(l.totals("", now).companyBytes))
			return nil
		}
	}
	return errLostLedger
}

// releaseAbandoned releases, ahead of their lapse, the holds of uploads whose
// transfer window has closed with no publish having claimed them, where the
// engine knows every put: it deletes the upload's record first (so no transfer
// or finalize can take it after), then every object it stored, and adds the
// uploads whose holds may go to released. A hold whose objects could not all
// be deleted is kept, and lapses on its own.
func (s *Service) releaseAbandoned(ctx context.Context, l *ledger, now time.Time, released map[string]bool) {
	for _, h := range l.Holds {
		if len(released) >= earlyReleases {
			return
		}
		if released[h.Upload] || h.Direct || !h.live(now) || h.Expires.After(now) {
			continue
		}
		r, rev, err := s.readRecord(ctx, h.Upload)
		if err != nil || r == nil || r.State != stateOpen {
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
			released[h.Upload] = true
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
		if l.holdOf(h.Upload) >= 0 {
			// A RETRIED INTENT whose first attempt wrote its hold and lost
			// its answer: the hold is the one it would write.
			return nil
		}
		if err := l.admit(h.Principal, h.Bytes, true, s.limits(), named, now); err != nil {
			return err
		}
		l.Holds = append(l.Holds, h)
		return nil
	})
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
				Until: until, Expires: until})
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
