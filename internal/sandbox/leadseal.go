package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

// A lead a run keeps beside its record is sealed once, by the launch whose brief
// first told it ([KeptLead.Sealed]).
//
// # Why it is sealed
//
// A lead is told again to a later session of the work as what carried it on: an
// answer, in the words of the person who gave it, which the run reads as their
// instruction. That makes it what ADR-0031 calls an instruction, as the answer it
// was is ([AnswerSeal]), so it is signed under the fleet keyring in a key domain
// of its own ([LeadSealDomain]): no frame of another channel opens as a lead, and
// no lead as an answer or anything else. It is signed and not encrypted: its
// words are what the row must not forge, as an answer's are, and a held call's
// result inside it rides as its row sealed it ([HeldKept], under the call's own
// AEAD), so the lead keeps nothing in the clear the row did not.
//
// # What the seal binds
//
// The turn, the job whose brief first told the lead, that job's place among the
// turn's launches, the lead's kind and the round it carried on: a lead sealed
// for one job opens as that job's lead alone, carried onto another turn's
// leads, or onto another place in this one's history, it does not open. NEVER
// WHERE IT IS KEPT: a lead is sealed once and kept as sealed beside the run's
// record ([leadsPart]), never sealed again however often a later job takes its
// own leads in beside it, so a later build's keys in its body survive and a
// lead under a key this node lacks is kept for a node that holds it.
//
// # Two failures, and neither waits
//
// A seal under a key this node does not hold is a fact about THIS NODE
// ([ErrLeadKeyUnknown]): the lead is told as a message that cannot be read here,
// stays where it is kept as it was, and a later relaunch on a node that holds
// the key tells it. Unlike an answer it does not hold the resume for a node that
// holds the key ([ConditionAnswerKey]): an answer is the instruction the run
// parked for, and a lead told again is one the session may already show, so a
// resume held for it would hold the run for a message it may not need. A seal
// that does not open under a key this node holds, or opens as another job's
// lead, is a fact about WHAT IS KEPT, the run's leads beside its record
// ([ErrLeadUnopened]): nothing of it is told, it is logged at error, and it
// stays where it is kept until the bound on the run's leads drops it
// ([MaxKeptLeadBytes], [MaxKeptLeads]). And a lead of a kind this build does
// not know ([ErrLeadKindUnknown]) is a fact about THIS BUILD, the disposition a
// key this node lacks gets: told as a message that cannot be read here, and
// kept for a build that reads it.

// LeadSealDomain is the key domain a lead a run keeps is sealed in: its own, so
// that no frame from a state log or another channel opens as a lead, and no
// sealed lead as anything else.
const LeadSealDomain = "seat.run.lead"

// ErrLeadKeyUnknown is a lead sealed under a key this node does not hold.
var ErrLeadKeyUnknown = errors.New("sandbox: the lead is sealed under a key this node does not hold")

// ErrLeadUnopened is a lead whose seal does not open under a key this node holds,
// or opens as another job's lead.
var ErrLeadUnopened = errors.New("sandbox: the lead's seal does not open as a lead of this work")

// ErrLeadKindUnknown is a lead of a kind this build does not know, a newer
// build's ([LeadKind.Valid]).
var ErrLeadKindUnknown = errors.New("sandbox: the lead is of a kind this build does not know")

// LeadSeal seals a lead onto the job whose brief first told it and opens it again
// for a reader. SAFE FOR CONCURRENT USE: both halves are immutable once built.
type LeadSeal struct {
	signer   *recordseal.Signer
	verifier *recordseal.Verifier
}

// NewLeadSeal builds both halves over the fleet keyring every state-log record is
// signed under, refusing one that could not seal (no active key) or open (no key
// at all), by the field to set.
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

// Domain is the key domain leads are sealed in, for the gate that holds every
// instruction channel's domains apart from every other's and from every state
// log's.
func (s *LeadSeal) Domain() string { return LeadSealDomain }

// KeyIDs is every key id this node opens leads under, sorted, for the line that
// says why a lead could not be read.
func (s *LeadSeal) KeyIDs() []string { return s.verifier.KeyIDs() }

// sealedLead is the body the seal covers.
//
// A FIXED STRUCT AND NO omitempty, so the bytes are a function of the values
// alone: every field is present, in this order, on every seal.
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
// answer carries its answer and no task, a task its own facts and no answer, and
// a review neither, its words recomposed from the round it closed.
func shapeRefusal(lead Lead) string {
	switch lead.Kind {
	case LeadKindAnswer:
		if lead.Answer == nil || lead.Task != nil {
			return "an answer lead carries its answer and nothing else"
		}
	case LeadKindTask:
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

// Seal is lead sealed for turn turnID as the lead of the job it names (its Job
// and Seq set), told by that job's brief, under this node's active key: what the
// attach that names the job's command takes into the run's leads
// (Coordinator.Launch).
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
// of the job and the place its kept fields name it by: [ErrLeadKindUnknown]
// where they name a kind this build does not know, [ErrLeadKeyUnknown]
// where it is sealed under a key this node does not hold, and [ErrLeadUnopened]
// where it does not open: not a frame, a MAC that fails, a body bound to
// another turn, job, place, kind or round, or a source that does not suit its
// kind.
//
// THE KEPT FIELDS SUPPLY ONLY WHERE THE LEAD MUST BELONG, never what it says:
// the turn, the job, its place, the kind and the round are compared, and
// everything a reader is handed comes out of the seal.
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
