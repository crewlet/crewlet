package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
)

// A tool call at Needs approval parks its run as a run's ask does (ADR-0077
// holds the argument): the bridge records it as the job's question
// ([PendingStore.RecordAsk], an [Ask] carrying a [HeldCall]), and only a
// person (an administrator or a lead of the seat's unit, never the assistant)
// answers it, at `POST /mcp-approvals/{id}` alone, with one of [HeldChoices]:
// a chat reply ([ConversationRef.Answers]) or `answer_executor` never does, and
// the seat's holder spends an answer not naming it by its id ([heldMismatch]).
// Its arguments and result ride the row sealed ([CallSeal]). The engine host
// makes the call at most once, since it may have taken effect: marked first
// ([PendingStore.MarkHeldCall]), and a resume finding the mark answers from it
// before holding the call to the run's tools and skills ([HeldResume.Call]).
// It joins its round's log ([HeldMade]), or reads as a reply never sent.

// The three answers every held call offers, as a [decision.Option] id the
// person's answer names ([HeldDecision]).
const (
	// HeldOnce makes the call, this once.
	HeldOnce = "once"
	// HeldAlways makes the call and sets the tool to Always allow on its
	// server's entry. The route writes the level first, as a configuration
	// write at a recent step-up (internal/api/mcpapi).
	HeldAlways = "always"
	// HeldDeny does not make the call: the run is told it was refused
	// ([RefusalForbidden]).
	HeldDeny = "deny"
)

// HeldAct is the decision a held call carries out, as its approver reads it:
// the work item, the question its seat asked there, and the answer a person
// gave it.
type HeldAct struct {
	Item     string `json:"item"`
	Key      string `json:"key,omitempty"`
	Ask      string `json:"ask"`
	Question string `json:"question" untrusted:"name"`
	// Choice is the chosen option's label, empty for an answer in words, and
	// ChoiceID its id; Act is what the question was for where the engine
	// composed it (ADR-0125), which a call that matches it exactly is made on.
	Choice     string         `json:"choice,omitempty" untrusted:"name"`
	ChoiceID   string         `json:"choice_id,omitempty"`
	Act        *delegated.Act `json:"act,omitempty"`
	AnsweredBy string         `json:"answered_by"`
	AnsweredAt time.Time      `json:"answered_at"`
}

// HeldChoices is every answer, in the order a person is offered them.
var HeldChoices = []string{HeldOnce, HeldAlways, HeldDeny}

// HeldFixedChoices are the answers a call whose tool always asks offers
// ([HeldCall.Fixed]): no Always allow, since no level of it can be set.
var HeldFixedChoices = []string{HeldOnce, HeldDeny}

// ChoicesOf is the answers h offers.
func ChoicesOf(h HeldCall) []string {
	if h.Fixed {
		return HeldFixedChoices
	}
	return HeldChoices
}

// HeldApproversSSH is [HeldCall.Approvers] for an SSH operate command: an
// administrator or a person integrations.ssh.approvers names, at a fresh
// step-up (authz's ssh.operate.approve).
const HeldApproversSSH = "ssh"

// HeldApproversRelease is [HeldCall.Approvers] for a merge or a deploy asked
// with ask_to_merge or ask_to_deploy: an administrator, a lead of the seat, or
// a person the kind's delegated-decisions rule names in escalate_to, at a
// fresh step-up (authz's release.approve).
const HeldApproversRelease = "release"

// HeldApproversAdmin is [HeldCall.Approvers] for a growth integration's
// spend, public or admin act: a person holding admin alone, at a fresh
// step-up, never a lead, a named person or a delegate (ADR-0171; authz's
// growth.*.approve). Such a call lapses at [HeldCall.LapsesAt].
const HeldApproversAdmin = "admin"

// HeldLapse is how long a growth act waits for its answer before it lapses:
// a day, the budget cycle its change acts on. A raise allowed later acts on
// another day's spend than the one the agent reasoned about.
const HeldLapse = 24 * time.Hour

// heldApprovers is every [HeldCall.Approvers] a call may name beside empty.
var heldApprovers = []string{HeldApproversSSH, HeldApproversRelease, HeldApproversAdmin}

// maxHeldBindingBytes bounds [HeldCall.Binding]: a SHA-256 in hex.
const maxHeldBindingBytes = 64

// heldMismatch reports whether given is an answer by turn that the run's held
// call, or its lack of one, refuses: an answer to a held call that does not
// name it by its id, or one naming a held call the run's question is not.
func heldMismatch(run PendingRun, given types.ExecutorAnswerGiven) bool {
	if run.HeldCall == nil {
		return given.Approval != ""
	}
	return given.Approval == "" || given.Approval != run.HeldCall.ID
}

// HeldResume is the held call an answer carries its run on with
// ([AskedRun.Held]): the call as the claimed row recorded it, the choice, and
// the two writes the resume makes under the claim's fence
// ([PendingStore.MarkHeldCall], [PendingStore.RecordHeldResult]).
type HeldResume struct {
	// TurnID is the run, which every seal of the call is bound to.
	TurnID string
	// Call is the held call as the claim read it, marked and its result
	// recorded where an earlier attempt at the same answer got that far. The
	// resume answers from those first, since nothing clears a mark while the
	// claim holds the row.
	Call HeldCall
	// Choice is the answer: [HeldOnce], [HeldAlways] or [HeldDeny].
	Choice string
	// By is who answered, as the answer's own record names them.
	By string
	// Mark records that the call is being made, returning the row's held
	// call and whether this mark is the first: false means an earlier
	// attempt marked it, and it is not made again.
	Mark func(ctx context.Context) (HeldCall, bool, error)
	// Record records what the call came to: its sealed result and the call
	// as its round's log keeps it.
	Record func(ctx context.Context, made HeldMade) error
}

// HeldMade is what a held call came to once a person let it be made, as its
// run's row records it ([PendingStore.RecordHeldResult]).
type HeldMade struct {
	// Result is what the call returned, sealed ([CallSeal.SealResult]), for
	// the job that carries the work on.
	Result string

	// Call is the call as its round's log keeps it, shaped as a bridged call
	// ([BridgeLog.Calls]): whether it failed and who answered it (origin,
	// server, source, a refusal's class). The store names, places and marks
	// it approved and sets [BridgeCall.HeldID]; its arguments and output are
	// never beside it, since they ride the row sealed.
	Call BridgeCall
}

// askedWith is [askedRun] with the claimed run's held call, where its question
// is one, and the writes that make it under the claim's fence.
func (c *Coordinator) askedWith(claimed PendingRun, words AnswerWords) *AskedRun {
	asked := askedRun(claimed, words)
	if claimed.HeldCall == nil {
		return asked
	}
	fence := fenceOf(claimed)
	turnID, launch := claimed.TurnID, claimed.LaunchID
	asked.Held = &HeldResume{
		TurnID: turnID, Call: *claimed.HeldCall, Choice: words.Choice, By: words.By,
		Mark: func(ctx context.Context) (HeldCall, bool, error) {
			return c.pending.MarkHeldCall(ctx, turnID, launch, fence)
		},
		Record: func(ctx context.Context, made HeldMade) error {
			recorded, err := c.pending.RecordHeldResult(ctx, turnID, launch, made, fence)
			if err == nil && !recorded {
				err = fmt.Errorf("%w: run %s", ErrHeldCallUnclaimed, turnID)
			}
			return err
		},
	}
	return asked
}

// HeldMakes reports whether answer choice makes the call: once or always.
func HeldMakes(choice string) bool { return choice == HeldOnce || choice == HeldAlways }

// HeldDecision is the decision a held call is asked as: its question, the
// three answers, and the evidence (the run's work item, if any). Role
// approver, since the answer decides the call; no recommendation, since the
// engine asks on the model's behalf and has no view.
func HeldDecision(question string, evidence []decision.Evidence) *decision.Decision {
	return heldDecision(question, evidence, HeldChoices)
}

// HeldFixedDecision is the decision a call whose tool always asks is asked as
// ([HeldCall.Fixed]): Allow once and Deny alone.
func HeldFixedDecision(question string, evidence []decision.Evidence) *decision.Decision {
	return heldDecision(question, evidence, HeldFixedChoices)
}

func heldDecision(question string, evidence []decision.Evidence, choices []string) *decision.Decision {
	options := map[string]decision.Option{
		HeldOnce: {ID: HeldOnce, Label: "Allow once", Detail: "Make this call, and ask again next time."},
		HeldAlways: {ID: HeldAlways, Label: "Always allow",
			Detail: "Make this call, and set the tool to run without asking from now on."},
		HeldDeny: {ID: HeldDeny, Label: "Deny", Detail: "Do not make this call; the run is told it was refused."},
	}
	d := &decision.Decision{Question: question, Evidence: evidence, Role: decision.RoleApprover}
	for _, id := range choices {
		d.Options = append(d.Options, options[id])
	}
	return d
}

// HeldCall is a call to a tool at Needs approval that a run's job made and the
// engine did not, recorded on the run's row with the question it is asked as
// ([PendingRun.HeldCall]).
//
// THE JSON TAGS ARE A WIRE FORMAT, as the row's are, and the section carries
// the keys a later build adds to it ([HeldCall.Extra]).
type HeldCall struct {
	// ID names the held call, a uuid minted when it is held: what `POST
	// /mcp-approvals/{id}` answers it by, and what the answer the seat's
	// holder records must name.
	ID string `json:"id"`

	// LaunchID is the job that made the call, which the question is pinned
	// to.
	LaunchID string `json:"launch_id"`

	// Server is the MCP server's configured name; Tool the tool's name as
	// the run called it (`tool_prefix` applied); Raw the server's own name
	// for it, which a level is set by; Digest the definition a person
	// approves (internal/mcp's Definition.Digest), which the resume holds
	// the call to.
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Raw    string `json:"raw"`
	Digest string `json:"digest"`

	// Arguments are the call's arguments in their canonical form, sealed
	// ([CallSeal.SealArguments]): never on the row in the clear.
	Arguments string `json:"arguments"`

	// Fixed is a call to a tool that asks before every call and has no
	// level to set, an engine tool such as ssh_operate: answered once or
	// deny alone ([HeldFixedChoices]).
	Fixed bool `json:"fixed,omitempty"`

	// Approvers names who may answer where it is not the run's seat's lead
	// or an administrator: [HeldApproversSSH] for an SSH operate command,
	// [HeldApproversRelease] for a merge or a deploy.
	// Empty is the seat's lead or an administrator (tools.approve).
	Approvers string `json:"approvers,omitempty"`

	// Binding is a digest of what a fixed call's tool binds it to beyond
	// its arguments, as the person was shown it (an SSH host's address,
	// port, operate user and keys): the call is made only while it holds.
	Binding string `json:"binding,omitempty"`

	// Acting is the decision the call carries out: the newest question its
	// seat asked on the turn's work item that a person answered; nil for none.
	Acting *HeldAct `json:"acting,omitempty"`

	// Act is what the call is for, set by the engine from the tool that made
	// it (internal/delegation's Kind): ssh_operate for an SSH operate command,
	// other for every other call.
	Act string `json:"act,omitempty"`

	// Subject is the identity of what a fixed call decides, apart from its
	// wording ([delegated.Subject]): its seat's repeat joins this call while
	// it waits ([PendingRun.HeldOpen]). Empty for a call nothing joins.
	Subject string `json:"subject,omitempty"`

	// Delegation is the rule that put the call to agent delegates before any
	// person (ADR-0125), nil for a call put to people at once. While it is
	// not escalated the call waits on its delegates alone: it is put to
	// nobody's Inbox.
	Delegation *HeldDelegation `json:"delegation,omitempty"`

	// HeldAt is when the call was held, and HeldOn the node whose clock read it,
	// which judges its delegates' deadline on that clock ([HeldCall.WaitedFor]).
	HeldAt time.Time `json:"held_at"`
	HeldOn string    `json:"held_on,omitempty"`

	// LapsesAt is when a call answered by [HeldApproversAdmin] lapses
	// unanswered, [HeldLapse] after HeldAt: the engine then answers it Deny,
	// and nothing runs. Zero for every other call, which waits until answered.
	LapsesAt time.Time `json:"lapses_at,omitzero"`

	// CallingAt is when a resume began making the call
	// ([PendingStore.MarkHeldCall]): zero until then, and from then on the
	// reason no resume makes it again.
	CallingAt time.Time `json:"calling_at,omitzero"`

	// Result is what the call returned, sealed ([CallSeal.SealResult]),
	// and MadeAt when it was recorded; both empty until the call returns,
	// and empty for good where the node making it stopped first.
	Result string    `json:"result,omitempty"`
	MadeAt time.Time `json:"made_at,omitzero"`

	// Call is the call as its round's log keeps it, written with the result
	// ([PendingStore.RecordHeldResult]): the row and the log are two keys
	// written in no one step, so the log is appended after, once by the held
	// call's id, and an append that never landed is read off the row by an
	// ending ([PendingStore.TurnLog]) or appended by the next job's opening
	// ([OwedCall]). Never its arguments or output.
	Call *BridgeCall `json:"call,omitempty"`

	// Extra is every key of the section this build does not know: see
	// wirekeys.go.
	Extra map[string]json.RawMessage `json:"-"`
}

// Calling reports whether a resume has begun making the call.
func (h HeldCall) Calling() bool { return !h.CallingAt.IsZero() }

// Lapsed reports whether the call lapses and now is past it ([HeldCall.LapsesAt]):
// it then takes Deny alone, never an answer that makes it.
func (h HeldCall) Lapsed(now time.Time) bool { return !h.LapsesAt.IsZero() && !now.Before(h.LapsesAt) }

// WaitedFor reports whether the call has waited d by node's clock at now: from
// HeldAt where node's clock read it ([HeldCall.HeldOn]); from seen, when node
// first saw it waiting, on its own clock; or from HeldAt and
// [RowClockAllowance], whoever read it (rowclock.go). A zero seen is never seen.
func (h HeldCall) WaitedFor(node string, now, seen time.Time, d time.Duration) bool {
	switch {
	case h.HeldAt.IsZero():
		return false
	case h.HeldOn == node && now.Sub(h.HeldAt) >= d:
		return true
	case !seen.IsZero() && now.Sub(seen) >= d:
		return true
	}
	return now.Sub(h.HeldAt) >= d+RowClockAllowance
}

// Delegated reports whether the call waits on its delegates alone: put to
// agent delegates and not yet escalated to people.
func (h HeldCall) Delegated() bool { return h.Delegation != nil && h.Delegation.Escalated == nil }

// HeldOpen reports whether the run's held call still waits for its answer, on
// delegates or on people: from its recording, before the job parks, until an
// answer, an ending or the run's next job takes the question.
func (r PendingRun) HeldOpen() bool {
	return r.HeldCall != nil && r.HeldCall.LaunchID == r.LaunchID && r.Ending == nil &&
		!r.HeldCall.Calling() && r.AskedBy(r.LaunchID) &&
		(r.Status == StatusLaunching || r.Status == StatusRunning || slices.Contains(Awaiting, r.Status))
}

// HeldInFlight reports whether the run's held call has not yet come to an end
// another turn of its seat could act on: open ([PendingRun.HeldOpen]), or its
// job's completion claimed and not yet parked, or answered and the answer not
// yet taken by the resumed turn, or allowed and not yet made. A repeat of the
// same request joins it in any of those (ADR-0125): a second request raised
// then reached its delegate after the first was carried out. Taken and not made
// (a denial the turn read) or made, it is over, and the same ask after it is a
// new request. A person's answer is sealed, so only a delegates' allow is told
// from a denial between its taking and its making.
func (r PendingRun) HeldInFlight() bool {
	h := r.HeldCall
	switch {
	case h == nil || r.Ending != nil || h.LaunchID != r.LaunchID:
		return false
	case h.Calling():
		return !h.Made()
	case r.Answer != nil:
		return !r.Answer.Taken() || h.delegatesAllowed()
	}
	// COLLECTED, NOT YET PARKED: the completion's claim holds the row resumed
	// until the park marks it awaiting and wakes whoever decides it.
	return r.HeldOpen() || (r.Status == StatusResumed && r.AskedBy(r.LaunchID))
}

// delegatesAllowed reports whether the call's delegates decided it and allowed
// it, never put to people: their verdicts are on the row in the clear.
func (h HeldCall) delegatesAllowed() bool {
	d := h.Delegation
	if d == nil || d.Escalated != nil {
		return false
	}
	given := make([]delegated.Given, 0, len(d.Given))
	for _, v := range d.Given {
		given = append(given, delegated.Given{By: v.By, Verdict: delegated.Verdict(v.Verdict),
			Confidence: delegated.Confidence(v.Confidence)})
	}
	o := delegated.Combine(d.Delegates, given)
	return o.Decided && o.Verdict == delegated.Allow
}

// HeldDelegation is how a held call was put to agent delegates (ADR-0125):
// the rule, the seats, what a verdict binds to, what each answered, and the
// escalation that put it to people.
type HeldDelegation struct {
	// Rule is the 1-based number of the rule that routed the call, as it
	// stood when the call was held, and Delegates its seats then: one, or
	// a panel of two that must both allow.
	Rule      int      `json:"rule"`
	Delegates []string `json:"delegates"`
	// Digest is the SHA-256 of the call's arguments, which a verdict names
	// so it is of the call its delegate was shown.
	Digest string `json:"digest"`
	// Given is each delegate's answer, in the order given.
	Given []HeldVerdict `json:"given,omitempty"`
	// Escalated is set once the call is put to people.
	Escalated *HeldEscalation `json:"escalated,omitempty"`
}

// HeldVerdict is one delegate's answer to a held call: its verdict and
// confidence, and its own words, which people read before the call itself.
type HeldVerdict struct {
	By         string    `json:"by"`
	Verdict    string    `json:"verdict"`
	Confidence string    `json:"confidence"`
	Reason     string    `json:"reason"`
	Findings   []string  `json:"findings,omitempty"`
	At         time.Time `json:"at"`
}

// Redacted is the answer with a secret its reason or findings carry replaced
// by its marker, a copy: redacted where it is recorded, since every reader of
// the row reads them. Who gave it, the verdict and the confidence are closed
// words or a seat's name.
func (v HeldVerdict) Redacted() HeldVerdict {
	v.Reason = redact.Secrets(v.Reason)
	if v.Findings != nil {
		findings := make([]string, len(v.Findings))
		for i, f := range v.Findings {
			findings[i] = redact.Secrets(f)
		}
		v.Findings = findings
	}
	return v
}

// HeldEscalation is why a delegated call went to people: the engine's closed
// words, and the sentences people read beside the delegates' answers.
type HeldEscalation struct {
	Overruled []string  `json:"overruled,omitempty"`
	Engine    []string  `json:"engine,omitempty"`
	At        time.Time `json:"at"`
}

// The bounds of a delegate's answer on the row, the verdict tool's own.
const (
	MaxReviewReasonRunes  = 500
	MaxReviewFindings     = 8
	MaxReviewFindingRunes = 300
	maxEscalationReasons  = 16
	maxEscalationRunes    = 300
)

// validate holds a delegate's answer to its bounds.
func (v HeldVerdict) validate() error {
	switch {
	case v.By == "" || v.At.IsZero():
		return errors.New("a delegate's answer names who gave it and when")
	case !delegated.Verdict(v.Verdict).Valid() || !delegated.Confidence(v.Confidence).Valid():
		return fmt.Errorf("a delegate's answer is %s with confidence %s", words.Or(delegated.Verdicts()),
			words.Or(delegated.Confidences()))
	case utf8.RuneCountInString(v.Reason) > MaxReviewReasonRunes:
		return fmt.Errorf("a delegate's reason is at most %d characters", MaxReviewReasonRunes)
	case len(v.Findings) > MaxReviewFindings:
		return fmt.Errorf("a delegate's answer carries at most %d findings", MaxReviewFindings)
	}
	for _, f := range v.Findings {
		if utf8.RuneCountInString(f) > MaxReviewFindingRunes {
			return fmt.Errorf("a finding is at most %d characters", MaxReviewFindingRunes)
		}
	}
	return nil
}

// validate holds an escalation to its bounds.
func (e HeldEscalation) validate() error {
	switch {
	case e.At.IsZero():
		return errors.New("an escalation names when it was made")
	case len(e.Engine) > maxEscalationReasons || len(e.Overruled) > maxEscalationReasons:
		return fmt.Errorf("an escalation carries at most %d reasons", maxEscalationReasons)
	}
	for _, said := range e.Engine {
		if utf8.RuneCountInString(said) > maxEscalationRunes {
			return fmt.Errorf("an escalation's reason is at most %d characters", maxEscalationRunes)
		}
	}
	return nil
}

// Made reports whether the call's result is recorded.
func (h HeldCall) Made() bool { return !h.MadeAt.IsZero() }

// Ref is the call as an announcement names it, or nil for no held call: what
// it is and who may approve it, never its arguments, result or log.
func (h *HeldCall) Ref() *types.HeldCallRef {
	if h == nil {
		return nil
	}
	return &types.HeldCallRef{
		ID: h.ID, Server: h.Server, Tool: h.Tool, Raw: h.Raw, Digest: h.Digest, HeldAt: h.HeldAt,
	}
}

// MarshalJSON writes the section with the keys a later build added to it.
func (h HeldCall) MarshalJSON() ([]byte, error) {
	type plain HeldCall
	return marshalCarrying(plain(h), wireKeys(reflect.TypeFor[HeldCall]()), h.Extra)
}

// UnmarshalJSON reads the section and keeps the keys this build does not know.
func (h *HeldCall) UnmarshalJSON(raw []byte) error {
	type plain HeldCall
	p := plain(*h)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[HeldCall]()))
	if err != nil {
		return err
	}
	*h = HeldCall(p)
	h.Extra = extra
	return nil
}

// validate is the held call's own contract, beside the ask it rides with: it
// names the call, its server and tool, and carries sealed arguments, and the
// ask's decision is the one every held call is asked as.
func (h HeldCall) validate(decided *decision.Decision) []error {
	var found []error
	for _, field := range []struct{ name, value string }{
		{"held_call.id", h.ID}, {"held_call.server", h.Server}, {"held_call.tool", h.Tool},
		{"held_call.raw", h.Raw}, {"held_call.arguments", h.Arguments},
	} {
		if strings.TrimSpace(field.value) == "" {
			found = append(found, fieldRefusal(field.name, "is empty, and a held call names "+
				"the call, its server and tool, and carries its sealed arguments"))
		}
	}
	if h.Arguments != "" && !secrets.IsEnvelope(h.Arguments) {
		found = append(found, fieldRefusal("held_call.arguments", "is not sealed, and a held "+
			"call's arguments are never on the row in the clear"))
	}
	if h.Calling() || h.Made() || h.Result != "" {
		found = append(found, fieldRefusal("held_call", "is already being made, and a call is "+
			"held before anything makes it"))
	}
	if want := ChoicesOf(h); decided == nil || !slices.Equal(decided.OptionIDs(), want) {
		found = append(found, fieldRefusal("decision", "is not the one a held call is asked "+
			"as: this held call offers "+words.And(want)))
	}
	if h.Binding != "" && (!h.Fixed || len(h.Binding) > maxHeldBindingBytes) {
		found = append(found, fieldRefusal("held_call.binding", "is set on a call no tool binds, "+
			"or is longer than a digest"))
	}
	if h.Subject != "" && (!h.Fixed || len(h.Subject) != maxHeldBindingBytes) {
		found = append(found, fieldRefusal("held_call.subject", "is set on a call no tool names "+
			"the subject of, or is not a digest"))
	}
	if h.Act != "" && !delegated.Kind(h.Act).Valid() {
		found = append(found, fieldRefusal("held_call.act", "is not an act this build knows"))
	}
	if d := h.Delegation; d != nil {
		switch {
		case !delegated.Kind(h.Act).Delegable():
			found = append(found, fieldRefusal("held_call.delegation", "is set on a call whose act is "+
				"never delegated"))
		case d.Rule < 1 || len(d.Delegates) < 1 || len(d.Delegates) > 2 || len(d.Digest) != maxHeldBindingBytes:
			found = append(found, fieldRefusal("held_call.delegation", "names no rule, one or two "+
				"delegates, or the digest a verdict binds to"))
		case len(d.Given) > 0 || d.Escalated != nil:
			found = append(found, fieldRefusal("held_call.delegation", "carries an answer or an "+
				"escalation on a call being held, and both come after it is held"))
		}
	}
	if h.Approvers != "" && (!slices.Contains(heldApprovers, h.Approvers) || !h.Fixed) {
		found = append(found, fieldRefusal("held_call.approvers", "names approvers this build does "+
			"not know, or names them on a call that may be allowed always"))
	}
	growthAct := slices.Contains([]delegated.Kind{delegated.KindAdSpend, delegated.KindPublicPost,
		delegated.KindAccountAdmin}, delegated.Kind(h.Act))
	switch {
	case growthAct != (h.Approvers == HeldApproversAdmin):
		found = append(found, fieldRefusal("held_call.approvers", "is admin exactly for a growth act "+
			"(ad_spend, public_post, account_admin), which an administrator alone answers"))
	case growthAct && !h.LapsesAt.Equal(h.HeldAt.Add(HeldLapse)):
		found = append(found, fieldRefusal("held_call.lapses_at", "is not a day after held_at, and a "+
			"growth act lapses a day after it is held"))
	case !growthAct && !h.LapsesAt.IsZero():
		found = append(found, fieldRefusal("held_call.lapses_at", "is set on a call that waits until "+
			"answered"))
	}
	return found
}

// CallSeal seals a held call's arguments and its result onto its run's row and
// opens them again: AEAD under the fleet keyring ([secrets.Cipher]), each
// bound to the run, the job, the call and which of the two it is, so a value
// sealed for one call never opens as another's, nor an argument as a result.
//
// NOT [AnswerSeal], which signs and does not encrypt: an answer is an
// instruction the row must not forge, while a call's arguments and result are
// words the row must not show.
type CallSeal struct{ cipher secrets.Cipher }

// ErrNoCallSeal is a held call with nothing to seal it under: a node built
// without the fleet keyring's cipher.
var ErrNoCallSeal = errors.New("sandbox: this node has no keyring cipher to seal a held call under")

// NewCallSeal is a seal over cipher, or nil without one.
func NewCallSeal(cipher secrets.Cipher) *CallSeal {
	if cipher == nil {
		return nil
	}
	return &CallSeal{cipher: cipher}
}

// heldAAD binds a sealed part of a held call to where it belongs.
func heldAAD(part, turnID string, h HeldCall) string {
	return "sandbox/held_call/" + part + "|" + turnID + "|" + h.LaunchID + "|" + h.ID + "|" +
		h.Server + "|" + h.Raw + "|" + h.Digest
}

// SealArguments seals a call's canonical arguments for the held call h of run
// turnID (its ID, launch, server, raw name and digest set).
func (s *CallSeal) SealArguments(turnID string, h HeldCall, args map[string]any) (string, error) {
	if s == nil {
		return "", ErrNoCallSeal
	}
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("sandbox: encoding the arguments of held call %s: %w", h.ID, err)
	}
	return s.cipher.Encrypt(string(body), heldAAD("arguments", turnID, h))
}

// OpenArguments opens held call h's arguments, as run turnID's row carries it,
// redacted as values ([redact.Args]): what every reader is shown but those
// [CallSeal.OpenRawArguments] names.
func (s *CallSeal) OpenArguments(turnID string, h HeldCall) (map[string]any, error) {
	args, err := s.OpenRawArguments(turnID, h)
	if err != nil {
		return nil, err
	}
	return redact.Args(args), nil
}

// OpenArgumentsFor opens run's held call's arguments for one reader, by THE ONE
// RULE every view that serves them answers by: as the call would send them,
// with their digest ([HeldArgumentsDigest]), only to a reader who may answer
// the call (mayAnswer, decided as internal/runanswer's May decides it) and
// only while it waits ([PendingRun.HeldOpen]), who consents to those bytes.
// Every other reader, and every reader once it is answered, made or ended,
// gets them redacted ([CallSeal.OpenArguments]) and an empty digest: nothing
// is answered from them, and a digest of the raw bytes beside their redacted
// form would let a short secret in them be guessed.
func (s *CallSeal) OpenArgumentsFor(run PendingRun, mayAnswer bool) (map[string]any, string, error) {
	if run.HeldCall == nil {
		return nil, "", fmt.Errorf("sandbox: run %s holds no call to open the arguments of", run.TurnID)
	}
	if !mayAnswer || !run.HeldOpen() {
		args, err := s.OpenArguments(run.TurnID, *run.HeldCall)
		return args, "", err
	}
	args, err := s.OpenRawArguments(run.TurnID, *run.HeldCall)
	if err != nil {
		return nil, "", err
	}
	digest, err := HeldArgumentsDigest(args)
	if err != nil {
		return nil, "", err
	}
	return args, digest, nil
}

// OpenRawArguments opens held call h's arguments as the call would send them.
// ONLY WHO CONSENTS TO OR MAKES THE PENDING CALL reads these: a person shown
// the call by [CallSeal.OpenArgumentsFor]'s rule, which is every view's; Allow
// once's digest check, which compares and serves nothing; the delegate
// deciding it; and the engine making, escalating, settling or digesting it.
// Every other reader is shown [CallSeal.OpenArguments], so a new reader is
// redacted unless it asks, and `git grep RawArguments` is the list of who
// reads them raw.
func (s *CallSeal) OpenRawArguments(turnID string, h HeldCall) (map[string]any, error) {
	if s == nil {
		return nil, ErrNoCallSeal
	}
	plain, err := s.cipher.Decrypt(h.Arguments, heldAAD("arguments", turnID, h))
	if err != nil {
		return nil, fmt.Errorf("sandbox: opening the arguments of held call %s: %w", h.ID, err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(plain), &args); err != nil {
		return nil, fmt.Errorf("sandbox: the arguments of held call %s do not decode: %w", h.ID, err)
	}
	return args, nil
}

// HeldArgumentsDigest is the SHA-256, in hex, of a held call's arguments as
// JSON with sorted keys: what an approver who was shown them names, a person
// at `POST /mcp-approvals/{id}` and the SSH reviewer alike.
func HeldArgumentsDigest(args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("sandbox: digesting a held call's arguments: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// MaxHeldResultBytes bounds a held call's result as handed to the run that
// carries the work on, cut past it with the cut said (internal/textcut.Within).
//
// An answer's page ([MaxAnswerTextBytes]), since it travels where an answer
// does: sealed on the row, read whole on every status flip and active-run
// listing, then into the run's next prompt, which its harness re-sends with
// every request. A longer result the run reads by calling the tool again.
const MaxHeldResultBytes = MaxAskContextBytes

// HeldResult is what a held call returned once a person let it be made: its
// output as the run reads it, and whether the tool reported a failure.
type HeldResult struct {
	Output string `json:"output"`
	Failed bool   `json:"failed"`
}

// SealResult seals what held call h of run turnID returned.
func (s *CallSeal) SealResult(turnID string, h HeldCall, result HeldResult) (string, error) {
	if s == nil {
		return "", ErrNoCallSeal
	}
	body, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("sandbox: encoding the result of held call %s: %w", h.ID, err)
	}
	return s.cipher.Encrypt(string(body), heldAAD("result", turnID, h))
}

// HeldKept is a held call's result as its row sealed it ([HeldCall.Result]),
// with the fields its seal binds ([heldAAD]): what a lead that told the result
// keeps ([LeadHeld.Result]) once the next job's reset clears the held call.
//
// KEPT SEALED, NEVER IN THE CLEAR, since a lead is signed, not encrypted
// ([LeadSeal]); kept with those fields so a later node opens it as that call's
// result alone ([CallSeal.OpenKept]), or tells it as one it cannot read.
type HeldKept struct {
	Result   string `json:"result"`
	LaunchID string `json:"launch_id"`
	ID       string `json:"id"`
	Server   string `json:"server"`
	Raw      string `json:"raw"`
	Digest   string `json:"digest"`
}

// Kept is h's result as its row seals it, with the fields its seal binds, nil
// where none is recorded: a call not made, or one whose node stopped first.
func (h HeldCall) Kept() *HeldKept {
	if h.Result == "" {
		return nil
	}
	return &HeldKept{Result: h.Result, LaunchID: h.LaunchID, ID: h.ID, Server: h.Server, Raw: h.Raw,
		Digest: h.Digest}
}

// KeptAs is result, as [CallSeal.SealResult] sealed it for held call h, kept
// with the fields its seal binds ([HeldKept]).
func KeptAs(h HeldCall, result string) HeldKept {
	return HeldKept{Result: result, LaunchID: h.LaunchID, ID: h.ID, Server: h.Server, Raw: h.Raw,
		Digest: h.Digest}
}

// OpenKept opens what a held call of run turnID returned, as a lead kept it
// ([HeldKept]): under the very binding [CallSeal.SealResult] sealed it with, so
// a result kept for one call never opens as another's.
func (s *CallSeal) OpenKept(turnID string, kept HeldKept) (HeldResult, error) {
	return s.OpenResult(turnID, HeldCall{ID: kept.ID, LaunchID: kept.LaunchID, Server: kept.Server,
		Raw: kept.Raw, Digest: kept.Digest, Result: kept.Result})
}

// OpenResult opens what held call h of run turnID returned.
func (s *CallSeal) OpenResult(turnID string, h HeldCall) (HeldResult, error) {
	if s == nil {
		return HeldResult{}, ErrNoCallSeal
	}
	plain, err := s.cipher.Decrypt(h.Result, heldAAD("result", turnID, h))
	if err != nil {
		return HeldResult{}, fmt.Errorf("sandbox: opening the result of held call %s: %w", h.ID, err)
	}
	var result HeldResult
	if err := json.Unmarshal([]byte(plain), &result); err != nil {
		return HeldResult{}, fmt.Errorf("sandbox: the result of held call %s does not decode: %w", h.ID, err)
	}
	return result, nil
}
