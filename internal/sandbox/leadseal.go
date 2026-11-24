package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

// LeadSealDomain is the key domain a lead a run keeps is sealed in. A lead is
// told to a later session as a person's answer, so it is an instruction under
// ADR-0031 as the answer it was is ([AnswerSeal]), signed under the fleet
// keyring in a domain of its own: no frame of a state log or another channel
// opens as a lead, nor a lead as anything else. Signed, not encrypted: its words
// are what the row must not forge, and a held call's result inside it stays
// under the call's own AEAD ([HeldKept]), so nothing is in the clear the row hid.
const LeadSealDomain = "seat.run.lead"

// ErrLeadKeyUnknown is a lead sealed under a key this node does not hold, a fact
// about THIS NODE: it is told as unreadable here and kept as it was for a
// relaunch on a node that holds the key. Unlike an answer it never holds the
// resume for such a node ([ConditionAnswerKey]): the session may already show it.
var ErrLeadKeyUnknown = errors.New("sandbox: the lead is sealed under a key this node does not hold")

// ErrLeadUnopened is a lead whose seal does not open under a key this node holds,
// or opens as another job's lead, a fact about WHAT IS KEPT: nothing of it is
// told, it is logged at error, and it stays until the bound on the run's leads
// drops it ([MaxKeptLeadBytes], [MaxKeptLeads]).
var ErrLeadUnopened = errors.New("sandbox: the lead's seal does not open as a lead of this work")

// ErrLeadKindUnknown is a lead of a kind this build does not know, a newer
// build's ([LeadKind.Valid]): a fact about THIS BUILD, disposed of as
// [ErrLeadKeyUnknown] is, and kept for a build that reads it.
var ErrLeadKindUnknown = errors.New("sandbox: the lead is of a kind this build does not know")

// LeadSeal seals a lead once, by the launch whose brief first told it
// ([KeptLead.Sealed]), and opens it for a reader. The seal binds the turn, the
// job, its place among the turn's launches, the kind and the round, NEVER WHERE
// IT IS KEPT: kept as sealed beside the run's record ([leadsPart]) and never
// resealed, so a later build's keys in it survive and a lead under a key this
// node lacks is kept for a node that holds it. SAFE FOR CONCURRENT USE: both
// halves are immutable once built.
type LeadSeal struct {
	signer   *recordseal.Signer
	verifier *recordseal.Verifier
}

// NewLeadSeal builds both halves over the fleet keyring, refusing one that could
// not seal (no active key) or open (no key at all), by the field to set.
func NewLeadSeal(ring recordseal.Keyring) (*LeadSeal, error) {
	signer, err := recordseal.NewSigner(LeadSealDomain, ring)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the seal on the leads a run keeps: %w", err)
	}
	verifier, err := recordseal.NewVerifier(LeadSealDomain, ring)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the seal on the leads a run keeps: %w", err)
	}
	return &LeadSeal{signer: signer, verifier: verifier}, nil
}

// Domain is [LeadSealDomain], for the gate that holds every instruction
// channel's domain apart from every other's and every state log's.
func (s *LeadSeal) Domain() string { return LeadSealDomain }

// KeyIDs is every key id this node opens leads under, sorted, for the line that
// says why a lead could not be read.
func (s *LeadSeal) KeyIDs() []string { return s.verifier.KeyIDs() }

// sealedLead is the body the seal covers: A FIXED STRUCT AND NO omitempty, so
// the bytes are a function of the values alone.
type sealedLead struct {
	TurnID string      `json:"turn_id"`
	Job    string      `json:"job"`
	Seq    int64       `json:"seq"`
	Kind   LeadKind    `json:"kind"`
	Round  int         `json:"round"`
	Answer *LeadAnswer `json:"answer"`
	Task   *LeadTask   `json:"task"`
}

// shapeRefusal is why lead's source does not suit its kind, "" where it does: an
// answer carries its answer alone, never a wait's account; the engine's answer
// that calls returned carries those calls, and to a wait that wait's account,
// neither a held call nor a refused reply, which only a person's reply makes;
// a task or a fresh brief's task its own facts alone; and a review neither,
// its words recomposed from the round it closed.
func shapeRefusal(lead Lead) string {
	switch lead.Kind {
	case LeadKindAnswer:
		if lead.Answer == nil || lead.Task != nil || lead.Answer.Waited != nil {
			return "an answer lead carries its answer and nothing else"
		}
	case LeadKindCalled, LeadKindWaited:
		a := lead.Answer
		switch {
		case a == nil || lead.Task != nil || a.Held != nil || a.Refused != nil:
			return "the engine's answer carries what it answered and nothing else"
		case lead.Kind == LeadKindCalled && (len(a.Called) == 0 || a.Waited != nil):
			return "a lead of calls that returned carries those calls"
		case lead.Kind == LeadKindWaited && a.Waited == nil:
			return "a wait's lead carries the account of the run it waited for"
		}
	case LeadKindTask, LeadKindFresh:
		if lead.Task == nil || lead.Answer != nil {
			return "a task lead carries the task's own facts and nothing else"
		}
	case LeadKindReview:
		if lead.Answer != nil || lead.Task != nil {
			return "a review lead carries nothing of its own: its words are its round's"
		}
	default:
		return fmt.Sprintf("its kind %q is none this build knows", lead.Kind)
	}
	return ""
}

// Seal is lead sealed under this node's active key for turn turnID, as the lead
// of the job its Job and Seq name, whose brief told it: what the attach naming
// the job's command takes into the run's leads (Coordinator.Launch).
func (s *LeadSeal) Seal(turnID string, lead Lead) (KeptLead, error) {
	switch {
	case lead.Job == "" || lead.Seq < 1:
		return KeptLead{}, fmt.Errorf("sandbox: a lead of run %s names no job: it is sealed for the job whose "+
			"brief told it", turnID)
	case shapeRefusal(lead) != "":
		return KeptLead{}, fmt.Errorf("sandbox: a lead of run %s is refused: %s", turnID, shapeRefusal(lead))
	}
	body, err := json.Marshal(sealedLead{TurnID: turnID, Job: lead.Job, Seq: lead.Seq, Kind: lead.Kind,
		Round: lead.Round, Answer: lead.Answer, Task: lead.Task})
	if err != nil {
		return KeptLead{}, fmt.Errorf("sandbox: encoding a lead of run %s: %w", turnID, err)
	}
	return KeptLead{Kind: lead.Kind, Job: lead.Job, Seq: lead.Seq, Round: lead.Round,
		Sealed: s.signer.Seal(body)}, nil
}

// Open is kept, a lead turn turnID's run keeps, once its seal opens as the lead
// of the job and place its kept fields name: [ErrLeadKindUnknown] for a kind
// this build does not know, [ErrLeadKeyUnknown] for a key this node does not
// hold, and [ErrLeadUnopened] where it does not open (not a frame, a failing
// MAC, a body bound to another turn, job, place, kind or round, or a source
// unsuited to its kind).
//
// THE KEPT FIELDS SUPPLY ONLY WHERE THE LEAD MUST BELONG, never what it says:
// they are compared, and everything a reader is handed comes out of the seal.
func (s *LeadSeal) Open(turnID string, kept KeptLead) (Lead, error) {
	if !kept.Kind.Valid() {
		return Lead{}, fmt.Errorf("%w: %q", ErrLeadKindUnknown, kept.Kind)
	}
	body, verdict := s.verifier.Open(kept.Sealed)
	switch verdict {
	case recordseal.Verified:
	case recordseal.KeyUnknown:
		return Lead{}, fmt.Errorf("%w (it holds %s): add the key the fleet sealed it under to secrets.keys",
			ErrLeadKeyUnknown, words.And(s.verifier.KeyIDs()))
	default:
		return Lead{}, fmt.Errorf("%w: the frame does not verify under the key it names", ErrLeadUnopened)
	}
	var sealed sealedLead
	if err := json.Unmarshal(body, &sealed); err != nil {
		return Lead{}, fmt.Errorf("%w: the sealed body does not decode: %w", ErrLeadUnopened, err)
	}
	switch {
	case sealed.TurnID != turnID:
		return Lead{}, fmt.Errorf("%w: it was sealed for run %q", ErrLeadUnopened, sealed.TurnID)
	case sealed.Job != kept.Job || sealed.Seq != kept.Seq:
		return Lead{}, fmt.Errorf("%w: it was sealed for job %q, launch %d, and is kept as job %q's, "+
			"launch %d", ErrLeadUnopened, sealed.Job, sealed.Seq, kept.Job, kept.Seq)
	case sealed.Kind != kept.Kind || sealed.Round != kept.Round:
		return Lead{}, fmt.Errorf("%w: it was sealed as %s of round %d, and is kept as %s of round %d",
			ErrLeadUnopened, sealed.Kind, sealed.Round, kept.Kind, kept.Round)
	}
	lead := Lead{Kind: sealed.Kind, Job: sealed.Job, Seq: sealed.Seq, Round: sealed.Round,
		Answer: sealed.Answer, Task: sealed.Task}
	if why := shapeRefusal(lead); why != "" {
		return Lead{}, fmt.Errorf("%w: %s", ErrLeadUnopened, why)
	}
	return lead, nil
}
