package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

// A person's answer to a parked run is sealed onto its row. The resumed turn
// hands the text to the harness as that person's words ([AskedRun]), so under
// ADR-0031 it is an instruction, not bookkeeping: sealed by the node that
// records it ([Coordinator.record]) in [AnswerSealDomain], and opened by every
// reader before any of it is used ([Coordinator.attemptOwed]). The seal binds
// the words, answerer and route to the turn, the launch whose question they
// answer and the deliveries, so it opens nowhere else. Words and answerer live
// ONLY in the seal: a clear copy would one day be read unopened.
// [RecordedAnswer.Events] ride beside it, never read as the answer. An unknown
// key is a fact about THIS NODE: the answer waits ([ConditionAnswerKey]). A
// seal that does not open is a fact about the ROW: let go, handed to no run and
// back to nobody, and the run waits on its question again ([Coordinator.dropUnopened]).

// AnswerSealDomain is the key domain a person's answer to a parked run is
// sealed in: its own, so that no frame from a state log or another channel
// opens as an answer, and no sealed answer as anything else.
const AnswerSealDomain = "seat.run.answer"

// ConditionAnswerKey is what an owed answer whose seal names a key this node
// does not hold waits on. Nothing signals it: Tier A's keyring changes only
// with a restart, so only [answerWaitBackstop] re-checks it (re-stating the
// keys this node holds), and the seat moving to a node that holds the key
// ends it.
const ConditionAnswerKey Condition = "answer_key"

// ErrAnswerKeyUnknown is an answer sealed under a key this node does not hold.
var ErrAnswerKeyUnknown = errors.New("sandbox: the answer is sealed under a key this node does not hold")

// ErrAnswerUnopened is an answer whose seal does not open under a key this node
// holds, or opens as an answer to another run's question.
var ErrAnswerUnopened = errors.New("sandbox: the answer's seal does not open as an answer to this run")

// AnswerSeal seals a person's answer onto the run it answers and opens it again
// for a reader. SAFE FOR CONCURRENT USE: both halves are immutable once built.
type AnswerSeal struct {
	signer   *recordseal.Signer
	verifier *recordseal.Verifier
}

// NewAnswerSeal builds both halves over the fleet keyring every state-log
// record is signed under, refusing one that could not seal (no active key) or
// open (no key at all), by the field to set.
func NewAnswerSeal(ring recordseal.Keyring) (*AnswerSeal, error) {
	signer, err := recordseal.NewSigner(AnswerSealDomain, ring)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the seal on answers to a parked run: %w", err)
	}
	verifier, err := recordseal.NewVerifier(AnswerSealDomain, ring)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the seal on answers to a parked run: %w", err)
	}
	return &AnswerSeal{signer: signer, verifier: verifier}, nil
}

// Domain is the key domain answers are sealed in, for the gate that keeps
// every instruction channel's and state log's domains apart.
func (s *AnswerSeal) Domain() string { return AnswerSealDomain }

// KeyIDs is every key id this node opens answers under, sorted, for the line
// that says why an answer waits.
func (s *AnswerSeal) KeyIDs() []string { return s.verifier.KeyIDs() }

// AnswerWords is what a person's answer says and who gave it: what the seal
// carries, and what a reader is handed once it opens.
type AnswerWords struct {
	// Text is the person's own words, redacted where they were recorded
	// ([Reply.answerOf], [answerGivenOf]); empty for an answer that is a
	// choice alone.
	Text string

	// Choice is the option of the run's question the person chose, by id,
	// or empty for an answer in words (ADR-0068). The resumed run reads the
	// two together ([AnswerText]), and the answer's record announces both.
	Choice string

	// Via is the route it came by; [RecordedAnswer.Via], the store's clear
	// copy, must agree with it.
	Via types.AnswerVia

	// By, ByKind and OperatorID are who gave it — author, kind of author and
	// credential, as iam.ActorFor splits a principal and the answer's record
	// states it ([types.ExecutorAnswered]). A chat reply carries the
	// envelope's sender, a person, and no credential: the chat surface
	// vouched for them ([chatAnswerer]). An answer by turn carries the bound
	// seat, else the login, and the credential it was given under.
	By, ByKind, OperatorID string

	// Label is who wrote Text, as the delivery's fence labelled it for the
	// run's seat ([Reply.Label]); the run reads a chat reply fenced under it
	// ([AskedRun]). Nil for an answer by turn, the person's own instruction,
	// defused rather than fenced (ADR-0068), and for words no surface
	// labelled. SEALED WITH THE WORDS, so who the run is told wrote them
	// comes out of the seal too.
	Label *provenance.Label

	// Refusal is set where what was recorded is the REFUSAL of a reply too
	// large to be an answer ([PendingRun.AnswerRefusal]): its deliveries and
	// figures, which the resumed run is told, and none of its words (Text,
	// Choice and Label are empty). [AnswerSeal.Open] holds it to the refusal
	// the row records in the clear, so a refusal never reads as a wordless
	// answer, nor an answer as a refusal (ADR-0114).
	Refusal *AnswerRefusal
}

// attribution is who the resumed turn is told gave this answer: an answer by
// turn's author, and nobody for a chat reply, whose sender is already in the
// conversation the resumed turn reports back to.
func (w AnswerWords) attribution() string {
	if w.Via != types.AnswerViaOperator {
		return ""
	}
	return w.By
}

// answerer is who gave this answer, as its record announces them
// ([Coordinator.announceAnswered]).
func (w AnswerWords) answerer() answerer {
	return answerer{by: w.By, kind: w.ByKind, operatorID: w.OperatorID,
		choice: w.Choice, said: w.Text}
}

// sealedAnswer is the body the seal covers.
//
// A FIXED STRUCT AND NO omitempty, so the bytes are a function of the values
// alone: every field is present, in this order, on every seal.
type sealedAnswer struct {
	TurnID     string            `json:"turn_id"`
	LaunchID   string            `json:"launch_id"`
	EventIDs   []string          `json:"event_ids"`
	Via        types.AnswerVia   `json:"via"`
	By         string            `json:"by"`
	ByKind     string            `json:"by_kind"`
	OperatorID string            `json:"operator_id"`
	Text       string            `json:"text"`
	Choice     string            `json:"choice"`
	Label      *provenance.Label `json:"label"`
	Refusal    *AnswerRefusal    `json:"refusal"`
}

// Seal is answer recording words as the answer to the question run's job
// asked: sealed to run's turn, its launch and the deliveries answer names,
// under this node's active key, with [RecordedAnswer.Via] set from words so
// the clear and sealed copies agree. Exported for a case that seeds a
// recorded answer as [Coordinator.record] would.
func (s *AnswerSeal) Seal(run PendingRun, words AnswerWords, answer RecordedAnswer) (RecordedAnswer, error) {
	body, err := json.Marshal(sealedAnswer{
		TurnID: run.TurnID, LaunchID: run.LaunchID, EventIDs: answer.EventIDs, Via: words.Via,
		By: words.By, ByKind: words.ByKind, OperatorID: words.OperatorID, Text: words.Text,
		Choice: words.Choice, Label: words.Label, Refusal: words.Refusal,
	})
	if err != nil {
		return RecordedAnswer{}, fmt.Errorf("sandbox: encoding the answer to run %s: %w", run.TurnID, err)
	}
	answer.Via, answer.Sealed = words.Via, s.signer.Seal(body)
	return answer, nil
}

// Open is the answer recorded on run, once its seal opens as the answer to the
// question the row's own job asked: [ErrAnswerKeyUnknown] under a key this
// node does not hold, and [ErrAnswerUnopened] where it does not open — not a
// frame, a failing MAC, or a body bound to another run, question, deliveries
// or route than the row names.
//
// THE ROW SUPPLIES ONLY WHERE THE ANSWER MUST BELONG, never what it says:
// everything a reader is handed comes out of the seal.
func (s *AnswerSeal) Open(run PendingRun) (AnswerWords, error) {
	if run.Answer == nil {
		return AnswerWords{}, fmt.Errorf("%w: run %s records no answer", ErrAnswerUnopened, run.TurnID)
	}
	answer := *run.Answer
	body, verdict := s.verifier.Open(answer.Sealed)
	switch verdict {
	case recordseal.Verified:
	case recordseal.KeyUnknown:
		return AnswerWords{}, fmt.Errorf("%w (it holds %s) — add the key the fleet sealed it "+
			"under to secrets.keys", ErrAnswerKeyUnknown, words.And(s.verifier.KeyIDs()))
	default:
		return AnswerWords{}, fmt.Errorf("%w: the frame does not verify under the key it names",
			ErrAnswerUnopened)
	}
	var sealed sealedAnswer
	if err := json.Unmarshal(body, &sealed); err != nil {
		return AnswerWords{}, fmt.Errorf("%w: the sealed body does not decode: %w", ErrAnswerUnopened, err)
	}
	switch {
	case sealed.TurnID != run.TurnID:
		return AnswerWords{}, fmt.Errorf("%w: it was sealed for run %q", ErrAnswerUnopened, sealed.TurnID)
	case sealed.LaunchID != run.LaunchID:
		return AnswerWords{}, fmt.Errorf("%w: it was sealed for the question launch %q asked, "+
			"and the run's job is %q", ErrAnswerUnopened, sealed.LaunchID, run.LaunchID)
	case !slices.Equal(sealed.EventIDs, answer.EventIDs):
		return AnswerWords{}, fmt.Errorf("%w: it was sealed for deliveries %s, and the row names %s",
			ErrAnswerUnopened, words.And(sealed.EventIDs), words.And(answer.EventIDs))
	case sealed.Via != answer.Via:
		return AnswerWords{}, fmt.Errorf("%w: it was sealed as come by %q, and the row says %q",
			ErrAnswerUnopened, sealed.Via, answer.Via)
	case !sealed.Refusal.equal(run.refusal()):
		return AnswerWords{}, fmt.Errorf("%w: it was sealed as %s, and the row records %s",
			ErrAnswerUnopened, refusalWord(sealed.Refusal), refusalWord(run.refusal()))
	}
	return AnswerWords{Text: sealed.Text, Choice: sealed.Choice, Via: sealed.Via, By: sealed.By,
		ByKind: sealed.ByKind, OperatorID: sealed.OperatorID, Label: sealed.Label,
		Refusal: sealed.Refusal}, nil
}

// refusalWord names what a seal or a row holds, for the error that says the two
// disagree.
func refusalWord(refusal *AnswerRefusal) string {
	if refusal == nil {
		return "an answer"
	}
	return fmt.Sprintf("the refusal of a reply of %d bytes", refusal.Bytes)
}
