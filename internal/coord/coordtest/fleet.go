package coordtest

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// RunFleet is the contract suite every [coord.Fleet] must pass.
//
// A SECOND entry point rather than more cases in [Run]: the memory twin has a
// lease backend and no shared state, so a case needing both would make its
// certification depend on the KV.
//
// newFleet makes the fresh store one case runs against and answers how a NODE
// opens a handle on it. Cases about what concurrent writers must agree on
// spread them across several handles ([fleetHarness.nodes]): the KV store
// sends one handle's concurrent read-modify-writes of a key as one
// compare-and-swap, so a race through one handle certifies nothing about how
// the store arbitrates between nodes. A store that is its handle repeats it.
func RunFleet(t *testing.T, newFleet func(t *testing.T) (node func() coord.Fleet)) {
	t.Helper()
	groups := []struct {
		name  string
		cases []fleetCase
	}{
		{"valve", valveCases},
		{"claims", claimCases},
		{"ledger", ledgerCases},
		{"budgets", budgetCases},
		{"plane", planeCases},
		{"payload", payloadCases},
		{"payload_race", payloadRaceCases},
		{"body_sweep", bodySweepCases},
		{"channels", channelCases},
		{"follows", followCases},
		{"chat_reads", chatReadCases},
		{"chat_read_watch", chatReadWatchCases},
		{"home_boards", homeBoardCases},
		{"assistant_instructions", instructionsCases},
		{"fires", fireCases},
		{"custody", custodyCases},
		{"rebases", rebaseCases},
		{"sandbox_runs", runCases},
		{"sandbox_run_parts", runPartCases},
		{"session_claims", sessionClaimCases},
		{"secrets", secretCases},
		{"integrations", integrationCases},
		{"atlas_sources", atlasSourceCases},
		{"atlas_code_scans", atlasCodeScanCases},
		{"atlas_source_refreshes", atlasSourceRefreshCases},
		{"seat_access", seatAccessCases},
		{"grant_requests", grantRequestCases},
		{"access_reviews", accessReviewCases},
		{"atlas_wakes", atlasWakeCases},
		{"conversation_holds", conversationHoldCases},
		{"seen_through", seenThroughCases},
		{"box_readings", boxReadingCases},
		{"mailboxes", mailboxCases},
		{"object stores", objectStoreCases},
		{"trash reaps", trashReapCases},
		{"upload reservations", uploadReservationCases},
		{"save allowances", saveAllowanceCases},
		{"seat_pauses", seatPauseCases},
		{"model_retries", modelRetryCases},
		{"device_sign_ins", deviceSignInCases},
		{"model_checks", modelCheckCases},
		{"mcp_listings", mcpListingCases},
		{"mcp_restarts", mcpRestartCases},
		{"mcp_checks", mcpCheckCases},
		{"ssh_tests", sshTestCases},
		{"issue_reports", issueReportCases},
		{"insight_records", insightRecordCases},
		{coord.MCPGrantClass, oauthGrantCases(coord.MCPGrantFamily)},
		{coord.IntegrationGrantClass, oauthGrantCases(coord.IntegrationGrantFamily)},
		{"google_identities", googleIdentityCases},
		{"mcp_accepted", mcpAcceptedCases},
		{"maintenance", maintenanceCases},
		{"retirements", retirementCases},
		{"backups", backupCases},
		{"feed_marks", feedMarkCases},
		{"listings", listingCases},
		{"markers", markerCases},
		{"floors", floorCases},
		{"values", valueCases},
		{"positions_values", positionsValueCases},
		{"positions_widest", widestPositionsCases},
		{"uploads_values", uploadsValueCases},
		{"chat_reads_values", chatReadsValueCases},
		{"secrets_values", secretsValueCases},
		{"atlas_values", atlasValueCases},
		{"integrations_values", integrationsValueCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					node := newFleet(t)
					f := node()
					if f == nil {
						t.Fatal("newFleet opened a nil backend")
					}
					// NOT t.Context(), for the reason [newHarness]
					// states: that one is cancelled before cleanup
					// runs, and the concurrency cases join goroutines
					// that are still mid-call at that point.
					ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
					t.Cleanup(cancel)
					c.fn(&fleetHarness{t: t, ctx: ctx, f: f, node: node})
				})
			}
		})
	}
}

type fleetCase struct {
	name string
	fn   func(h *fleetHarness)
}

// anOrigin is the revision origin every activation in the suite carries but
// the cases about the origin itself: a backend refuses an activation without
// one, so a case about something else states one and moves on.
var anOrigin = coord.RevisionOrigin{
	Author: "suite", AuthorKind: iam.ActorOperator, Source: "api",
	CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// fleetHarness is a backend plus the assertions the cases are written in.
type fleetHarness struct {
	t   *testing.T
	ctx context.Context
	f   coord.Fleet

	// node opens another handle on f's store, as another node would.
	node func() coord.Fleet
}

// nodes is n handles on the case's store, each a node of its own: f, and n-1
// opened beside it.
func (h *fleetHarness) nodes(n int) []coord.Fleet {
	h.t.Helper()
	out := []coord.Fleet{h.f}
	for range n - 1 {
		peer := h.node()
		if peer == nil {
			h.t.Fatal("newFleet opened a nil peer")
		}
		out = append(out, peer)
	}
	return out
}

// now is the suite's base instant.
//
// FIXED and in the past, not time.Now(): every case that reasons about a
// window boundary or a lapsed deadline has to be able to name both sides of
// it, and a case anchored to the wall clock passes or fails depending on
// where in a second it happened to start.
func (h *fleetHarness) now() time.Time {
	return time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
}

func (h *fleetHarness) allow(bucket string, limit int, window time.Duration, at time.Time) bool {
	h.t.Helper()
	ok, err := h.f.Allow(h.ctx, bucket, limit, window, at)
	if err != nil {
		h.t.Fatalf("Allow(%s): %v", bucket, err)
	}
	return ok
}

func (h *fleetHarness) claim(key string, ttl time.Duration, at time.Time) bool {
	h.t.Helper()
	ok, err := h.f.Claim(h.ctx, key, ttl, at)
	if err != nil {
		h.t.Fatalf("Claim(%s): %v", key, err)
	}
	return ok
}

// report hands the errors a shared check found to the test.
//
// The checks are functions returning errors rather than case bodies for the
// reason fleetchecks.go gives: a body that can only fail a
// *testing.T is a body nothing in this package can prove fails at all.
func (h *fleetHarness) report(errs []error) {
	h.t.Helper()
	for _, err := range errs {
		h.t.Error(err)
	}
}

func (h *fleetHarness) worked(scope string, keys ...string) map[string]bool {
	h.t.Helper()
	got, err := h.f.Worked(h.ctx, scope, keys)
	if err != nil {
		h.t.Fatalf("Worked(%s): %v", scope, err)
	}
	return got
}

func (h *fleetHarness) workedAt(scope string, keys ...string) map[string]time.Time {
	h.t.Helper()
	got, err := h.f.WorkedAt(h.ctx, scope, keys)
	if err != nil {
		h.t.Fatalf("WorkedAt(%s): %v", scope, err)
	}
	return got
}

func (h *fleetHarness) workedBy(scope string, keys ...string) map[string]string {
	h.t.Helper()
	got, err := h.f.WorkedBy(h.ctx, scope, keys)
	if err != nil {
		h.t.Fatalf("WorkedBy(%s): %v", scope, err)
	}
	return got
}

func (h *fleetHarness) record(scope, key, detail string, at time.Time) {
	h.t.Helper()
	if err := h.f.Record(h.ctx, scope, key, detail, at); err != nil {
		h.t.Fatalf("Record(%s/%s): %v", scope, key, err)
	}
}
