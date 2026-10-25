package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A PROPOSAL EXECUTES ONCE, however many approvals race for it: two tabs on
// two nodes approving at the same instant send one write, and the loser is
// told the proposal was already decided. Mutation: drop the arbitrated
// approve record (send before or without it) and two writes are sent.
func TestAProposalExecutesOnce(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	nodes := []*approval.Approver{f.node(t), f.node(t)}
	ctx := as(t.Context(), ada)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = nodes[i%2].Approve(ctx, approve(p))
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, approval.ErrAlreadyDecided):
			t.Errorf("a losing approval answered %v, want already decided", err)
		}
	}
	if won != 1 || f.api.sends() != 1 {
		t.Fatalf("%d approvals won and %d writes were sent; want one of each", won, f.api.sends())
	}
	if got := f.store.state(p.ID); got != wire.ProposalApplied {
		t.Errorf("the proposal is %s, want applied", got)
	}
}

// A RETRIED PROPOSAL IS THE SAME WRITE. An answer lost on the way back reads
// as unknown and is never retried by itself; the person's Retry sends the
// proposal's own operation key again, so the ledger answers it with the
// first write. Mutation: mint a key per send and the handler lands two.
func TestARetriedProposalIsTheSameWrite(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	node := f.node(t)
	ctx := as(t.Context(), ada)

	f.api.err = errors.New("the connection dropped")
	got, err := node.Approve(ctx, approve(p))
	if err != nil || got.Proposal.State != wire.ProposalUnknown {
		t.Fatalf("a lost answer approved as %v, %v; want unknown", got.Proposal.State, err)
	}
	if len(f.turns.resumed) != 0 {
		t.Error("an unknown outcome resumed the turn: it waits for the person's Retry")
	}
	got, err = node.Approve(ctx, approve(p))
	if err != nil || got.Proposal.State != wire.ProposalApplied {
		t.Fatalf("the Retry answered %v, %v; want applied", got.Proposal.State, err)
	}
	if f.api.sends() != 2 {
		t.Fatalf("%d sends, want the first and the Retry", f.api.sends())
	}
	for i, sent := range f.api.sent {
		if sent.IdempotencyKey != p.OpKey {
			t.Errorf("send %d carried key %q, want the proposal's own %q", i, sent.IdempotencyKey, p.OpKey)
		}
	}
	if n := f.api.writes[p.OpKey]; n != 1 {
		t.Errorf("%d writes landed under the proposal's key, want one", n)
	}
}

// AN UNKNOWN OUTCOME IS DECIDED ON READ: an approval whose node died before
// it settled is offered a Retry once it is past UnknownAfter, and not before.
func TestAnUnsettledApprovalIsOfferedARetryOnceItCannotStillLand(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	p.State, p.DecidedAt = wire.ProposalExecuting, noon
	f.store.proposals[p.ID] = p
	ctx := as(t.Context(), ada)
	if _, err := f.node(t).Approve(ctx, approve(p)); !errors.Is(err, approval.ErrAlreadyDecided) {
		t.Fatalf("an executing proposal inside its grant was approved again: %v", err)
	}
	f.clock = noon.Add(proposal.UnknownAfter)
	got, err := f.node(t).Approve(ctx, approve(p))
	if err != nil || got.Proposal.State != wire.ProposalApplied || f.api.sends() != 1 {
		t.Fatalf("the Retry of an unsettled write answered %v, %v after %d sends",
			got.Proposal.State, err, f.api.sends())
	}
}

// A RETRIED APPROVAL REQUEST IS THE SAME APPROVAL: the same key sent again
// after its answer was lost answers the proposal's state and sends nothing.
func TestARetriedApprovalRequestSendsNothingTwice(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	node, ctx, ap := f.node(t), as(t.Context(), ada), approve(p)
	if _, err := node.Approve(ctx, ap); err != nil {
		t.Fatal(err)
	}
	got, err := node.Approve(ctx, ap)
	if err != nil || got.Proposal.State != wire.ProposalApplied || f.api.sends() != 1 {
		t.Errorf("the same approval again answered %v, %v after %d sends",
			got.Proposal.State, err, f.api.sends())
	}
}

// AN APPROVAL IS NEVER ANOTHER PERSON'S: somebody else's proposal does not
// exist for them, and nothing is recorded or sent. Mutation: read the
// proposal by id alone and the second person executes it.
func TestAnApprovalIsNeverAnotherPersons(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada, bob := person(noon.Add(time.Hour)), person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	for name, call := range map[string]func(context.Context) error{
		"approve": func(ctx context.Context) error { _, err := f.node(t).Approve(ctx, approve(p)); return err },
		"decline": func(ctx context.Context) error { _, err := f.node(t).Decline(ctx, approve(p)); return err },
		"report": func(ctx context.Context) error {
			_, err := f.node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
				Report: wire.ActionReport{State: wire.ActionApplying}})
			return err
		},
	} {
		if err := call(as(t.Context(), bob)); !errors.Is(err, approval.ErrNotFound) {
			t.Errorf("another person's %s answered %v, want not found", name, err)
		}
		if err := call(t.Context()); !errors.Is(err, approval.ErrNotAPerson) {
			t.Errorf("a %s with nobody resolved answered %v", name, err)
		}
	}
	assisted := ada
	assisted.Via = "assist:" + uuid.NewString()
	assisted.AssistScope = "exec"
	if _, err := f.node(t).Approve(as(t.Context(), assisted), approve(p)); !errors.Is(err, approval.ErrNotAPerson) {
		t.Errorf("an approval through the assistant answered %v: the assistant never "+
			"approves its own proposal", err)
	}
	if f.store.recorded() != 0 || f.api.sends() != 0 {
		t.Errorf("%d moves and %d sends for approvals nobody may make", f.store.recorded(), f.api.sends())
	}
}

// AN APPROVAL OF A CHANGED PROPOSAL IS REFUSED: a row whose request moved since
// the person was shown it, or an echo of another card's digest, sends nothing
// and records nothing. Mutation: skip the re-hash of the stored row and the
// tampered body is sent.
func TestAnApprovalOfAChangedProposalIsRefused(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	ctx := as(t.Context(), ada)
	p := f.propose(t, ada, nil)
	tampered := p
	tampered.Body = []byte(`{"args":{"title":"Grant everyone admin"}}`)
	f.store.proposals[p.ID] = tampered
	if _, err := f.node(t).Approve(ctx, approve(p)); !errors.Is(err, approval.ErrDigestMismatch) {
		t.Errorf("a proposal whose body moved was approved: %v", err)
	}
	other := f.propose(t, ada, nil)
	wrong := approve(other)
	wrong.Digest = p.Digest
	if _, err := f.node(t).Approve(ctx, wrong); !errors.Is(err, approval.ErrDigestMismatch) {
		t.Errorf("an approval echoing another card's digest was taken: %v", err)
	}
	if _, err := f.node(t).Decline(ctx, wrong); !errors.Is(err, approval.ErrDigestMismatch) {
		t.Errorf("a decline echoing another card's digest was taken: %v", err)
	}
	if f.store.recorded() != 0 || f.api.sends() != 0 {
		t.Errorf("%d moves and %d sends for changed proposals", f.store.recorded(), f.api.sends())
	}
}

// A STALE REVISION IS REFUSED: an object that moved (or went) since its
// preview is never written, the proposal is stale, and the turn is told so it
// may propose against the object as it is now. Mutation: skip the re-read.
func TestAStaleRevisionIsRefused(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(string) (string, error){
		"moved": func(string) (string, error) { return "task:ENG-1@4", nil },
		"gone":  func(string) (string, error) { return "", proposal.ErrGone },
	} {
		f := newFleet()
		f.revision = answer
		ada := person(noon.Add(time.Hour))
		p := f.propose(t, ada, nil)
		if _, err := f.node(t).Approve(as(t.Context(), ada), approve(p)); !errors.Is(err, approval.ErrStale) {
			t.Errorf("%s: an approval of a stale proposal answered %v", name, err)
		}
		if f.api.sends() != 0 || f.store.state(p.ID) != wire.ProposalStale {
			t.Errorf("%s: %d sends and state %s, want none and stale", name, f.api.sends(),
				f.store.state(p.ID))
		}
		if len(f.turns.resumed) != 1 || f.turns.resumed[0].State != wire.ProposalStale {
			t.Errorf("%s: the turn was not told its proposal is stale", name)
		}
	}
	f := newFleet()
	f.revision = func(string) (string, error) { return "", errors.New("the read timed out") }
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	if _, err := f.node(t).Approve(as(t.Context(), ada), approve(p)); !errors.Is(err, approval.ErrUnavailable) {
		t.Errorf("an approval whose revision could not be read answered %v, want unavailable", err)
	}
	if f.api.sends() != 0 || f.store.state(p.ID) != wire.ProposalProposed {
		t.Error("an approval nobody could check against its object went ahead")
	}
}

// A STEP-UP ROW ASKS THE APPROVER, before anything is recorded, whether the
// proposal says so or the verb's own row does; and the same approval goes
// through once they have confirmed it is them. Mutation: check the recency
// after recording the approval and the refusal leaves it approved.
func TestAStepUpRowAsksTheApprover(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*fleet) func(*proposal.Draft){
		"the proposal": func(*fleet) func(*proposal.Draft) {
			return func(d *proposal.Draft) { d.Preview.StepUp = true }
		},
		"the verb's row": func(f *fleet) func(*proposal.Draft) {
			f.stepUp["create_work_item"] = true
			return nil
		},
	} {
		f := newFleet()
		stale := person(noon.Add(-time.Minute))
		p := f.propose(t, stale, setup(f))
		_, err := f.node(t).Approve(as(t.Context(), stale), approve(p))
		if !errors.Is(err, approval.ErrStepUp) {
			t.Fatalf("%s: an unconfirmed approval answered %v, want a step-up", name, err)
		}
		if f.store.recorded() != 0 || f.api.sends() != 0 {
			t.Errorf("%s: a step-up refusal recorded %d moves and sent %d", name,
				f.store.recorded(), f.api.sends())
		}
		fresh := stale
		fresh.ReauthAt = noon.Add(5 * time.Minute)
		if got, err := f.node(t).Approve(as(t.Context(), fresh), approve(p)); err != nil ||
			got.Proposal.State != wire.ProposalApplied {
			t.Errorf("%s: the replayed approval answered %v, %v", name, got.Proposal.State, err)
		}
	}
}

// A COMPANY-WIDE SHARE ASKS A STEP-UP (D09): the share's preview names the
// whole company as its audience and asks the person to confirm it is them,
// and the approval holds them to it.
func TestACompanyWideShareAsksAStepUp(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(-time.Minute))
	p := f.propose(t, ada, func(d *proposal.Draft) {
		d.Tool, d.Act, d.ChangesVisibility = "share_artifact", "share_artifact", true
		d.Preview = proposal.Preview{
			Title: "Share artifact", ApproveLabel: "Approve and share",
			Changes:  []wire.Change{proposal.Changed("audience", "Who can view", "Only you", "Everyone")},
			Audience: []string{"Everyone in the company"}, StepUp: true,
			SourceRevision: "artifact:a1@2",
			Request: exec.Request{Method: "PUT", Path: "/artifacts/a1/shares",
				Body: []byte(`{"company":"viewer"}`)},
		}
	})
	if got := p.Card.Proposal; len(got.Audience) == 0 || !got.StepUp {
		t.Fatalf("a company-wide share's card lost its audience row or its step-up: %+v", got)
	}
	if _, err := f.node(t).Approve(as(t.Context(), ada), approve(p)); !errors.Is(err, approval.ErrStepUp) {
		t.Errorf("a company-wide share approved without a step-up: %v", err)
	}
}

// A DECLINE SENDS NOTHING and tells the turn; a Stop declines what the turn
// still waits on.
func TestADeclineSendsNothingAndTellsTheTurn(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	ctx := as(t.Context(), ada)
	p := f.propose(t, ada, nil)
	got, err := f.node(t).Decline(ctx, approve(p))
	if err != nil || got.Proposal.State != wire.ProposalDeclined {
		t.Fatalf("Decline answered %v, %v", got.Proposal.State, err)
	}
	if f.api.sends() != 0 || len(f.turns.resumed) != 1 ||
		f.turns.resumed[0].State != wire.ProposalDeclined {
		t.Errorf("a decline sent %d writes and resumed %v", f.api.sends(), f.turns.resumed)
	}
	if _, err := f.node(t).Approve(ctx, approve(p)); !errors.Is(err, approval.ErrAlreadyDecided) {
		t.Errorf("a declined proposal was approved: %v", err)
	}

	q := f.propose(t, ada, nil)
	if err := f.node(t).DeclineTurn(ctx, approval.Stop{Owner: ada.ID, Conversation: q.Conversation,
		Turn: q.Turn, Key: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	if f.store.state(q.ID) != wire.ProposalDeclined {
		t.Errorf("a Stop left the parked proposal %s", f.store.state(q.ID))
	}
}

// AN EXPIRED PROPOSAL IS NEVER APPROVED, and its expiry is recorded.
func TestAnExpiredProposalIsNeverApproved(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	f.clock = p.ExpiresAt
	if _, err := f.node(t).Approve(as(t.Context(), ada), approve(p)); !errors.Is(err, approval.ErrExpired) {
		t.Errorf("an expired proposal answered %v", err)
	}
	if f.api.sends() != 0 || f.store.state(p.ID) != wire.ProposalExpired {
		t.Errorf("an expired proposal sent %d and is %s", f.api.sends(), f.store.state(p.ID))
	}
}

// A REFUSED WRITE FAILS IN THE ENGINE'S OWN WORDS, and the turn is told.
func TestARefusedWriteFailsInTheEnginesWords(t *testing.T) {
	t.Parallel()
	f := newFleet()
	body, _ := json.Marshal(map[string]any{"error": "unauthorized", "message": "You do not have permission.",
		"grants": []string{"tracker"}})
	f.api.answers = []exec.Response{{Status: http.StatusForbidden, Body: body}}
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	got, err := f.node(t).Approve(as(t.Context(), ada), approve(p))
	if err != nil || got.Proposal.State != wire.ProposalFailed {
		t.Fatalf("a refused write answered %v, %v", got.Proposal.State, err)
	}
	r := f.turns.resumed
	if len(r) != 1 || r[0].Failure == nil || r[0].Failure.Refusal.Code != "unauthorized" ||
		r[0].Failure.Refusal.Grants[0] != "tracker" {
		t.Errorf("the turn was not handed the engine's refusal: %+v", r)
	}
}

// AN APPLIED WRITE HANDS THE TURN ITS RECEIPT and the position open screens
// raise their read floor to.
func TestAnAppliedWriteHandsTheTurnItsReceipt(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	got, err := f.node(t).Approve(as(t.Context(), ada), approve(p))
	if err != nil || got.StreamURL == "" || got.TurnID != p.Turn {
		t.Fatalf("Approve answered %+v, %v", got, err)
	}
	r := f.turns.resumed[0]
	switch {
	case r.Receipt == nil || r.Receipt.Position != "CREWLET_TRACKER_LOG@1:42":
		t.Errorf("the receipt is %+v", r.Receipt)
	case r.Invalidate == nil || r.Invalidate.Domain != "CREWLET_TRACKER_LOG":
		t.Errorf("the invalidation is %+v", r.Invalidate)
	case r.Card.Proposal.State != wire.ProposalApplied || r.ClientReported:
		t.Errorf("the card is %s and client-reported %v", r.Card.Proposal.State, r.ClientReported)
	case r.Read == nil || f.grants.revoked[r.Read.ID()]:
		t.Error("the resumed turn holds no live read grant from the approving request")
	}
	if x := f.grants.execs; len(x) != 1 || x[0].Request.Path != p.Path || x[0].Proposal != p.ID {
		t.Errorf("the exec grant was not derived for the stored request: %+v", x)
	}
}

// A PAGE ACTION IS CLAIMED ONCE ACROSS TABS: approval sends it to the page,
// one tab claims it and the other is refused, and the page's report resumes
// the turn as client-reported. Mutation: let a claim land from any state and
// both tabs apply it.
func TestAPageActionIsClaimedOnceAcrossTabs(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	ctx := as(t.Context(), ada)
	p := f.propose(t, ada, func(d *proposal.Draft) {
		d.Tool, d.Act, d.Scope = "propose_page_change", "", wire.ScopePage
		d.Preview.SourceRevision = "r7"
		d.Preview.Request = exec.Request{Method: proposal.PageMethod,
			Path: proposal.PageActionPath + "work.filters", Body: []byte(`{"status":"open"}`)}
	})
	got, err := f.node(t).Approve(ctx, approve(p))
	if err != nil || got.Proposal.State != wire.ProposalApproved || f.api.sends() != 0 {
		t.Fatalf("a page approval answered %v, %v after %d sends", got.Proposal.State, err, f.api.sends())
	}
	action := f.turns.emitted[0].Action
	if action == nil || action.ActionID != p.ID || action.Name != "work.filters" ||
		string(action.Payload) != `{"status":"open"}` || action.SourceRevision != "r7" {
		t.Fatalf("the page was sent %+v", action)
	}
	if err := action.Validate(); err != nil {
		t.Fatalf("the action frame does not validate: %v", err)
	}
	claim := func() error {
		_, err := f.node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
			Report: wire.ActionReport{State: wire.ActionApplying, Revision: "r7"}})
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = claim() })
	}
	wg.Wait()
	claimed := 0
	for _, err := range errs {
		switch {
		case err == nil:
			claimed++
		case !errors.Is(err, approval.ErrActionClaimed):
			t.Errorf("a losing claim answered %v", err)
		}
	}
	if claimed != 1 {
		t.Fatalf("%d tabs claimed the action, want one", claimed)
	}
	if _, err := f.node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
		Report: wire.ActionReport{State: wire.ActionApplied, Revision: "r8"}}); err != nil {
		t.Fatal(err)
	}
	r := f.turns.resumed
	if len(r) != 1 || !r[0].ClientReported || r[0].State != wire.ProposalApplied {
		t.Errorf("the page's report resumed %+v, want client-reported applied", r)
	}
	if f.store.state(p.ID) != wire.ProposalApplied {
		t.Errorf("the page action is %s", f.store.state(p.ID))
	}
}

// A SERVER WRITE IS NOT THE PAGE'S TO REPORT.
func TestAPageCannotReportAServerWrite(t *testing.T) {
	t.Parallel()
	f := newFleet()
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	if _, err := f.node(t).Report(as(t.Context(), ada), approval.Report{Action: p.ID,
		Key: uuid.NewString(), Report: wire.ActionReport{State: wire.ActionApplied}}); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("a page reported a server write: %v", err)
	}
}

// AN UNCONFIRMED DECISION BUILDS NOTHING: an approval the log cannot confirm
// sends no write and answers the operation to retry under.
func TestAnUnconfirmedApprovalSendsNothing(t *testing.T) {
	t.Parallel()
	f := newFleet()
	f.store.unknown = true
	ada := person(noon.Add(time.Hour))
	p := f.propose(t, ada, nil)
	ap := approve(p)
	_, err := f.node(t).Approve(as(t.Context(), ada), ap)
	var unconfirmed *approval.Unconfirmed
	if !errors.As(err, &unconfirmed) || unconfirmed.OpID == "" || f.api.sends() != 0 {
		t.Errorf("an unconfirmed approval answered %v after %d sends", err, f.api.sends())
	}
}
