package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// THE SKILL-PROMOTION LEDGER, FOR AN OPERATOR. Every record stands for its
// convergence for good, so one the pass got wrong — a draft a restriction hid
// from the org token, read as deleted — is wrong for good unless a person can
// find it and clear it. The ledger lives in the coordination store, inside the
// engine on the default topology, so both halves are routes on a node.

// rejectedRecord is a record the pass wrote for a lead's rejection.
const rejectedRecord = `{"v":1,"state":"rejected","tools":["build","tag"],"agents":3,` +
	`"backend":"confluence","name":"cut-a-release","container":"ENG",` +
	`"title":"[Auto-draft] cut-a-release","page_id":"123",` +
	`"at":"2026-03-01T09:00:00Z","rejection":"it was deleted"}`

// ledgerWith is a fleet holding the given records.
func ledgerWith(t *testing.T, recs ...coord.PromotionRecord) *coordmemory.Fleet {
	t.Helper()
	fleet := coordmemory.NewFleet()
	for _, rec := range recs {
		if _, _, err := fleet.CreatePromotion(t.Context(), rec); err != nil {
			t.Fatalf("seed %s/%s: %v", rec.Unit, rec.Fingerprint, err)
		}
	}
	return fleet
}

// THE WHOLE LEDGER IS READ, EVERY UNIT'S, with what each record says: its
// state, how a rejection was seen and when. A record filed under a unit the
// company has since renamed still holds its convergence, and a read of the
// units the company runs now would hide it.
func TestTheLedgerIsReadWholeWithWhatEachRecordSays(t *testing.T) {
	t.Parallel()
	fleet := ledgerWith(t,
		coord.PromotionRecord{Unit: "Platform", Fingerprint: "fp-1", Value: []byte(rejectedRecord)},
		coord.PromotionRecord{Unit: "Renamed Team", Fingerprint: "fp-2", Value: []byte("garbled")},
	)
	a := newApp(t, api.Options{Bootstrap: guarded(), Promotions: fleet})

	status, body := get(t, a, "/learning/promotions")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	rows, _ := body["promotions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("promotions = %v, want both units' records", body["promotions"])
	}
	first, _ := rows[0].(map[string]any)
	if first["unit"] != "Platform" || first["state"] != "rejected" ||
		first["rejection"] != "it was deleted" || first["at"] != "2026-03-01T09:00:00Z" ||
		first["page_id"] != "123" || first["version"] == nil {
		t.Errorf("the rejection reads as %v", first)
	}
	second, _ := rows[1].(map[string]any)
	if second["unit"] != "Renamed Team" || second["unreadable"] == nil || second["raw"] != "garbled" {
		t.Errorf("a record the pass cannot read reads as %v, want it shown whole "+
			"with why", second)
	}

	status, body = get(t, a, "/learning/promotions?unit=Platform")
	if rows, _ := body["promotions"].([]any); status != http.StatusOK || len(rows) != 1 {
		t.Errorf("one unit's read = %d, %v, want its one record", status, body)
	}
}

// A CLEAR DELETES THE ONE RECORD IT NAMES, and answers with what it cleared,
// so the next pass drafts that convergence as though it had never been
// drafted — and no other unit's record moves.
func TestAClearDeletesTheRecordItNamesAndSaysWhatItWas(t *testing.T) {
	t.Parallel()
	fleet := ledgerWith(t,
		coord.PromotionRecord{Unit: "Platform", Fingerprint: "fp-1", Value: []byte(rejectedRecord)},
		coord.PromotionRecord{Unit: "Platform", Fingerprint: "fp-2", Value: []byte(rejectedRecord)},
	)
	a := newApp(t, api.Options{Bootstrap: guarded(), Promotions: fleet})

	status, body := post(t, a, "/learning/promotions/clear?unit=Platform&fingerprint=fp-1", "t0ken")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	cleared, _ := body["cleared"].(map[string]any)
	if cleared["fingerprint"] != "fp-1" || cleared["state"] != "rejected" {
		t.Errorf("the answer names %v, want the record cleared", cleared)
	}
	held, err := fleet.Promotions(t.Context(), "Platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Fingerprint != "fp-2" {
		t.Errorf("the unit holds %+v, want only the record not named", held)
	}
}

// A CLEAR THAT CANNOT SAY WHAT IT DELETED DELETES NOTHING: an address that is
// incomplete or names no record is refused, and a record that moved between
// the read and the delete — to a lead's rejection, say — is refused rather
// than deleted unseen.
func TestAClearThatCannotSayWhatItDeletedDeletesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		path   string
		ledger func(*testing.T) promotionsLedger
		status int
	}{
		{"no fingerprint", "/learning/promotions/clear?unit=Platform",
			func(t *testing.T) promotionsLedger { return seeded(t) }, http.StatusBadRequest},
		{"no such record", "/learning/promotions/clear?unit=Platform&fingerprint=fp-9",
			func(t *testing.T) promotionsLedger { return seeded(t) }, http.StatusNotFound},
		{"moved while it was cleared", "/learning/promotions/clear?unit=Platform&fingerprint=fp-1",
			func(t *testing.T) promotionsLedger { return movingLedger{seeded(t)} }, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ledger := tc.ledger(t)
			a := newApp(t, api.Options{Bootstrap: guarded(), Promotions: ledger})
			status, body := post(t, a, tc.path, "t0ken")
			if status != tc.status || body["error"] == nil {
				t.Fatalf("status = %d, body = %v, want %d with a code", status, body, tc.status)
			}
			if held, _ := ledger.Promotions(t.Context(), "Platform"); len(held) != 1 {
				t.Errorf("a refused clear left %+v, want the record untouched", held)
			}
		})
	}
}

// A CLEAR IS A WRITE, whatever the read posture opens: undoing a lead's
// recorded rejection is not a read, while the ledger itself reads as every
// other route on the read surface does.
func TestAClearIsRefusedWithoutAToken(t *testing.T) {
	t.Parallel()
	fleet := seeded(t)
	a := newApp(t, api.Options{Bootstrap: guarded(), Promotions: fleet})
	if status, _ := post(t, a, "/learning/promotions/clear?unit=Platform&fingerprint=fp-1", ""); status == http.StatusOK {
		t.Fatal("an unauthenticated caller cleared a promotion record")
	}
	if held, _ := fleet.Promotions(t.Context(), "Platform"); len(held) != 1 {
		t.Errorf("a refused clear still deleted: %+v", held)
	}
	if status, _ := get(t, a, "/learning/promotions"); status != http.StatusOK {
		t.Errorf("the ledger answered %d to an anonymous read the posture opens", status)
	}
}

// promotionsLedger is what a test hands the app as its ledger.
type promotionsLedger interface {
	AllPromotions(ctx context.Context) ([]coord.PromotionRecord, error)
	Promotions(ctx context.Context, unit string) ([]coord.PromotionRecord, error)
	DeletePromotion(ctx context.Context, unit, fingerprint string, version uint64) (bool, error)
}

// seeded is a ledger holding Platform's one rejected record.
func seeded(t *testing.T) *coordmemory.Fleet {
	t.Helper()
	return ledgerWith(t,
		coord.PromotionRecord{Unit: "Platform", Fingerprint: "fp-1", Value: []byte(rejectedRecord)})
}

// movingLedger answers every delete as a lost race: the record moved after it
// was read.
type movingLedger struct{ *coordmemory.Fleet }

func (movingLedger) DeletePromotion(context.Context, string, string, uint64) (bool, error) {
	return false, nil
}
