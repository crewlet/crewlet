package sandbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/words"
)

// A fold-in: whether another writer's request, waiting on a busy seat's inbox,
// joins the work a running job is doing (ADR-0109's amendment "another
// writer's request joins only on approval"). The engine asks it on the run's
// row ([PendingStore.AskFoldIn]), put to the run's requester and the seat's
// leads; a person answers it through `answer_executor` ([PendingStore.AnswerFoldIn]),
// decided as a run's question is (internal/runanswer). It is the JOB's: the
// next job's opening clears it, and the row goes at the turn's end.
//
// The row's copy of the request's words is what an approver READS, never what
// joins: an approved fold-in joins as the delivery waiting on the inbox, read
// again by [FoldIn.Wake], so a row holds no instruction a store writer could
// forge into a run.

// FoldInAnswer is what a person decided of a fold-in.
type FoldInAnswer string

const (
	// FoldInApprove joins the request to the running work as a steer note
	// naming who asked and who approved.
	FoldInApprove FoldInAnswer = "approve"
	// FoldInRefuse leaves the request to wait for a turn of its own.
	FoldInRefuse FoldInAnswer = "refuse"
)

// FoldInAnswers is the closed set, in the order a person is offered them.
var FoldInAnswers = []FoldInAnswer{FoldInApprove, FoldInRefuse}

// Valid reports whether a is one of [FoldInAnswers].
func (a FoldInAnswer) Valid() bool { return slices.Contains(FoldInAnswers, a) }

// FoldIn is one fold-in question on a run's row ([PendingRun.FoldIns]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), as a wake item is.
type FoldIn struct {
	// ID names the question, which an answer names it by: a UUID the engine
	// derives from the turn, the job and the wake, so asking again is asking
	// the same question.
	ID string `json:"id"`

	// Launch is the job whose work the request would join, and Wake the
	// delivery waiting on the seat's inbox that carries it.
	Launch string `json:"launch"`
	Wake   string `json:"wake"`

	// Asker and AskerKind are who wrote the request, by seat handle, as the
	// wake names its writer; Conversation is where they wrote it.
	Asker        string        `json:"asker"`
	AskerKind    iam.ActorKind `json:"asker_kind"`
	Conversation string        `json:"conversation"`

	// Words are the asker's words as an approver reads them, Cut how many
	// characters past them the message holds, and Label the fence a model
	// reads them in.
	Words string           `json:"words" untrusted:"fence"`
	Cut   int              `json:"cut"`
	Label provenance.Label `json:"label"`

	// PutTo are the seats it was put to, the run's requester first and then
	// the seat's leads nearest first, at most [MaxFoldInPutTo], never the
	// asker. An administrator and any lead of the seat may answer it too.
	PutTo []string `json:"put_to"`

	// AskedAt is when the store took it.
	AskedAt time.Time `json:"asked_at"`

	// Answer is what was decided, "" while it waits; AnsweredBy and
	// AnsweredByKind who decided it, as iam.ActorFor names them, and
	// AnsweredAt when the store took the answer.
	Answer         FoldInAnswer  `json:"answer"`
	AnsweredBy     string        `json:"answered_by"`
	AnsweredByKind iam.ActorKind `json:"answered_by_kind"`
	AnsweredAt     time.Time     `json:"answered_at,omitzero"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// Open reports whether f still waits for its answer.
func (f FoldIn) Open() bool { return f.Answer == "" }

// AnswerableBy reports whether by may be recorded as f's answerer: anybody but
// its asker, whose request it is. Who else may is the authority's to decide
// (internal/runanswer's FoldIn), before the answer reaches the store.
func (f FoldIn) AnswerableBy(by string) bool { return by != "" && by != f.Asker }

// FoldInFor is the fold-in the row's current job holds for wake.
func (r PendingRun) FoldInFor(wake string) (FoldIn, bool) {
	for _, f := range r.FoldIns {
		if f.Launch == r.LaunchID && f.Wake == wake {
			return f, true
		}
	}
	return FoldIn{}, false
}

// OpenFoldIns is every fold-in the row's current job waits on an answer to,
// while that job's harness takes notes ([ItemStatuses]): the ones a person can
// still answer.
func (r PendingRun) OpenFoldIns() []FoldIn {
	if !slices.Contains(ItemStatuses, r.Status) {
		return nil
	}
	var out []FoldIn
	for _, f := range r.FoldIns {
		if f.Launch == r.LaunchID && f.Open() {
			out = append(out, f)
		}
	}
	return out
}

// FoldInDecision is the decision a fold-in is put to people as: who asked the
// seat for what, which work it would join, whose apps it would run on, and the
// two answers. Role approver, as a held call's is ([HeldDecision]), since the
// answer decides whose request the requester's apps serve; evidence, the run's
// work item where it has one.
func FoldInDecision(run PendingRun, f FoldIn) *decision.Decision {
	requester := cmp.Or(run.Requester, run.RequesterLogin, "the person it works for")
	question := fmt.Sprintf("%s asked %s for work of their own while it works for %s: add it to that "+
		"running work, on %s's apps?", f.Asker, run.AgentHandle, requester, requester)
	d := &decision.Decision{Question: question, Role: decision.RoleApprover, Options: []decision.Option{
		{ID: string(FoldInApprove), Label: "Add it to the running work",
			Detail: "The run takes the request as a note naming who asked and who approved, and works on it " +
				"with " + requester + "'s apps."},
		{ID: string(FoldInRefuse), Label: "Run it on its own",
			Detail: "The request waits, and runs as a turn of its own once the running work ends."},
	}}
	d.Evidence = TaskEvidence(run.WorkItem)
	return d
}

// TaskEvidence is what a question put to people about a run cites: the work
// item the run is on, where it is a Crewlet Works task (the one kind of item a
// task reference names), labelled by its key where that fits a label. A held
// call's and a fold-in's both cite it.
func TaskEvidence(item *types.WorkItem) []decision.Evidence {
	if item == nil || item.Backend != types.WorkWorklet || item.ID == "" {
		return nil
	}
	label := item.Key
	if len(label) > decision.MaxEvidenceLabel {
		label = ""
	}
	return []decision.Evidence{{Kind: decision.EvidenceTask, Ref: item.ID, Label: label}}
}

// ErrFoldInClosed is a fold-in whose job is over or was never the run's: the
// record is gone, its ending is decided, or it names another job or a status
// whose harness takes no notes ([ItemStatuses]). The request runs as a turn of
// its own.
var ErrFoldInClosed = errors.New("sandbox: the work this fold-in would join has ended")

// ErrFoldInsSpent is a fold-in asked of a job holding [MaxJobFoldIns] already.
var ErrFoldInsSpent = errors.New("sandbox: the job holds every fold-in it may")

// ErrFoldInTooLarge is a fold-in encoded past [MaxFoldInBytes], refused before
// anything is written.
var ErrFoldInTooLarge = errors.New("sandbox: a fold-in is encoded past what the row holds of one")

// ErrFoldInAnswered is an answer to a fold-in answered already; the fold-in it
// is returned with names who answered first.
var ErrFoldInAnswered = errors.New("sandbox: the fold-in is answered already")

// ErrFoldInAsker is an answer by the fold-in's own asker ([FoldIn.AnswerableBy]).
var ErrFoldInAsker = errors.New("sandbox: a fold-in is never answered by its asker")

// FoldInAnswered is one answer to a fold-in ([PendingStore.AnswerFoldIn]).
type FoldInAnswered struct {
	Answer FoldInAnswer
	By     string
	ByKind iam.ActorKind
}

// foldInBytes is f as the row writes it.
func foldInBytes(f FoldIn) (int, error) {
	raw, err := json.Marshal(f)
	return len(raw), err
}

// AskFoldIn records a fold-in on a run's row. See the contract on
// [PendingStore].
//
// ONE LIVE WRITE, pinned to the job and [ItemStatuses]: one per job and wake
// (asking again answers the one recorded), within [MaxJobFoldIns] for the job.
// NO FENCE, as a note's filing takes none ([CoordStore.FileNote]).
func (s *CoordStore) AskFoldIn(ctx context.Context, turnID string, f FoldIn) (FoldIn, error) {
	if _, err := uuid.Parse(f.ID); err != nil || f.Launch == "" || !validWakeID(f.Wake) || f.Asker == "" ||
		len(f.PutTo) == 0 || len(f.PutTo) > MaxFoldInPutTo || slices.Contains(f.PutTo, f.Asker) ||
		f.Answer != "" {
		return FoldIn{}, fmt.Errorf("sandbox: a fold-in for run %s names a UUID, its job, its wake, its asker "+
			"and 1 to %d seats besides the asker it is put to, and no answer", turnID, MaxFoldInPutTo)
	}
	f.AskedAt, f.AnsweredBy, f.AnsweredByKind, f.AnsweredAt = s.clock(), "", "", time.Time{}
	switch size, err := foldInBytes(f); {
	case err != nil:
		return FoldIn{}, fmt.Errorf("sandbox: encoding fold-in %s for run %s: %w", f.ID, turnID, err)
	case size > MaxFoldInBytes:
		return FoldIn{}, fmt.Errorf("sandbox: fold-in %s for run %s is %d bytes, past the %d the row holds of "+
			"one: %w", f.ID, turnID, size, MaxFoldInBytes, ErrFoldInTooLarge)
	}
	var (
		got           FoldIn
		closed, spent bool
	)
	_, won, found, err := s.writeFound(ctx, turnID, true, Fence{}, func(run *PendingRun) bool {
		got, closed, spent = FoldIn{}, false, false
		if run.LaunchID != f.Launch || !slices.Contains(ItemStatuses, run.Status) {
			closed = true
			return false
		}
		job := 0
		for _, have := range run.FoldIns {
			if have.ID == f.ID || (have.Launch == f.Launch && have.Wake == f.Wake) {
				got = have
				return false
			}
			if have.Launch == f.Launch {
				job++
			}
		}
		if job >= MaxJobFoldIns {
			spent = true
			return false
		}
		run.FoldIns, got = append(slices.Clone(run.FoldIns), f), f
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return FoldIn{}, fmt.Errorf("sandbox: fold-in %s for run %s: %w", f.ID, turnID, ErrFoldInClosed)
	case err != nil:
		return FoldIn{}, err
	case won:
		return got, nil
	case !found || closed:
		return FoldIn{}, fmt.Errorf("sandbox: fold-in %s for run %s's job %s: %w", f.ID, turnID, f.Launch,
			ErrFoldInClosed)
	case spent:
		return FoldIn{}, fmt.Errorf("sandbox: fold-in %s for run %s's job %s: %w", f.ID, turnID, f.Launch,
			ErrFoldInsSpent)
	}
	return got, nil
}

// AnswerFoldIn records the answer to a fold-in on a run's row. See the contract
// on [PendingStore].
//
// ONE LIVE WRITE, pinned to the fold-in's job and [ItemStatuses]: the first
// answer is the answer, and a later one is refused naming it.
func (s *CoordStore) AnswerFoldIn(ctx context.Context, turnID, id string, a FoldInAnswered) (FoldIn, error) {
	if !a.Answer.Valid() || a.By == "" {
		return FoldIn{}, fmt.Errorf("sandbox: an answer to fold-in %s for run %s is one of %s, by somebody",
			id, turnID, words.Or(FoldInAnswers))
	}
	var (
		got                     FoldIn
		closed, answered, asker bool
	)
	_, won, found, err := s.writeFound(ctx, turnID, true, Fence{}, func(run *PendingRun) bool {
		got, closed, answered, asker = FoldIn{}, false, false, false
		at := slices.IndexFunc(run.FoldIns, func(f FoldIn) bool { return f.ID == id })
		if at < 0 || run.FoldIns[at].Launch != run.LaunchID || !slices.Contains(ItemStatuses, run.Status) {
			closed = true
			return false
		}
		f := run.FoldIns[at]
		switch {
		case !f.Open():
			got, answered = f, true
			return false
		case !f.AnswerableBy(a.By):
			got, asker = f, true
			return false
		}
		f.Answer, f.AnsweredBy, f.AnsweredByKind, f.AnsweredAt = a.Answer, a.By, a.ByKind, s.clock()
		run.FoldIns = slices.Clone(run.FoldIns)
		run.FoldIns[at], got = f, f
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return FoldIn{}, fmt.Errorf("sandbox: fold-in %s for run %s: %w", id, turnID, ErrFoldInClosed)
	case err != nil:
		return FoldIn{}, err
	case won:
		return got, nil
	case !found || closed:
		return FoldIn{}, fmt.Errorf("sandbox: fold-in %s for run %s: %w", id, turnID, ErrFoldInClosed)
	case answered:
		return got, fmt.Errorf("sandbox: fold-in %s for run %s, answered %s by %s: %w", id, turnID, got.Answer,
			got.AnsweredBy, ErrFoldInAnswered)
	case asker:
		return got, fmt.Errorf("sandbox: fold-in %s for run %s, answered by %s: %w", id, turnID, a.By,
			ErrFoldInAsker)
	}
	return got, nil
}
