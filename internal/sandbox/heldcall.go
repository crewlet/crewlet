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
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
)

// A TOOL CALL THAT NEEDS APPROVAL PARKS ITS RUN IN THE SHAPE A RUN'S ASK DOES:
// ADR-0077.
//
// A seat's executor that calls an MCP tool at Needs approval (an `mcp_servers`
// entry's `tools` level `approve`, or a tool its hints start there) does not
// run it. Its bridge records the call on the run's row as the job's question
// ([PendingStore.RecordAsk], an [Ask] carrying a [HeldCall]), tells the harness
// to end its turn, and the collection parks the run on it with the seat freed,
// exactly as it parks on `ask_and_wait` (ADR-0068): one question per job, no
// replay of anything the job did after it, and an answer pinned to the job
// that asked. A person answers it with `POST /mcp-approvals/{id}`, choosing
// one of the three options every held call offers ([HeldChoices]); the run's
// resume makes the call for `once` and `always` and hands the result to the
// job that carries the work on, and answers `forbidden` for `deny`.
//
// # Who may answer it, and how
//
// Only the route's people (an administrator, or a lead of the seat's unit, a
// person and never the assistant), through the route alone. A chat reply on
// the conversation the run was launched from never answers a held call
// ([ConversationRef.Answers]), and `answer_executor` refuses one: either would
// let anybody the run reports to approve a call their own grant does not
// reach. The seat holding the run spends an answer that does not name the held
// call by its id ([HeldCall.ID], [types.ExecutorAnswerGiven.Approval]), and
// one that names it for a question that is not one.
//
// # The arguments are sealed, and so is the result
//
// What the call would send is the run's own business and often a credential's
// worth: an issue body, a query, an address. So the arguments ride the row
// encrypted under the fleet keyring ([CallSeal], AEAD, bound to the run, the
// job and the call), opened only for a person who may answer it and for the
// resume that makes the call. What the call returned is sealed beside them
// once it is made, for the job that carries the work on.
//
// # The call is made at most once
//
// A resume can be attempted more than once for one answer: a failed attempt
// gives its claim back, and a node that took the seat over revives an answer
// no resume holds. So the resume marks the call on the row as being made
// before it makes it ([PendingStore.MarkHeldCall]), under the claim's fence,
// and records its result after ([PendingStore.RecordHeldResult]). Neither is
// cleared by anything but the next job's opening. A resume that finds the call
// marked and its result recorded hands the job that result; one that finds it
// marked and no result says so, and the call is never made again, since it may
// or may not have taken effect.
//
// IT FINDS THE MARK ON THE ROW ITS CLAIM READ ([HeldResume.Call]), and answers
// from it before it holds the call to the tools and skills it carries the run
// on with: those decide whether a call nobody has made may be made now, and a
// call an earlier attempt made — its relaunch refused after it, say, by the
// floor of the session it continued — is past deciding. A tool blocked or
// changed, or a skill made required, while that attempt's answer waited to be
// tried again must not tell the run a call it made was never made.
//
// # The call it makes is a call of its round
//
// The resume makes the call on the engine host, not the job over its bridge,
// and the call is recorded beside every call the round's jobs made ([HeldMade],
// [BridgeCall.Approved]): answered as the tool answered it, its arguments and
// output left sealed on the held call. Every reader of whether the turn
// replied reads the round's log — the collection's own delivery check, a
// stop's or a loss's account of the turn, a later round's earlier calls — and
// a call recorded nowhere but in the sealed result read as a reply that never
// went out: a person told the agent did not answer them, and the next turn
// invited to post it again.
//
// The row and the log are two keys, written in no one step
// ([PendingStore.RecordHeldResult]): the result goes on the row with the
// call's entry ([HeldCall.Call]), and the call is appended to the log after
// it, once by the held call's id ([BridgeCall.HeldID]). An append that never
// landed loses nothing: an ending reads the call off the row
// ([PendingStore.TurnLog]), and the reset that opens the next job carries it
// to that job's opening on the row ([PendingRun.OwedCall]), which appends it.

// The three answers every held call offers, as a [decision.Option] id the
// person's answer names ([HeldDecision]).
const (
	// HeldOnce makes the call, this once.
	HeldOnce = "once"
	// HeldAlways makes the call and sets the tool to Always allow on its
	// server's entry, so the next call runs without asking. The route
	// writes the level before it answers, as a configuration write at a
	// recent step-up (internal/api/mcpapi).
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
// ([AskedRun.Held]): the call as the claimed row recorded it, the choice the
// person made, and the two writes the resume makes it under, on the claim's
// terms and fence ([PendingStore.MarkHeldCall],
// [PendingStore.RecordHeldResult]).
type HeldResume struct {
	// TurnID is the run, which every seal of the call is bound to.
	TurnID string
	// Call is the held call as the claim read it: marked, and its result
	// recorded, where an earlier attempt at the same answer got that far,
	// which the resume answers from before it asks anything else of the
	// call, since nothing clears a mark while the claim holds the row.
	Call HeldCall
	// Choice is the answer: [HeldOnce], [HeldAlways] or [HeldDeny].
	Choice string
	// By is who answered, as the answer's own record names them.
	By string
	// Mark records that the call is being made, and answers the held call
	// as the row then holds it and whether this mark is the first: false
	// means some attempt marked it before, and it is not made again.
	Mark func(ctx context.Context) (HeldCall, bool, error)
	// Record records what the call came to: its result, sealed, and the
	// call as its round's log keeps it.
	Record func(ctx context.Context, made HeldMade) error
}

// HeldMade is what a held call came to once a person let it be made, as its
// run's row records it ([PendingStore.RecordHeldResult]).
type HeldMade struct {
	// Result is what the call returned, sealed ([CallSeal.SealResult]), for
	// the job that carries the work on.
	Result string

	// Call is the call as its round's log keeps it, in the shape every call
	// a job made over its bridge takes ([BridgeLog.Calls]): whether
	// it failed, and who answered it — its origin, server, source and a
	// refusal's class — as the surface that made it said. The store names
	// it, places it, marks it approved and names its held call
	// ([BridgeCall.HeldID]); its arguments and output are never written
	// beside it, since they ride the row sealed.
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
// three answers, and the evidence (the work item the run was on, where it was
// on one). Role approver, since the answer decides the call. No
// recommendation: the engine asks on the model's behalf and has no view.
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
	// ID names the held call: what `POST /mcp-approvals/{id}` answers it
	// by, and what the answer the seat's holder records must name. Minted
	// when the call is held; a uuid.
	ID string `json:"id"`

	// LaunchID is the job that made the call, which the question is
	// pinned to as every question is.
	LaunchID string `json:"launch_id"`

	// Server is the MCP server's configured name; Tool the tool's name as
	// the run called it (the server's `tool_prefix` applied), Raw the
	// server's own name for it, which a level is set by; Digest the
	// definition the run was offered and a person approves
	// (internal/mcp's Definition.Digest), which the resume holds the call
	// to.
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
	// or an administrator: [HeldApproversSSH] for an SSH operate command.
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

	// Delegation is the rule that put the call to agent delegates before any
	// person (ADR-0125), nil for a call put to people at once. While it is
	// not escalated the call waits on its delegates alone: it is put to
	// nobody's Inbox.
	Delegation *HeldDelegation `json:"delegation,omitempty"`

	// HeldAt is when the call was held.
	HeldAt time.Time `json:"held_at"`

	// CallingAt is when a resume began making the call
	// ([PendingStore.MarkHeldCall]): zero until then, and from then on the
	// reason no resume makes it again.
	CallingAt time.Time `json:"calling_at,omitzero"`

	// Result is what the call returned, sealed ([CallSeal.SealResult]),
	// and MadeAt when it was recorded; both empty until the call returns,
	// and empty for good where the node making it stopped first.
	Result string    `json:"result,omitempty"`
	MadeAt time.Time `json:"made_at,omitzero"`

	// Call is the call as its round's log keeps it, written with the
	// result ([PendingStore.RecordHeldResult]): what the log is appended
	// after the result, and what the next job's opening appends where that
	// append never landed ([OwedCall]). Its arguments and output are never
	// here.
	Call *BridgeCall `json:"call,omitempty"`

	// Extra is every key of the section this build does not know: see
	// wirekeys.go.
	Extra map[string]json.RawMessage `json:"-"`
}

// Calling reports whether a resume has begun making the call.
func (h HeldCall) Calling() bool { return !h.CallingAt.IsZero() }

// Delegated reports whether the call waits on its delegates alone: put to
// agent delegates and not yet escalated to people.
func (h HeldCall) Delegated() bool { return h.Delegation != nil && h.Delegation.Escalated == nil }

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
	if h.Approvers != "" && (h.Approvers != HeldApproversSSH || !h.Fixed) {
		found = append(found, fieldRefusal("held_call.approvers", "names approvers this build does "+
			"not know, or names them on a call that may be allowed always"))
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

// OpenArguments opens held call h's arguments, as run turnID's row carries it.
func (s *CallSeal) OpenArguments(turnID string, h HeldCall) (map[string]any, error) {
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

// MaxHeldResultBytes bounds what a held call's result hands the run that
// carries the work on, cut past it with the cut said
// (internal/textcut.Within).
//
// THE PAGE AN ANSWER IS HELD TO ([MaxAnswerTextBytes], which is
// [MaxAskContextBytes]), because it travels where an answer does: sealed onto
// the run's row, read whole on every status flip and listing of active runs
// until the run is carried on, and then the next prompt of the run, which its
// harness re-sends with every request it makes from then on. A tool's answer
// past a page is one the run reads again by calling the tool, now that it may.
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
