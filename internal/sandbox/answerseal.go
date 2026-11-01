package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A person's answer to a parked run is sealed onto the run's row.
//
// # Why the answer is sealed and the rest of the row is not
//
// The coordination store decides between writers of a row by compare-and-set,
// and that is all ADR-0031 asks of it: almost everything on a run's row is the
// coordinator's own bookkeeping — statuses, leases, counts, the box — which it
// reads to decide what to do next. The answer is not bookkeeping. The resumed
// turn hands its text to the run's coding CLI as the person's words, and tells
// it who gave them ([AskedRun]); its announcement names that person too. That
// makes it what ADR-0031 calls an instruction, the same as a note to a running
// turn, so it is sealed under the fleet keyring like every other instruction
// channel: by the node that records it ([Coordinator.record]), in a key domain
// of its own ([AnswerSealDomain]), and opened by every reader before anything
// of it is used ([Coordinator.attemptOwed]).
//
// # What the seal binds
//
// The words and who gave them — the text, the route it came by and the three
// halves of the answerer iam.ActorFor makes of a principal — and WHERE THEY
// GO: the run's turn id, the launch whose question they answer and the
// deliveries they were made of. An answer sealed for one run's question opens
// as an answer to that question alone; carried onto another run's row, or onto
// the same run's next question, it does not open as an answer there.
//
// The answer's words and its answerer live ONLY inside the seal. A record that
// carried them in the clear beside it would have a copy that some reader, some
// day, used without opening, so the type keeps no such copy: [RecordedAnswer]
// has no text and no answerer, and the only way to them is [AnswerSeal.Open].
// The deliveries the answer was made of ride beside the seal as the messages
// they are ([RecordedAnswer.Events]) — the newest raises the working
// indicator, and a let-go hands them back to the seat the way each came, a chat
// reply to its inbox and an answer by turn to its control topic — and no reader
// takes the answer or its answerer from them.
//
// # Two failures, and only one of them waits
//
// The frame is internal/statelog's, judged by its three verdicts with their
// dispositions. A seal under a key this node does not hold is a fact about
// THIS NODE: the answer is kept, owed, and waits — logged with the keys this
// node holds — until a node that holds the key takes the seat, or this one is
// restarted with it ([ConditionAnswerKey]). A seal that does not open under a
// key it holds, or opens as an answer to some other question, is a fact about
// the ROW: no node will ever open it, so it is never handed to the run or
// attributed to anybody — it is logged at error and let go of, with nothing
// handed back, and the run waits on its question again
// ([Coordinator.dropUnopened]).

// AnswerSealDomain is the key domain a person's answer to a parked run is
// sealed in: its own, so that no frame from a state log or another channel
// opens as an answer, and no sealed answer as anything else.
const AnswerSealDomain = "seat.run.answer"

// ConditionAnswerKey is what an owed answer whose seal names a key this node
// does not hold waits on.
//
// THE ONE CONDITION THIS PACKAGE NAMES ITSELF, and nothing signals it: Tier A's
// keyring changes only with a restart, so this node never comes to hold the
// key while it runs. The wait is re-checked on [answerWaitBackstop] alone,
// which re-states the keys this node holds every five minutes for as long as
// the seat is here; the seat moving to a node that holds the key ends it.
const ConditionAnswerKey Condition = "answer_key"

// ErrAnswerKeyUnknown is an answer sealed under a key this node does not hold.
var ErrAnswerKeyUnknown = errors.New("sandbox: the answer is sealed under a key this node does not hold")

// ErrAnswerUnopened is an answer whose seal does not open under a key this node
// holds, or opens as an answer to another run's question.
var ErrAnswerUnopened = errors.New("sandbox: the answer's seal does not open as an answer to this run")

// AnswerSeal seals a person's answer onto the run it answers and opens it again
// for a reader. SAFE FOR CONCURRENT USE: both halves are immutable once built.
type AnswerSeal struct {
	signer   *statelog.Signer
	verifier *statelog.Verifier
}

// NewAnswerSeal builds both halves over the fleet keyring every state-log
// record is signed under, refusing one that could not seal (no active key) or
// open (no key at all), by the field to set.
func NewAnswerSeal(ring statelog.Keyring) (*AnswerSeal, error) {
	signer, err := statelog.NewSigner(AnswerSealDomain, ring)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the seal on answers to a parked run: %w", err)
	}
	verifier, err := statelog.NewVerifier(AnswerSealDomain, ring)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the seal on answers to a parked run: %w", err)
	}
	return &AnswerSeal{signer: signer, verifier: verifier}, nil
}

// Domain is the key domain answers are sealed in, for the gate that holds
// every instruction channel's domains apart from every other's and from every
// state log's.
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

	// Via is the route it came by ([RecordedAnswer.Via], which carries it in
	// the clear for the store and must agree with it).
	Via types.AnswerVia

	// By, ByKind and OperatorID are who gave it, in the three halves
	// iam.ActorFor makes of a principal — the author, what sort of author
	// that is, and the credential the answer came through — exactly as the
	// answer's own record states them ([types.ExecutorAnswered]). For a
	// chat reply, the sender as the delivery's envelope names them, a
	// person, and no credential: the chat surface vouched for the sender and
	// nothing of this engine's was presented ([chatAnswerer]). For an answer
	// by turn, the author is the seat of a principal the identity directory
	// binds to one and the login of anybody else, and the credential is the
	// one the answer was given under.
	By, ByKind, OperatorID string

	// Label is who wrote Text as the fence around the delivery's message
	// labelled it for the run's seat ([Reply.Label]): a chat reply's, which
	// the run reads inside a fence of its turn's token under it
	// ([AskedRun]). Nil for an answer by turn, which is the answering
	// person's own instruction to the run (ADR-0068), defused rather than
	// fenced, and for a reply whose words no surface labelled. SEALED WITH
	// THE WORDS, so what the run is told about who wrote them comes out of
	// the seal like everything else it is handed.
	Label *provenance.Label
}

// attribution is who the resumed turn is told gave this answer: the author an
// answer by turn names — the seat of a bound principal, the login of anybody
// else, as iam.ActorFor named them — and nobody for a chat reply, whose sender
// is already in the conversation the resumed turn reports back to.
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
}

// Seal is answer recording words as the answer to the question run's job
// asked: sealed to run's turn, its launch and the deliveries answer names,
// under this node's active key, with [RecordedAnswer.Via] set from words so
// the clear copy and the sealed one agree. What the coordinator records
// ([Coordinator.record]); exported for a case that seeds a recorded answer the
// way a node would have.
func (s *AnswerSeal) Seal(run PendingRun, words AnswerWords, answer RecordedAnswer) (RecordedAnswer, error) {
	body, err := json.Marshal(sealedAnswer{
		TurnID: run.TurnID, LaunchID: run.LaunchID, EventIDs: answer.EventIDs, Via: words.Via,
		By: words.By, ByKind: words.ByKind, OperatorID: words.OperatorID, Text: words.Text,
		Choice: words.Choice, Label: words.Label,
	})
	if err != nil {
		return RecordedAnswer{}, fmt.Errorf("sandbox: encoding the answer to run %s: %w", run.TurnID, err)
	}
	answer.Via, answer.Sealed = words.Via, s.signer.Seal(body)
	return answer, nil
}

// Open is the answer recorded on run, once its seal opens as the answer to the
// question the row's own job asked: [ErrAnswerKeyUnknown] where it is sealed
// under a key this node does not hold, and [ErrAnswerUnopened] where it does
// not open — not a frame, a MAC that fails, or a body bound to another run,
// another question, other deliveries or another route than the row names.
//
// THE ROW SUPPLIES ONLY WHERE THE ANSWER MUST BELONG, never what it says: the
// turn, the launch, the deliveries and the route are compared, and everything
// a reader is handed comes out of the seal.
func (s *AnswerSeal) Open(run PendingRun) (AnswerWords, error) {
	if run.Answer == nil {
		return AnswerWords{}, fmt.Errorf("%w: run %s records no answer", ErrAnswerUnopened, run.TurnID)
	}
	answer := *run.Answer
	body, verdict := s.verifier.Open(answer.Sealed)
	switch verdict {
	case statelog.Verified:
	case statelog.KeyUnknown:
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
	}
	return AnswerWords{Text: sealed.Text, Choice: sealed.Choice, Via: sealed.Via, By: sealed.By,
		ByKind: sealed.ByKind, OperatorID: sealed.OperatorID, Label: sealed.Label}, nil
}
