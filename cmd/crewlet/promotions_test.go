package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/learning"
)

// `crewlet promotions` reads and clears the fleet's skill-promotion ledger
// through a running node, whose routes answer with internal/learning's own
// rendering of each record. The fake here answers with that same rendering.

// ledgerNode is a node answering the two ledger routes.
type ledgerNode struct {
	server  *httptest.Server
	records []coord.PromotionRecord
	cleared string // unit/fingerprint the last clear named
	token   string
}

func newLedgerNode(t *testing.T, records ...coord.PromotionRecord) *ledgerNode {
	t.Helper()
	n := &ledgerNode{records: records}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /learning/promotions", func(w http.ResponseWriter, r *http.Request) {
		held := n.records
		if unit := r.URL.Query().Get("unit"); unit != "" {
			held = nil
			for _, rec := range n.records {
				if rec.Unit == unit {
					held = append(held, rec)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"promotions": learning.PromotionReports(held),
		})
	})
	mux.HandleFunc("POST /learning/promotions/clear", func(w http.ResponseWriter, r *http.Request) {
		n.token = r.Header.Get("Authorization")
		unit, fingerprint := r.URL.Query().Get("unit"), r.URL.Query().Get("fingerprint")
		n.cleared = unit + "/" + fingerprint
		w.Header().Set("Content-Type", "application/json")
		for _, rec := range n.records {
			if rec.Unit == unit && rec.Fingerprint == fingerprint {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"cleared": learning.PromotionReports([]coord.PromotionRecord{rec})[0],
				})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"promotion_not_found","detail":"no such record"}`))
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// longRejection is a rejection too long for any column, which the listing
// must print whole.
var longRejection = "it was deleted " + strings.Repeat("and stayed deleted ", 20) + "end"

func ledgerRecord(unit, fingerprint string) coord.PromotionRecord {
	value := `{"v":1,"state":"rejected","tools":["build","tag"],"agents":3,` +
		`"backend":"confluence","name":"cut-a-release","container":"ENG",` +
		`"title":"[Auto-draft] cut-a-release","page_id":"123",` +
		`"at":"2026-03-01T09:00:00Z","rejection":"` + longRejection + `"}`
	return coord.PromotionRecord{Unit: unit, Fingerprint: fingerprint, Value: []byte(value), Version: 4}
}

// THE LISTING SAYS WHAT EACH RECORD IS, WHOLE: its address, which a clear
// names it by, its state and when, and the prose — a title, how a rejection
// was seen, why a record cannot be read — on lines of its own rather than cut
// to a column.
func TestPromotionsListPrintsEveryRecordWhole(t *testing.T) {
	node := newLedgerNode(t, ledgerRecord("Platform", "fp-1"),
		coord.PromotionRecord{Unit: "Old Team", Fingerprint: "fp-2", Value: []byte("garbled")})
	cfg := bootstrapForURL(t, node.server.URL)

	out, _, err := cli(t, "promotions", "list", "-config", cfg)
	if err != nil {
		t.Fatalf("promotions list: %v", err)
	}
	for _, want := range []string{
		"Platform  fp-1  rejected  2026-03-01T09:00:00Z",
		"title:      [Auto-draft] cut-a-release",
		"page:       123 in ENG (confluence)",
		"rejection:  " + longRejection,
		"Old Team  fp-2  unreadable",
		"raw:        garbled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not say %q:\n%s", want, out)
		}
	}
}

// A CLEAR NAMES THE RECORD TO THE NODE AND SAYS WHAT WAS CLEARED — and it is
// a guarded write, so the token goes with it.
func TestPromotionsClearNamesTheRecordAndWhatWasCleared(t *testing.T) {
	node := newLedgerNode(t, ledgerRecord("Site Reliability", "fp-1"))
	cfg := bootstrapForURL(t, node.server.URL)

	out, _, err := cli(t, "promotions", "clear", "-config", cfg,
		"-unit", "Site Reliability", "-fingerprint", "fp-1")
	if err != nil {
		t.Fatalf("promotions clear: %v", err)
	}
	if node.cleared != "Site Reliability/fp-1" {
		t.Errorf("the node was asked to clear %q, want the record named", node.cleared)
	}
	if node.token != "Bearer t0ken" {
		t.Errorf("the clear carried %q, want the configured token", node.token)
	}
	if !strings.Contains(out, "Site Reliability  fp-1  rejected") {
		t.Errorf("the clear does not say what it cleared:\n%s", out)
	}

	if _, _, err := cli(t, "promotions", "clear", "-config", cfg, "-unit", "Platform"); err == nil {
		t.Error("a clear with no fingerprint was sent")
	}
	if _, _, err := cli(t, "promotions", "clear", "-config", cfg,
		"-unit", "Platform", "-fingerprint", "fp-9"); err == nil ||
		!strings.Contains(err.Error(), "promotion_not_found") {
		t.Errorf("a clear the node refused = %v, want its refusal", err)
	}
}
