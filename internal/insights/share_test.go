package insights_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/insights"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/usage"
)

// relay is a fake relay that keeps every body it was sent.
type relay struct {
	mu     sync.Mutex
	bodies [][]byte
	fail   bool
	// refuse is the status a failing relay answers, 0 for none at all.
	refuse int
}

func (r *relay) send(_ context.Context, body []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return r.refuse, errors.New("the relay did not take it")
	}
	r.bodies = append(r.bodies, append([]byte(nil), body...))
	return 202, nil
}

func (r *relay) sent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// harness is one node's sharer on a shared store, at a clock the test moves.
type harness struct {
	store      coord.InsightRecords
	relay      *relay
	now        time.Time
	consent    config.InsightsSharing
	consentErr error
}

func (h *harness) sharer() *insights.Sharer { return h.sharerOn(tagged) }

// sharerOn is a node's sharer on a build of origin.
func (h *harness) sharerOn(origin issuereport.Origin) *insights.Sharer {
	noops := []usage.TurnLedgerRow{started("app", "chat:message", "nothing", 12, 0, 0),
		started("app", "chat:message", "moved", 18, 0, 0)}
	return &insights.Sharer{
		Store: h.store, Origin: origin,
		Findings: func(context.Context) ([]advisor.Finding, error) {
			return advisor.Detect(advisor.Evidence{Ledger: noops}, kindOf), nil
		},
		Consent: func(context.Context) (config.InsightsSharing, error) {
			if h.consentErr != nil {
				return config.InsightsSharing{}, h.consentErr
			}
			return h.consent, nil
		},
		Day:   func(at time.Time) string { return at.UTC().Format("2006-01-02") },
		Delay: func() time.Duration { return 6 * time.Hour },
		Send:  h.relay.send,
		Now:   func() time.Time { return h.now },
	}
}

func newHarness(share config.InsightsShare) *harness {
	return &harness{store: memory.NewFleet(), relay: &relay{}, now: monday,
		consent: config.InsightsSharing{Share: share, Accepted: insights.SchemaVersion}}
}

// pass is one evaluation and one send pass at the harness's clock.
func (h *harness) pass(t *testing.T, s *insights.Sharer) {
	t.Helper()
	if err := s.Evaluate(t.Context()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if err := s.SendDue(t.Context()); err != nil {
		t.Fatalf("SendDue: %v", err)
	}
}

func (h *harness) record(t *testing.T) (coord.InsightRecord, bool) {
	t.Helper()
	r, found, err := h.store.InsightRecord(t.Context(), string(advisor.ClassNoopShare), "2030-W02")
	if err != nil {
		t.Fatal(err)
	}
	return r, found
}

// IN REVIEW MODE NOTHING LEAVES UNTIL AN ADMINISTRATOR SENDS IT: a class raised
// two days running waits, however long; Send schedules it at its own delay, and
// then exactly one record of the catalogue's shape goes.
func TestReviewModeSendsNothingUntilSend(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsReview)
	s := h.sharer()
	h.pass(t, s)
	h.now = h.now.Add(24 * time.Hour)
	h.pass(t, s)
	if r, _ := h.record(t); r.Status != coord.InsightQueued {
		t.Fatalf("after two days the record is %s, want queued", r.Status)
	}
	h.now = h.now.Add(4 * 24 * time.Hour)
	h.pass(t, s)
	if h.relay.sent() != 0 {
		t.Fatalf("review mode sent %d records before anybody pressed Send", h.relay.sent())
	}
	if _, err := s.Decide(t.Context(), string(advisor.ClassNoopShare), "2030-W02", true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	h.pass(t, s)
	if h.relay.sent() != 0 {
		t.Fatal("a record left before its delay")
	}
	h.now = h.now.Add(6 * time.Hour)
	h.pass(t, s)
	h.pass(t, s)
	if h.relay.sent() != 1 {
		t.Fatalf("after Send and its delay %d records left, want 1", h.relay.sent())
	}
	var sent map[string]any
	if err := json.Unmarshal(h.relay.bodies[0], &sent); err != nil || sent["class"] != "wake.noop_share" {
		t.Errorf("the relay was sent %s (%v)", h.relay.bodies[0], err)
	}
	if r, _ := h.record(t); r.Status != coord.InsightSent {
		t.Errorf("the record is %s, want sent", r.Status)
	}
}

// A DECLINED RECORD NEVER LEAVES, and a class seen on one day only, or on two
// days with one between them, is never eligible.
func TestADeclinedOrUnrepeatedRecordNeverLeaves(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsReview)
	s := h.sharer()
	h.pass(t, s)
	h.now = h.now.Add(48 * time.Hour)
	h.pass(t, s)
	if r, _ := h.record(t); r.Status != coord.InsightObserved {
		t.Fatalf("a class seen two days apart is %s, want observed", r.Status)
	}
	h.now = h.now.Add(24 * time.Hour)
	h.pass(t, s)
	if _, err := s.Decide(t.Context(), string(advisor.ClassNoopShare), "2030-W02", false); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	h.now = h.now.Add(48 * time.Hour)
	h.pass(t, s)
	if r, _ := h.record(t); r.Status != coord.InsightDeclined || h.relay.sent() != 0 {
		t.Errorf("a declined record is %s and %d were sent", r.Status, h.relay.sent())
	}
}

// OFF MEANS OFF, ACROSS A RESTART: turning sharing off forgets every record that
// waits, and a sharer started afresh on the same store sends nothing and keeps
// nothing until sharing is on again, when it starts over.
func TestOffMeansOffAcrossARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	s := h.sharer()
	h.pass(t, s)
	h.now = h.now.Add(24 * time.Hour)
	h.pass(t, s)
	if r, _ := h.record(t); r.Status != coord.InsightScheduled {
		t.Fatalf("auto mode left the record %s, want scheduled", r.Status)
	}
	h.consent.Share = config.InsightsOff
	h.now = h.now.Add(7 * time.Hour)
	if err := s.SendDue(t.Context()); err != nil || h.relay.sent() != 0 {
		t.Fatalf("a send pass after sharing went off sent %d (%v)", h.relay.sent(), err)
	}
	h.pass(t, s)
	restarted := h.sharer()
	h.now = h.now.Add(48 * time.Hour)
	h.pass(t, restarted)
	if all, _ := h.store.ListInsightRecords(t.Context()); len(all) != 0 || h.relay.sent() != 0 {
		t.Fatalf("off kept %+v and sent %d", all, h.relay.sent())
	}
	h.consent.Share = config.InsightsAuto
	h.pass(t, restarted)
	if r, _ := h.record(t); r.Status != coord.InsightObserved {
		t.Errorf("turned on again, the record is %s, want observed from scratch", r.Status)
	}
}

// A CONSENT THAT CANNOT BE READ IS NEVER TAKEN FOR OFF: the pass stops and
// forgets nothing, and the queue stands when the consent reads again.
func TestAnUnreadConsentForgetsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsReview)
	s := h.sharer()
	h.pass(t, s)
	h.consentErr = errors.New("no data node answered")
	if err := s.Evaluate(t.Context()); err == nil {
		t.Fatal("Evaluate passed on a consent it could not read")
	}
	if _, found := h.record(t); !found {
		t.Error("an unread consent forgot the record")
	}
}

// A SCHEMA NOBODY ACCEPTED SHARES NOTHING: a build whose catalogue is newer than
// the consent keeps and sends nothing, in any mode.
func TestAnUnacceptedSchemaSharesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	h.consent.Accepted = insights.SchemaVersion - 1
	s := h.sharer()
	for range 3 {
		h.pass(t, s)
		h.now = h.now.Add(24 * time.Hour)
	}
	if all, _ := h.store.ListInsightRecords(t.Context()); len(all) != 0 || h.relay.sent() != 0 {
		t.Errorf("an unaccepted schema kept %+v and sent %d", all, h.relay.sent())
	}
}

// ONE CLASS'S WEEK LEAVES ONCE, HOWEVER MANY NODES: two sharers on one store in
// auto mode, each passing at once, send one record.
func TestOneClassesWeekLeavesOnceAcrossNodes(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	a, b := h.sharer(), h.sharer()
	for day := range 2 {
		h.now = monday.Add(time.Duration(day) * 24 * time.Hour)
		h.pass(t, a)
		h.pass(t, b)
	}
	h.now = h.now.Add(7 * time.Hour)
	var wg sync.WaitGroup
	for _, s := range []*insights.Sharer{a, b, a, b} {
		wg.Go(func() {
			if err := s.SendDue(context.WithoutCancel(t.Context())); err != nil {
				t.Errorf("SendDue: %v", err)
			}
		})
	}
	wg.Wait()
	if h.relay.sent() != 1 {
		t.Errorf("%d records left, want 1", h.relay.sent())
	}
}

// AN ALWAYS-SENT CLASS SKIPS REVIEW, and a class the administrator did not name
// still waits.
func TestAnAlwaysSentClassSkipsReview(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsReview)
	h.consent.Always = []string{string(advisor.ClassNoopShare)}
	s := h.sharer()
	h.pass(t, s)
	h.now = h.now.Add(24 * time.Hour)
	h.pass(t, s)
	h.now = h.now.Add(6 * time.Hour)
	h.pass(t, s)
	if h.relay.sent() != 1 {
		t.Errorf("an always-sent class left %d records, want 1", h.relay.sent())
	}
}

// ONLY A SEALED RECORD LEAVES, WHATEVER THE STORE HOLDS: a scheduled payload with
// a field or a value outside the catalogue is never sent, and fails for good.
func TestAStoredPayloadOutsideTheCatalogueNeverLeaves(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	forged := `{"schema":1,"class":"wake.noop_share","week":"2030-W02","version":"main",` +
		`"facts":{"share":"75+","seats":"4+"},"seat":"payroll-agent"}`
	if _, _, err := h.store.ClaimInsightRecord(t.Context(), coord.InsightRecord{Class: "wake.noop_share",
		Week: "2030-W02", Status: coord.InsightScheduled, SendAt: monday, Payload: []byte(forged)},
		func(coord.InsightRecord) bool { return true }); err != nil {
		t.Fatal(err)
	}
	s := h.sharer()
	for range insights.MaxSendAttempts {
		h.now = h.now.Add(7 * time.Hour)
		if err := s.SendDue(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := h.record(t); h.relay.sent() != 0 || r.Status != coord.InsightFailed {
		t.Errorf("a forged payload: %d sent, record %s; want none sent and failed", h.relay.sent(), r.Status)
	}
}

// A SEND THE RELAY DID NOT TAKE IS TRIED AGAIN AT A NEW DELAY, and left failed
// after its last attempt.
func TestAFailedSendIsTriedAgainThenLeftFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	h.relay.fail, h.relay.refuse = true, 422
	s := h.sharer()
	h.pass(t, s)
	h.now = h.now.Add(24 * time.Hour)
	h.pass(t, s)
	for attempt := 1; attempt <= insights.MaxSendAttempts; attempt++ {
		h.now = h.now.Add(6 * time.Hour)
		h.pass(t, s)
		r, _ := h.record(t)
		want := coord.InsightScheduled
		if attempt == insights.MaxSendAttempts {
			want = coord.InsightFailed
		}
		if r.Status != want || r.Attempts != attempt || r.Answer != 422 {
			t.Fatalf("after attempt %d the record is %s with %d attempts and answer %d, want %s and 422",
				attempt, r.Status, r.Attempts, r.Answer, want)
		}
	}
}

// ONLY A PUBLIC BUILD SENDS, WHATEVER THE STORE HOLDS: a development build's node
// sends nothing while a public node's record waits for its time, and the public
// node sends it.
func TestAnInertNodeSendsNothingAPublicNodeScheduled(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	public, inert := h.sharer(), h.sharerOn(development)
	h.pass(t, public)
	h.now = h.now.Add(24 * time.Hour)
	h.pass(t, public)
	h.now = h.now.Add(7 * time.Hour)
	if err := inert.SendDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.record(t); h.relay.sent() != 0 || r.Status != coord.InsightScheduled {
		t.Fatalf("a development build's node sent %d and left the record %s", h.relay.sent(), r.Status)
	}
	if err := public.SendDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.record(t); h.relay.sent() != 1 || r.Status != coord.InsightSent || r.Answer != 202 {
		t.Errorf("the public node sent %d, record %s with answer %d", h.relay.sent(), r.Status, r.Answer)
	}
}

// A SEND THAT NEVER SETTLED READS AS FAILED after an hour, and off clears it; a
// send still under way is left to settle.
func TestAStaleSendReadsAsFailedAndOffClearsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(config.InsightsAuto)
	claim := func(class string, claimedAt time.Time) {
		t.Helper()
		if _, _, err := h.store.ClaimInsightRecord(t.Context(), coord.InsightRecord{Class: class, Week: "2030-W02",
			Status: coord.InsightSending, ClaimedAt: claimedAt}, func(coord.InsightRecord) bool { return true }); err != nil {
			t.Fatal(err)
		}
	}
	claim("wake.noop_share", h.now.Add(-2*time.Hour))
	claim("review.idle", h.now.Add(-time.Minute))
	if err := h.sharer().SendDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	stale, _ := h.record(t)
	fresh, _, _ := h.store.InsightRecord(t.Context(), "review.idle", "2030-W02")
	if stale.Status != coord.InsightFailed || fresh.Status != coord.InsightSending {
		t.Fatalf("after a send pass the stale claim is %s and the fresh one %s, want failed and sending", stale.Status, fresh.Status)
	}
	claim("ask.overtaken", h.now.Add(-2*time.Hour))
	h.consent.Share = config.InsightsOff
	h.pass(t, h.sharer())
	if _, found, _ := h.store.InsightRecord(t.Context(), "ask.overtaken", "2030-W02"); found {
		t.Error("off left a stale send standing")
	}
	if _, found, _ := h.store.InsightRecord(t.Context(), "review.idle", "2030-W02"); !found {
		t.Error("off removed a send still under way")
	}
}
