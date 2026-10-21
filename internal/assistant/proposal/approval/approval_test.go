package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval/approvaltest"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A PROPOSAL EXECUTES ONCE, however many approvals race for it: two tabs on
// two nodes approving at the same instant send one write, and the loser is
// told the proposal was already decided. Mutation: drop the arbitrated
// approve record (send before or without it) and two writes are sent.
func TestAProposalExecutesOnce(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	nodes := []*approval.Approver{f.Node(t), f.Node(t)}
	ctx := approvaltest.As(t.Context(), ada)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = nodes[i%2].Approve(ctx, approvaltest.Approve(p))
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
	if won != 1 || len(f.API.Sent()) != 1 {
		t.Fatalf("%d approvals won and %d writes were sent; want one of each", won, len(f.API.Sent()))
	}
	if got := f.Store.State(p.ID); got != wire.ProposalApplied {
		t.Errorf("the proposal is %s, want applied", got)
	}
}

// A RETRIED PROPOSAL IS THE SAME WRITE. An answer lost on the way back reads
// as unknown and is never retried by itself; the person's Retry sends the
// proposal's own operation key again, so the ledger answers it with the
// first write. Mutation: mint a key per send and the handler lands two.
func TestARetriedProposalIsTheSameWrite(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	node := f.Node(t)
	ctx := approvaltest.As(t.Context(), ada)

	f.API.LoseNextAnswer(errors.New("the connection dropped"))
	got, err := node.Approve(ctx, approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalUnknown {
		t.Fatalf("a lost answer approved as %v, %v; want unknown", got.Proposal.State, err)
	}
	if len(f.Turns.Resumed()) != 0 {
		t.Error("an unknown outcome resumed the turn: it waits for the person's Retry")
	}
	got, err = node.Approve(ctx, approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalApplied {
		t.Fatalf("the Retry answered %v, %v; want applied", got.Proposal.State, err)
	}
	if len(f.API.Sent()) != 2 {
		t.Fatalf("%d sends, want the first and the Retry", len(f.API.Sent()))
	}
	for i, sent := range f.API.Sent() {
		if sent.IdempotencyKey != p.OpKey {
			t.Errorf("send %d carried key %q, want the proposal's own %q", i, sent.IdempotencyKey, p.OpKey)
		}
	}
	if n := f.API.Writes(p.OpKey); n != 1 {
		t.Errorf("%d writes landed under the proposal's key, want one", n)
	}
}

// AN UNKNOWN OUTCOME IS DECIDED ON READ: an approval whose node died before
// it settled is offered a Retry once it is past UnknownAfter, and not before.
func TestAnUnsettledApprovalIsOfferedARetryOnceItCannotStillLand(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	p.State, p.DecidedAt = wire.ProposalExecuting, approvaltest.Noon
	f.Store.Put(p)
	ctx := approvaltest.As(t.Context(), ada)
	if _, err := f.Node(t).Approve(ctx, approvaltest.Approve(p)); !errors.Is(err, approval.ErrAlreadyDecided) {
		t.Fatalf("an executing proposal inside its grant was approved again: %v", err)
	}
	f.Set(approvaltest.Noon.Add(proposal.UnknownAfter))
	got, err := f.Node(t).Approve(ctx, approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalApplied || len(f.API.Sent()) != 1 {
		t.Fatalf("the Retry of an unsettled write answered %v, %v after %d sends",
			got.Proposal.State, err, len(f.API.Sent()))
	}
}

// A RETRIED APPROVAL REQUEST IS THE SAME APPROVAL: the same key sent again
// after its answer was lost answers the proposal's state and sends nothing.
func TestARetriedApprovalRequestSendsNothingTwice(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	node, ctx, ap := f.Node(t), approvaltest.As(t.Context(), ada), approvaltest.Approve(p)
	if _, err := node.Approve(ctx, ap); err != nil {
		t.Fatal(err)
	}
	got, err := node.Approve(ctx, ap)
	if err != nil || got.Proposal.State != wire.ProposalApplied || len(f.API.Sent()) != 1 {
		t.Errorf("the same approval again answered %v, %v after %d sends",
			got.Proposal.State, err, len(f.API.Sent()))
	}
}

// AN APPROVAL IS NEVER ANOTHER PERSON'S: somebody else's proposal does not
// exist for them, and nothing is recorded or sent. Mutation: read the
// proposal by id alone and the second person executes it.
func TestAnApprovalIsNeverAnotherPersons(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada, bob := approvaltest.Person(approvaltest.Noon.Add(time.Hour)), approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	for name, call := range map[string]func(context.Context) error{
		"approve": func(ctx context.Context) error { _, err := f.Node(t).Approve(ctx, approvaltest.Approve(p)); return err },
		"decline": func(ctx context.Context) error { _, err := f.Node(t).Decline(ctx, approvaltest.Approve(p)); return err },
		"report": func(ctx context.Context) error {
			_, err := f.Node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
				Report: wire.ActionReport{State: wire.ActionApplying}})
			return err
		},
	} {
		if err := call(approvaltest.As(t.Context(), bob)); !errors.Is(err, approval.ErrNotFound) {
			t.Errorf("another person's %s answered %v, want not found", name, err)
		}
		if err := call(t.Context()); !errors.Is(err, approval.ErrNotAPerson) {
			t.Errorf("a %s with nobody resolved answered %v", name, err)
		}
	}
	assisted := ada
	assisted.Via = "assist:" + uuid.NewString()
	assisted.AssistScope = "exec"
	if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), assisted), approvaltest.Approve(p)); !errors.Is(err, approval.ErrNotAPerson) {
		t.Errorf("an approval through the assistant answered %v: the assistant never "+
			"approves its own proposal", err)
	}
	if f.Store.Recorded() != 0 || len(f.API.Sent()) != 0 {
		t.Errorf("%d moves and %d sends for approvals nobody may make", f.Store.Recorded(), len(f.API.Sent()))
	}
}

// AN APPROVAL OF A CHANGED PROPOSAL IS REFUSED: a row whose request moved since
// the person was shown it, or an echo of another card's digest, sends nothing
// and records nothing. Mutation: skip the re-hash of the stored row and the
// tampered body is sent.
func TestAnApprovalOfAChangedProposalIsRefused(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	ctx := approvaltest.As(t.Context(), ada)
	p := f.Propose(t, ada, nil)
	tampered := p
	tampered.Body = []byte(`{"args":{"title":"Grant everyone admin"}}`)
	f.Store.Put(tampered)
	if _, err := f.Node(t).Approve(ctx, approvaltest.Approve(p)); !errors.Is(err, approval.ErrDigestMismatch) {
		t.Errorf("a proposal whose body moved was approved: %v", err)
	}
	other := f.Propose(t, ada, nil)
	wrong := approvaltest.Approve(other)
	wrong.Digest = p.Digest
	if _, err := f.Node(t).Approve(ctx, wrong); !errors.Is(err, approval.ErrDigestMismatch) {
		t.Errorf("an approval echoing another card's digest was taken: %v", err)
	}
	if _, err := f.Node(t).Decline(ctx, wrong); !errors.Is(err, approval.ErrDigestMismatch) {
		t.Errorf("a decline echoing another card's digest was taken: %v", err)
	}
	if f.Store.Recorded() != 0 || len(f.API.Sent()) != 0 {
		t.Errorf("%d moves and %d sends for changed proposals", f.Store.Recorded(), len(f.API.Sent()))
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
		f := approvaltest.NewFleet()
		f.Revision = answer
		ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
		p := f.Propose(t, ada, nil)
		if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p)); !errors.Is(err, approval.ErrStale) {
			t.Errorf("%s: an approval of a stale proposal answered %v", name, err)
		}
		if len(f.API.Sent()) != 0 || f.Store.State(p.ID) != wire.ProposalStale {
			t.Errorf("%s: %d sends and state %s, want none and stale", name, len(f.API.Sent()),
				f.Store.State(p.ID))
		}
		if len(f.Turns.Resumed()) != 1 || f.Turns.Resumed()[0].State != wire.ProposalStale {
			t.Errorf("%s: the turn was not told its proposal is stale", name)
		}
	}
	f := approvaltest.NewFleet()
	f.Revision = func(string) (string, error) { return "", errors.New("the read timed out") }
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p)); !errors.Is(err, approval.ErrUnavailable) {
		t.Errorf("an approval whose revision could not be read answered %v, want unavailable", err)
	}
	if len(f.API.Sent()) != 0 || f.Store.State(p.ID) != wire.ProposalProposed {
		t.Error("an approval nobody could check against its object went ahead")
	}
}

// A STEP-UP ROW ASKS THE APPROVER, before anything is recorded, whether the
// proposal says so or the verb's own row does; and the same approval goes
// through once they have confirmed it is them. Mutation: check the recency
// after recording the approval and the refusal leaves it approved.
func TestAStepUpRowAsksTheApprover(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*approvaltest.Fleet) func(*proposal.Draft){
		"the proposal": func(*approvaltest.Fleet) func(*proposal.Draft) {
			return func(d *proposal.Draft) { d.Preview.StepUp = true }
		},
		"the verb's row": func(f *approvaltest.Fleet) func(*proposal.Draft) {
			f.StepUp["create_work_item"] = true
			return nil
		},
	} {
		f := approvaltest.NewFleet()
		stale := approvaltest.Person(approvaltest.Noon.Add(-time.Minute))
		p := f.Propose(t, stale, setup(f))
		_, err := f.Node(t).Approve(approvaltest.As(t.Context(), stale), approvaltest.Approve(p))
		if !errors.Is(err, approval.ErrStepUp) {
			t.Fatalf("%s: an unconfirmed approval answered %v, want a step-up", name, err)
		}
		if f.Store.Recorded() != 0 || len(f.API.Sent()) != 0 {
			t.Errorf("%s: a step-up refusal recorded %d moves and sent %d", name,
				f.Store.Recorded(), len(f.API.Sent()))
		}
		fresh := stale
		fresh.ReauthAt = approvaltest.Noon.Add(5 * time.Minute)
		if got, err := f.Node(t).Approve(approvaltest.As(t.Context(), fresh), approvaltest.Approve(p)); err != nil ||
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
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(-time.Minute))
	p := f.Propose(t, ada, func(d *proposal.Draft) {
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
	if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p)); !errors.Is(err, approval.ErrStepUp) {
		t.Errorf("a company-wide share approved without a step-up: %v", err)
	}
}

// A DECLINE SENDS NOTHING and tells the turn; a Stop declines what the turn
// still waits on.
func TestADeclineSendsNothingAndTellsTheTurn(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	ctx := approvaltest.As(t.Context(), ada)
	p := f.Propose(t, ada, nil)
	got, err := f.Node(t).Decline(ctx, approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalDeclined {
		t.Fatalf("Decline answered %v, %v", got.Proposal.State, err)
	}
	if len(f.API.Sent()) != 0 || len(f.Turns.Resumed()) != 1 ||
		f.Turns.Resumed()[0].State != wire.ProposalDeclined {
		t.Errorf("a decline sent %d writes and resumed %v", len(f.API.Sent()), f.Turns.Resumed())
	}
	if _, err := f.Node(t).Approve(ctx, approvaltest.Approve(p)); !errors.Is(err, approval.ErrAlreadyDecided) {
		t.Errorf("a declined proposal was approved: %v", err)
	}

	q := f.Propose(t, ada, nil)
	if err := f.Node(t).DeclineTurn(ctx, approval.Stop{Owner: ada.ID, Conversation: q.Conversation,
		Turn: q.Turn, Key: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	if f.Store.State(q.ID) != wire.ProposalDeclined {
		t.Errorf("a Stop left the parked proposal %s", f.Store.State(q.ID))
	}
}

// AN EXPIRED PROPOSAL IS NEVER APPROVED, and its expiry is recorded.
func TestAnExpiredProposalIsNeverApproved(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	f.Set(p.ExpiresAt)
	if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p)); !errors.Is(err, approval.ErrExpired) {
		t.Errorf("an expired proposal answered %v", err)
	}
	if len(f.API.Sent()) != 0 || f.Store.State(p.ID) != wire.ProposalExpired {
		t.Errorf("an expired proposal sent %d and is %s", len(f.API.Sent()), f.Store.State(p.ID))
	}
	if lapsed := f.Turns.Lapsed(); len(lapsed) != 1 || lapsed[0] != p.Turn {
		t.Errorf("the expired proposal's turn was asked to lapse %v, want once for %s", lapsed, p.Turn)
	}
}

// AN APPROVER WHO LOST WHAT THE TURN READ SENDS NOTHING: the proposal settles
// failed with the access_withdrawn refusal before any write, and the turn is
// handed the news only to end ([approval.Turns.Carries]).
//
// Mutation: skip the Carries check and the write is sent, and the turn resumes
// with what the person may no longer read.
func TestAnApproverWhoLostWhatTheTurnReadSendsNothing(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	f.Turns.Withdraw()
	got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalFailed {
		t.Fatalf("approve = %+v, %v; want failed", got, err)
	}
	if len(f.API.Sent()) != 0 || f.Store.State(p.ID) != wire.ProposalFailed {
		t.Errorf("sent %d and the proposal is %s; want nothing sent, failed",
			len(f.API.Sent()), f.Store.State(p.ID))
	}
	resumed := f.Turns.Resumed()
	if len(resumed) != 1 || resumed[0].State != wire.ProposalFailed || resumed[0].Failure == nil ||
		resumed[0].Failure.Refusal.Code != approval.CodeAccessWithdrawn {
		t.Errorf("the turn was handed %+v", resumed)
	}
}

// A VIEW CHANGE THE TURN APPLIED AT ONCE IS SETTLED BY ITS REPORT AND RESUMES
// NOTHING (D11): the turn approved a `confirm: none` action itself and carried
// on without parking, so the page's claim and report settle the proposal and
// hand no turn anything.
//
// Mutation: resume on every page report and a turn that never parked is asked
// to carry on from a call it never suspended on.
func TestAViewChangeAppliedAtOnceResumesNothing(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	ctx := approvaltest.As(t.Context(), ada)
	p := f.Propose(t, ada, func(d *proposal.Draft) {
		d.Tool, d.Act, d.Scope = "propose_page_change", "", wire.ScopePage
		d.Preview.SourceRevision, d.PageRevision = "", "r7"
		d.Preview.Request = exec.Request{Method: proposal.PageMethod,
			Path: proposal.PageActionPath + "work.filter.apply", Body: []byte(`{"status":"todo"}`)}
	})
	p.State = wire.ProposalApproved
	f.Store.Put(p)
	for _, report := range []wire.ActionReport{{State: wire.ActionApplying, Revision: "r7"},
		{State: wire.ActionApplied, Receipt: "Showing to do"}} {
		if _, err := f.Node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
			Report: report}); err != nil {
			t.Fatalf("the page's %s report answered %v", report.State, err)
		}
	}
	if f.Store.State(p.ID) != wire.ProposalApplied {
		t.Errorf("the view change is %s, want applied", f.Store.State(p.ID))
	}
	if resumed := f.Turns.Resumed(); len(resumed) != 0 {
		t.Errorf("a turn that never parked was resumed %+v", resumed)
	}
}

// A REFUSED WRITE FAILS IN THE ENGINE'S OWN WORDS, and the turn is told.
func TestARefusedWriteFailsInTheEnginesWords(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	body, _ := json.Marshal(map[string]any{"error": "unauthorized", "message": "You do not have permission.",
		"grants": []string{"tracker"}})
	f.API.AnswerNext(exec.Response{Status: http.StatusForbidden, Body: body})
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalFailed {
		t.Fatalf("a refused write answered %v, %v", got.Proposal.State, err)
	}
	r := f.Turns.Resumed()
	if len(r) != 1 || r[0].Failure == nil || r[0].Failure.Refusal.Code != "unauthorized" ||
		r[0].Failure.Refusal.Grants[0] != "tracker" {
		t.Errorf("the turn was not handed the engine's refusal: %+v", r)
	}
}

// NO ROUTE DETAIL REACHES A PERSON. A write the route refuses with its own
// account of the request (a tool's name, config keys in backticks, the values to
// send instead) settles with a card whose failure is the engine's sentence for
// the kind of refusal and the turn's next step, and that account goes to the
// model alone, as the write's answer the resumed turn reads. Mutation: show the
// route's detail or hint on the card, or keep the answer from the turn, and
// this fails.
func TestNoRouteDetailReachesAPerson(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	detail := "write_page refused that: `container`: COMPANY is not a space this company " +
		"declares (`knowledge.root_space`); write in one of: FAIR, HOME, TS"
	body, _ := json.Marshal(map[string]any{"error": "invalid_input",
		"message": "One of the values was not accepted.", "detail": detail,
		"hint": "use PUT /config to declare the space"})
	f.API.AnswerNext(exec.Response{Status: http.StatusUnprocessableEntity, Body: body})
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalFailed {
		t.Fatalf("a refused write answered %v, %v", got.Proposal.State, err)
	}
	r := f.Turns.Resumed()
	if len(r) != 1 || r[0].Failure == nil || r[0].Card.Proposal == nil ||
		r[0].Card.Proposal.Failure == nil {
		t.Fatalf("the turn was handed %+v, want the failure and its card", r)
	}
	shown := r[0].Card.Proposal.Failure.Refusal
	for _, words := range []string{shown.Message, shown.Hint, r[0].Failure.Refusal.Message} {
		for _, internal := range []string{"write_page", "`", "knowledge.", "PUT /config", "COMPANY"} {
			if strings.Contains(words, internal) {
				t.Errorf("a person reads %q, which carries %q", words, internal)
			}
		}
	}
	if shown.Code != "invalid_input" || shown.Message == "" || shown.Hint == "" {
		t.Errorf("the card's failure is %+v, want the code with the engine's sentence and next step",
			shown)
	}
	encoded, _ := json.Marshal(r[0].Card)
	if strings.Contains(string(encoded), "FAIR, HOME, TS") {
		t.Errorf("the card as the drawer is sent it carries the route's detail: %s", encoded)
	}
	if !strings.Contains(string(r[0].Answer), "FAIR, HOME, TS") {
		t.Errorf("the turn was handed the answer %s, want the route's whole account", r[0].Answer)
	}
}

// THE MODEL HEARS THE TOOL'S SENTENCE BESIDE A PERSON'S REFUSAL (ADR-0060).
// The route answers a refusal in a person's words, or with no detail where
// nobody has worded it, so the floor alone would leave the assistant's model
// unable to say what to change. The tool's own sentence, told in process
// ([exec.Response.ModelDetail]), is handed to the resumed turn beside the
// route's answer under [approval.ToolDetailKey], and never to the card the
// drawer draws. Mutation: drop it from the answer and the turn reads only the
// floor; put it on the card and the person reads a model's words.
func TestARefusalsToolSentenceReachesTheModelAlone(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	said := "comment_on_work_item was refused: the choice \"ship\" is not an option " +
		"of the decision. Change `choice` and comment again."
	body, _ := json.Marshal(map[string]any{"error": "invalid",
		"message": "That change was refused, and nothing was written."})
	f.API.AnswerNext(exec.Response{Status: http.StatusUnprocessableEntity, Body: body,
		ModelDetail: said})
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada),
		approvaltest.Approve(p)); err != nil {
		t.Fatal(err)
	}
	r := f.Turns.Resumed()
	if len(r) != 1 {
		t.Fatalf("the turn was resumed %d times", len(r))
	}
	var answer map[string]any
	if err := json.Unmarshal(r[0].Answer, &answer); err != nil {
		t.Fatalf("the turn's answer is not JSON: %s", r[0].Answer)
	}
	if answer[approval.ToolDetailKey] != said || answer["error"] != "invalid" {
		t.Errorf("the turn was handed %v, want the route's answer and the tool's sentence", answer)
	}
	card, _ := json.Marshal(r[0].Card)
	if strings.Contains(string(card), "`choice`") {
		t.Errorf("the card the drawer is sent carries the model's sentence: %s", card)
	}
}

// A STATE IS TOLD IN A PERSON'S WORDS, NEVER AS A RACE. An approved write the
// route refused for the state of what it was about, in the words its refusal
// carries for a person (ADR-0060) with the tool's own told beside it in
// process ([exec.Response.ModelDetail]), fails with a card in those words and
// a next step that promises no change, since only whoever changes the state
// clears it. Mutation: leave the person's words behind, or stop reading the
// mark that makes them a person's, and the card reads the conflict's own
// sentence.
func TestAStateIsToldInAPersonsWords(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	trashed := "This artifact is in the trash. Restore it first, then try again."
	body, _ := json.Marshal(map[string]any{"error": "conflict",
		"message": httpjson.CodeConflict.Message(), "detail": trashed})
	f.API.AnswerNext(exec.Response{Status: http.StatusConflict, Body: body,
		ModelDetail: "update_artifact refused that: the artifact is in the trash"})
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalFailed {
		t.Fatalf("a refused write answered %v, %v", got.Proposal.State, err)
	}
	r := f.Turns.Resumed()
	if len(r) != 1 || r[0].Card.Proposal == nil || r[0].Card.Proposal.Failure == nil {
		t.Fatalf("the turn was handed %+v, want the failure and its card", r)
	}
	shown := r[0].Card.Proposal.Failure.Refusal
	if shown.Code != "conflict" || shown.Message != trashed || shown.Hint != cards.RefusalResumes {
		t.Errorf("the card's failure is %+v, want the person's words %q", shown, trashed)
	}
}

// AN APPLIED WRITE HANDS THE TURN ITS RECEIPT and the position open screens
// raise their read floor to.
func TestAnAppliedWriteHandsTheTurnItsReceipt(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
	if err != nil || got.StreamURL == "" || got.TurnID != p.Turn {
		t.Fatalf("Approve answered %+v, %v", got, err)
	}
	r := f.Turns.Resumed()[0]
	switch {
	case r.Receipt == nil || r.Receipt.Position != "CREWLET_TRACKER_LOG@1:42" ||
		r.Receipt.Summary != "Created ENG-156":
		// THE RECEIPT NAMES WHAT WAS CREATED, from the act tool's own
		// receipt, so the card says which task landed rather than
		// "Applied".
		t.Errorf("the receipt is %+v", r.Receipt)
	case r.Invalidate == nil || r.Invalidate.Domain != "tracker" ||
		r.Invalidate.Position != "CREWLET_TRACKER_LOG@1:42":
		// NAMED BY THE DOMAIN, the key a screen's read floors are kept
		// under: the stream's name raised no floor on any screen.
		t.Errorf("the invalidation is %+v", r.Invalidate)
	case r.Card.Proposal.State != wire.ProposalApplied || r.ClientReported:
		t.Errorf("the card is %s and client-reported %v", r.Card.Proposal.State, r.ClientReported)
	case r.Read == nil || f.Grants.Revoked(r.Read.ID()):
		t.Error("the resumed turn holds no live read grant from the approving request")
	}
	if x := f.Grants.Execs(); len(x) != 1 || x[0].Request.Path != p.Path || x[0].Proposal != p.ID {
		t.Errorf("the exec grant was not derived for the stored request: %+v", x)
	}
}

// A WRITE THAT LANDED WITHOUT NAMING A POSITION STILL MOVES THE SCREENS: a
// route outside the act surface (a brand adopted, a seat changed through the
// configuration) answers no position, and the page behind the drawer kept
// drawing what it held before the write until it was reloaded. The turn is
// handed an invalidation all the same, with no floor to wait for.
//
// Mutation: emit an invalidation only for an answer that names a position,
// and this turn is handed none.
func TestAWriteWithNoPositionStillInvalidatesTheScreens(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	f.API.AnswerNext(exec.Response{Status: http.StatusOK, Body: []byte(`{"version":4}`)})
	if _, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada),
		approvaltest.Approve(p)); err != nil {
		t.Fatal(err)
	}
	r := f.Turns.Resumed()[0]
	if r.State != wire.ProposalApplied || r.Invalidate == nil ||
		*r.Invalidate != (wire.Invalidate{}) {
		t.Fatalf("the resumed turn is %s with invalidation %+v, want applied with an "+
			"invalidation naming no position", r.State, r.Invalidate)
	}
}

// A PAGE ACTION IS CLAIMED ONCE ACROSS TABS: approval sends it to the page,
// one tab claims it and the other is refused, and the page's report resumes
// the turn as client-reported. Mutation: let a claim land from any state and
// both tabs apply it.
func TestAPageActionIsClaimedOnceAcrossTabs(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	ctx := approvaltest.As(t.Context(), ada)
	p := f.Propose(t, ada, func(d *proposal.Draft) {
		d.Tool, d.Act, d.Scope = "propose_page_change", "", wire.ScopePage
		d.Preview.SourceRevision, d.PageRevision = "", "r7"
		d.Preview.Request = exec.Request{Method: proposal.PageMethod,
			Path: proposal.PageActionPath + "work.filters", Body: []byte(`{"status":"open"}`)}
	})
	ap := approvaltest.Approve(p)
	got, err := f.Node(t).Approve(ctx, ap)
	if err != nil || got.Proposal.State != wire.ProposalApproved || len(f.API.Sent()) != 0 {
		t.Fatalf("a page approval answered %v, %v after %d sends", got.Proposal.State, err, len(f.API.Sent()))
	}
	if again, err := f.Node(t).Approve(ctx, ap); err != nil || len(f.Turns.Emitted()) != 2 ||
		again.Proposal.State != wire.ProposalApproved {
		t.Fatalf("the same approval again answered %v, %v and sent the page %d actions; "+
			"want the action sent again", again.Proposal.State, err, len(f.Turns.Emitted()))
	}
	action := f.Turns.Emitted()[0].Action
	if action == nil || action.ActionID != p.ID || action.Name != "work.filters" ||
		string(action.Payload) != `{"status":"open"}` || action.SourceRevision != "r7" {
		t.Fatalf("the page was sent %+v", action)
	}
	if err := action.Validate(); err != nil {
		t.Fatalf("the action frame does not validate: %v", err)
	}
	claim := func() error {
		_, err := f.Node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
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
	if _, err := f.Node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
		Report: wire.ActionReport{State: wire.ActionApplied, Revision: "r8"}}); err != nil {
		t.Fatal(err)
	}
	r := f.Turns.Resumed()
	if len(r) != 1 || !r[0].ClientReported || r[0].State != wire.ProposalApplied {
		t.Errorf("the page's report resumed %+v, want client-reported applied", r)
	}
	if f.Store.State(p.ID) != wire.ProposalApplied {
		t.Errorf("the page action is %s", f.Store.State(p.ID))
	}
}

// A SERVER WRITE IS NOT THE PAGE'S TO REPORT.
func TestAPageCannotReportAServerWrite(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	if _, err := f.Node(t).Report(approvaltest.As(t.Context(), ada), approval.Report{Action: p.ID,
		Key: uuid.NewString(), Report: wire.ActionReport{State: wire.ActionApplied}}); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("a page reported a server write: %v", err)
	}
}

// AN UNCONFIRMED DECISION BUILDS NOTHING: an approval the log cannot confirm
// sends no write and answers the operation to retry under.
func TestAnUnconfirmedApprovalSendsNothing(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	f.Store.AnswerUnknown()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	ap := approvaltest.Approve(p)
	_, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), ap)
	var unconfirmed *approval.Unconfirmed
	if !errors.As(err, &unconfirmed) || unconfirmed.OpID == "" || len(f.API.Sent()) != 0 {
		t.Errorf("an unconfirmed approval answered %v after %d sends", err, len(f.API.Sent()))
	}
}

// A PAGE PROPOSAL IS STALE ONCE THE PAGE'S REVISION MOVES: a page action is
// prepared against the open screen's revision, and a claim from a screen that
// has moved since is refused by the server, recorded stale, and handed to the
// turn, so nothing is applied to a screen the person was not shown it for.
// Mutation: skip the revision check in Report and the action is claimed.
func TestAPageProposalIsStaleOnceThePageMoves(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	ctx := approvaltest.As(t.Context(), ada)
	p := f.Propose(t, ada, func(d *proposal.Draft) {
		d.Tool, d.Act, d.Scope, d.PageRevision = "propose_page_change", "", wire.ScopePage, "url:r7"
		d.Preview.SourceRevision = ""
		d.Preview.Request = exec.Request{Method: proposal.PageMethod,
			Path: proposal.PageActionPath + "work.filters", Body: []byte(`{"status":"open"}`)}
	})
	if p.SourceRevision != "url:r7" {
		t.Fatalf("the page action was prepared against %q, want the page's url:r7", p.SourceRevision)
	}
	if _, err := f.Node(t).Approve(ctx, approvaltest.Approve(p)); err != nil {
		t.Fatal(err)
	}
	_, err := f.Node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
		Report: wire.ActionReport{State: wire.ActionApplying, Revision: "url:r8"}})
	if !errors.Is(err, approval.ErrStale) {
		t.Fatalf("a claim from a moved page answered %v, want stale", err)
	}
	if f.Store.State(p.ID) != wire.ProposalStale {
		t.Errorf("the page action is %s, want stale", f.Store.State(p.ID))
	}
	r := f.Turns.Resumed()
	if len(r) != 1 || r[0].State != wire.ProposalStale || r[0].Failure == nil {
		t.Errorf("the turn was not told the page moved: %+v", r)
	}
	if _, err := f.Node(t).Report(ctx, approval.Report{Action: p.ID, Key: uuid.NewString(),
		Report: wire.ActionReport{State: wire.ActionApplying, Revision: "url:r7"}}); !errors.Is(err, approval.ErrActionClaimed) {
		t.Errorf("a stale page action was claimed afterwards: %v", err)
	}
}

// A REPEATED DECISION THE STORE CANNOT ANSWER IS UNCONFIRMED, never another's:
// an approval or a decline sent again after the first one decided the
// proposal, met by a store that fails or answers unknown, is told nobody can
// confirm it (503 with its operation), because the retry may be the very
// decision that landed. Mutation: answer any failed repeat move with Decided
// and every row reads already decided.
func TestARepeatedDecisionTheStoreCannotAnswerIsUnconfirmed(t *testing.T) {
	t.Parallel()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	ctx := approvaltest.As(t.Context(), ada)
	approve := func(a *approval.Approver, ap approval.Approval) error {
		_, err := a.Approve(ctx, ap)
		return err
	}
	decline := func(a *approval.Approver, ap approval.Approval) error {
		_, err := a.Decline(ctx, ap)
		return err
	}
	failing := func(s *approvaltest.Store) { s.FailMoves(errors.New("the broker is unreachable")) }
	unheard := func(s *approvaltest.Store) { s.AnswerUnknown() }
	for _, tc := range []struct {
		name   string
		decide func(*approval.Approver, approval.Approval) error
		store  func(*approvaltest.Store)
	}{
		{"an approval, the store failing", approve, failing},
		{"an approval, the store unheard", approve, unheard},
		{"a decline, the store failing", decline, failing},
		{"a decline, the store unheard", decline, unheard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := approvaltest.NewFleet()
			p := f.Propose(t, ada, nil)
			node := f.Node(t)
			if err := tc.decide(node, approvaltest.Approve(p)); err != nil {
				t.Fatal(err)
			}
			tc.store(f.Store)
			err := tc.decide(node, approvaltest.Approve(p))
			var unconfirmed *approval.Unconfirmed
			if !errors.As(err, &unconfirmed) || errors.Is(err, approval.ErrAlreadyDecided) {
				t.Errorf("the repeated decision answered %v, want Unconfirmed", err)
			}
		})
	}
}

// A SETTLEMENT HAS A DEADLINE OF ITS OWN: the settle record and the
// resumption run detached from the approving request, which has none, and
// within SettleTimeout, so a store that hangs never pins the approval for
// ever. Mutation: settle on the bare detached context and both deadlines read
// zero.
func TestASettlementIsBounded(t *testing.T) {
	t.Parallel()
	f := approvaltest.NewFleet()
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, nil)
	ctx := approvaltest.As(context.Background(), ada)
	before := time.Now()
	if _, err := f.Node(t).Approve(ctx, approvaltest.Approve(p)); err != nil {
		t.Fatal(err)
	}
	limit := time.Now().Add(approval.SettleTimeout)
	moves, resumes := f.Store.MoveDeadlines(), f.Turns.ResumeDeadlines()
	if len(moves) == 0 || len(resumes) != 1 {
		t.Fatalf("recorded %d moves and %d resumptions", len(moves), len(resumes))
	}
	for name, bound := range map[string]time.Time{"the settle record": moves[len(moves)-1],
		"the resumption": resumes[0]} {
		if bound.IsZero() || bound.Before(before) || bound.After(limit) {
			t.Errorf("%s ran with deadline %v, want one within %v of the approval",
				name, bound, approval.SettleTimeout)
		}
	}
}

// A CREATE THAT RACED AN EXISTING RESOURCE IS SHOWN AS WHAT IT LOST TO.
//
// The approved create is sent under the `If-None-Match: *` its revision names,
// and somebody filled the path between the approval's re-read and the write:
// the route answers `412 entity_exists`, about a header the person never saw.
// The proposal fails in the proposal's own terms (something was created there
// since; nothing changed) under the stale code, and the turn is told. An edit
// under an If-Match that the route answers 412 reads the same way, and a 412
// to a write that carried no precondition stays the route's own words.
// Mutation: hand the route's refusal over as it came, or send the create with
// no If-None-Match, and a case here fails.
func TestARacedCreateIsRefusedAsStale(t *testing.T) {
	t.Parallel()
	exists, _ := json.Marshal(map[string]any{"error": "entity_exists",
		"message": "An entity by that name already exists.",
		"hint":    "If-None-Match: * asked for mcp-servers/linear to be added"})
	for name, c := range map[string]struct {
		revision   string
		code, hint string
	}{
		"a create": {revision: proposal.Revision{Kind: proposal.RevisionCreate,
			ID: "/config/mcp-servers/linear", Version: proposal.VersionAbsent}.String(),
			code: approval.CodeStale},
		"an edit": {revision: proposal.Revision{Kind: proposal.RevisionEntity,
			ID: "/config/mcp-servers/linear", Version: "rev-3"}.String(),
			code: approval.CodeStale},
		"an unconditional write": {revision: "", code: "entity_exists",
			hint: "If-None-Match: * asked for mcp-servers/linear to be added"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := approvaltest.NewFleet()
			f.API.AnswerNext(exec.Response{Status: http.StatusPreconditionFailed, Body: exists})
			ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
			p := f.Propose(t, ada, func(d *proposal.Draft) {
				d.Tool, d.Act = "propose_api_change", "config.org.write"
				d.Preview.SourceRevision = c.revision
				d.Preview.Request = exec.Request{Method: http.MethodPut,
					Path: "/config/mcp-servers/linear", Body: []byte(`{"name":"linear"}`)}
			})
			got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
			if err != nil || got.Proposal.State != wire.ProposalFailed {
				t.Fatalf("the raced write answered %v, %v", got.Proposal.State, err)
			}
			sent := f.API.Sent()
			want := proposal.RequestOf(p)
			if len(sent) != 1 || sent[0].IfMatch != want.IfMatch ||
				sent[0].IfNoneMatch != want.IfNoneMatch || sent[0].Path != want.Path {
				t.Fatalf("sent %+v, want the proposal's own request %+v", sent, want)
			}
			if c.revision != "" && sent[0].IfMatch == "" && sent[0].IfNoneMatch == "" {
				t.Errorf("the conditional write was sent with no precondition: %+v", sent[0])
			}
			r := f.Turns.Resumed()
			if len(r) != 1 || r[0].Failure == nil || r[0].Failure.Refusal.Code != c.code ||
				r[0].Failure.Refusal.Message == "" {
				t.Fatalf("the turn was handed %+v, want code %s", r, c.code)
			}
			refusal := r[0].Failure.Refusal
			// THE ROUTE'S OWN WORDS ARE THE MODEL'S, handed over as the
			// write's answer; the card's failure is the person's, naming no
			// header and no path.
			if c.hint != "" && !strings.Contains(string(r[0].Answer), c.hint) {
				t.Errorf("the turn was handed the answer %s, want the route's own hint %q",
					r[0].Answer, c.hint)
			}
			for _, shown := range []string{refusal.Message, refusal.Hint} {
				if strings.Contains(shown, "If-") || strings.Contains(shown, "/config") {
					t.Errorf("the failed write reads %q / %q, not in the person's terms",
						refusal.Message, refusal.Hint)
				}
			}
		})
	}
}

// A REFUSAL NO CHANGE MENDS IS NOT PROMISED ONE. A steered turn that has taken
// every note a turn takes answers its next one `409 notes_spent`: the failed
// proposal reads as what happened — the turn has taken all it takes, the rest
// belongs on its work item — never as a change that collided with another, and
// its hint is the settled one, which says what can still be done, never the
// one for a state whoever changes it clears, since nothing clears it for that
// turn. The turn is handed the route's own refusal.
//
// Mutation: hint every refusal with cards.RefusalResumes, or word every 409 as
// a collision, and this goes red.
func TestARefusalNoChangeMendsIsNotPromisedOne(t *testing.T) {
	t.Parallel()
	spent, _ := json.Marshal(map[string]any{"error": "notes_spent",
		"message": "The turn has taken as many notes as a turn takes, so nothing was sent."})
	f := approvaltest.NewFleet()
	f.API.AnswerNext(exec.Response{Status: http.StatusConflict, Body: spent})
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := f.Propose(t, ada, func(d *proposal.Draft) {
		d.Tool, d.Act = "steer_turn", "steer_turn"
		d.Preview.Title, d.Preview.ApproveLabel = "Send a note", "Approve and send"
		d.Preview.SourceRevision = ""
		d.Preview.Request = exec.Request{Method: http.MethodPost, Path: "/operator/act/steer_turn",
			Body: []byte(`{"args":{"turn_id":"run-1","note":"use staging"}}`)}
	})
	got, err := f.Node(t).Approve(approvaltest.As(t.Context(), ada), approvaltest.Approve(p))
	if err != nil || got.Proposal.State != wire.ProposalFailed {
		t.Fatalf("the refused note answered %v, %v", got.Proposal.State, err)
	}
	r := f.Turns.Resumed()
	if len(r) != 1 || r[0].Failure == nil || r[0].Failure.Refusal.Code != "notes_spent" {
		t.Fatalf("the turn was handed %+v, want the route's notes_spent", r)
	}
	refusal := r[0].Failure.Refusal
	if strings.Contains(refusal.Message, "same moment") || !strings.Contains(refusal.Message, "work item") {
		t.Errorf("the spent turn reads %q, want what happened, never a collision", refusal.Message)
	}
	if refusal.Hint != cards.RefusalSettled {
		t.Errorf("the spent turn is hinted %q, want %q", refusal.Hint, cards.RefusalSettled)
	}
}
