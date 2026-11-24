package sandbox

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A relaunch's brief opens with a lead (an answer, a review that sent the round
// back, or a continued session's task) that no record keeps once the next job
// opens, and only a session whose harness ended its own run is known to hold.
// So the run keeps the turn's leads, oldest first, in a part beside its row
// ([leadsPart]), each taken in by the attach naming its job's command
// ([PendingStore.AttachSandbox]) and sealed once, so a later build's keys and a
// lead under a key this node lacks survive; a continuation that may lack its
// brief is told them again ([LaunchRecord.ToldLeads]). The part lives and dies
// with the record ([coord.RunLeads]): a take racing the run's end is undone.
// Beside the row, as only a resume reads them and half a MiB would ride its
// every read; the row keeps each job's marks and [PendingRun.LeadsAppended], so
// a part gone, short, unreadable or another run's tells its leads as not kept.

// LeadKind is what carried a session of the work on, as a later brief tells it
// again.
type LeadKind string

const (
	// LeadKindAnswer is an answer to the run's question, a withdrawal of it,
	// a refused reply, or a held call a person answered ([LeadAnswer]), the
	// calls the run parked on told beside it ([LeadAnswer.Called]).
	LeadKindAnswer LeadKind = "answer"
	// LeadKindCalled is the engine's answer that the calls the run parked on
	// returned ([LeadAnswer.Called]): A KIND OF ITS OWN, as LeadKindWaited is,
	// so a build that does not know it tells it as unreadable
	// ([LeadKind.Valid]) rather than as a person's answer. It carries a
	// [LeadAnswer].
	LeadKindCalled LeadKind = "called"
	// LeadKindWaited is the engine's answer to the run's `wait_for_run`
	// ([LeadAnswer.Waited]), a kind of its own as LeadKindCalled is. It
	// carries a [LeadAnswer].
	LeadKindWaited LeadKind = "waited"
	// LeadKindReview is a review that sent the round back, recomposed from
	// the round it closed ([KeptLead.Round]), and said as not kept once a park
	// has dropped that round.
	LeadKindReview LeadKind = "review"
	// LeadKindTask is the turn's task, told to a job that continued the
	// session its conversation's last turn left, recomposed with the closed
	// rounds its brief rendered ([LeadTask]).
	LeadKindTask LeadKind = "task"
	// LeadKindFresh is the turn's task as a fresh brief told it to a session
	// of the run's own, recomposed as a task lead is ([LeadTask]): kept by
	// every launch whose run may start a session on that brief
	// ([LaunchRequest.FreshOwn], [BriefLeads.Fresh]).
	LeadKindFresh LeadKind = "fresh"
)

// LeadKinds are the six.
var LeadKinds = []LeadKind{LeadKindAnswer, LeadKindCalled, LeadKindWaited, LeadKindReview, LeadKindTask,
	LeadKindFresh}

// Valid reports whether k is one of [LeadKinds]. A newer build's kind is kept as
// sealed and told as a message that cannot be read here.
func (k LeadKind) Valid() bool { return slices.Contains(LeadKinds, k) }

// KeptLead is one lead as its run's leads part keeps it ([leadsPart]): in the
// clear what a reader orders, filters and logs it by, and the whole sealed once,
// by the job whose brief first told it ([LeadSeal]), kept as sealed through every
// take. THE JSON TAGS ARE A WIRE FORMAT, as the row's are, carrying a later
// build's keys ([KeptLead.Extra]).
type KeptLead struct {
	Kind LeadKind `json:"kind"`

	// Job is the job whose own brief opened with the lead, and Seq that job's
	// place among the turn's launches ([PendingRun.LaunchSeq]).
	Job string `json:"job"`
	Seq int64  `json:"seq"`

	// Round is the turn iteration the lead carried on: a review's, the one it
	// closed.
	Round int `json:"round"`

	// Appended is the lead's number in the order the part took leads in, from
	// 1 ([leadsPart.Appends]), which a job's record names it by
	// ([LaunchRecord.LeadMarks]): given by the take, outside the seal, never
	// changed.
	Appended int `json:"appended"`

	// Sealed is the lead itself ([LeadSeal.Seal]), never re-sealed.
	Sealed []byte `json:"sealed"`

	// Extra is every key of the section this build does not know: see
	// wirekeys.go.
	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON writes the section with the keys a later build added to it.
func (k KeptLead) MarshalJSON() ([]byte, error) {
	type plain KeptLead
	return marshalCarrying(plain(k), wireKeys(reflect.TypeFor[KeptLead]()), k.Extra)
}

// UnmarshalJSON reads the section and keeps the keys this build does not know.
func (k *KeptLead) UnmarshalJSON(raw []byte) error {
	type plain KeptLead
	p := plain(*k)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[KeptLead]()))
	if err != nil {
		return err
	}
	*k = KeptLead(p)
	k.Extra = extra
	return nil
}

// Ref names the lead by its job and its kind, as a brief that tells it again
// names it ([LeadRef]).
func (k KeptLead) Ref() LeadRef { return LeadRef{Job: k.Job, Kind: k.Kind} }

// LeadRef names a lead a brief tells again ([BriefTold.Told]): by the job whose
// brief first told it and its kind, which a job's leads never repeat.
type LeadRef struct {
	Job  string
	Kind LeadKind
}

// Lead is a lead opened ([LeadSeal.Open]), or one a brief is about to tell
// before its job has a name ([BriefTold.Own]): IN MEMORY ONLY.
type Lead struct {
	Kind  LeadKind
	Job   string
	Seq   int64
	Round int

	// Answer is an answer lead's source, Task a task or fresh lead's own
	// facts; a review keeps neither.
	Answer *LeadAnswer
	Task   *LeadTask

	// Unread is why a lead a resume read ([PendingStore.Leads]) did not open on
	// this node ([ErrLeadKeyUnknown], [ErrLeadUnopened]); its source is then
	// unset, and it is told as a message that cannot be read here, never
	// dropped.
	Unread string
}

// LeadAnswer is an answer lead's source: what the run was told, in the parts a
// later brief tells again. SEALED WHOLE ([sealedLead]): a fixed struct with no
// omitempty, so the sealed bytes are a function of the values alone.
type LeadAnswer struct {
	// Question is the run's own question, as its brief read it back.
	Question string `json:"question"`

	// Words is the answer's words, or a withdrawal's reason, cut to
	// [MaxLeadWordsBytes]; Cut is how many bytes were cut, said when told.
	Words string `json:"words"`
	Cut   int    `json:"cut"`

	// By is who answered, "" where the reply's route did not say; Label who
	// wrote the words, as a chat reply's fence labelled them, nil for an
	// answer by turn (ADR-0040, ADR-0068).
	By    string            `json:"by"`
	Label *provenance.Label `json:"label"`

	// Withdrawn says the seat withdrew the question or held call itself
	// (ADR-0111); Refused that a reply too large to hand the run was refused
	// (ADR-0114).
	Withdrawn bool         `json:"withdrawn"`
	Refused   *LeadRefused `json:"refused"`

	// Held is the call a person answered, nil for a question the run asked
	// itself (ADR-0077).
	Held *LeadHeld `json:"held"`

	// Called is every bridged call of the run's own it parked on
	// ([PendingRun.ParkedCalls]), and Returned what they returned as the run
	// was told it, cut to [MaxLeadWordsBytes] with ReturnedCut bytes cut: told
	// again as those calls, never as a person's answer. Under
	// [LeadKindCalled] the answer itself is the engine's that they returned
	// (callwait.go), Words then empty.
	Called      []types.CallReturned `json:"called"`
	Returned    string               `json:"returned"`
	ReturnedCut int                  `json:"returned_cut"`

	// Waited is how the run a `wait_for_run` waited for left its seat, where
	// the engine answered it (ADR-0176), under [LeadKindWaited] alone: told
	// again as that run's account, never as a person's answer.
	Waited *types.RunWaited `json:"waited"`
}

// LeadRefused is a refused reply's figures as the run was told them: its
// messages, their bytes as the engine records an answer, and the bound.
type LeadRefused struct {
	Messages int `json:"messages"`
	Bytes    int `json:"bytes"`
	MaxBytes int `json:"max_bytes"`
}

// LeadHeldOutcome is what became of a held call, as its lead told it.
type LeadHeldOutcome string

const (
	// LeadHeldMade is a call made, whose result the lead keeps.
	LeadHeldMade LeadHeldOutcome = "made"
	// LeadHeldMadeUnread is a call made whose result could not be opened where
	// the lead was told: kept sealed for a node that holds its key.
	LeadHeldMadeUnread LeadHeldOutcome = "made_unread"
	// LeadHeldDenied is a call the person refused.
	LeadHeldDenied LeadHeldOutcome = "denied"
	// LeadHeldNotMade is a call the person allowed that was not made, Why
	// saying why.
	LeadHeldNotMade LeadHeldOutcome = "not_made"
	// LeadHeldUnknown is a call whose node stopped before its result was
	// recorded: it may or may not have taken effect.
	LeadHeldUnknown LeadHeldOutcome = "unknown"
)

// LeadHeldOutcomes are the five.
var LeadHeldOutcomes = []LeadHeldOutcome{LeadHeldMade, LeadHeldMadeUnread, LeadHeldDenied, LeadHeldNotMade,
	LeadHeldUnknown}

// Valid reports whether o is one of [LeadHeldOutcomes]: a newer build's word is
// told as a call whose outcome is not known here.
func (o LeadHeldOutcome) Valid() bool { return slices.Contains(LeadHeldOutcomes, o) }

// LeadHeld is a held-call lead: the call, the person's choice, and what became of
// it.
type LeadHeld struct {
	// Tool is the tool as the run called it, Server its MCP server, HeldID the
	// held call's id, which a list of the round's calls leaves out where the
	// lead tells that call ([BridgeCall.HeldID]).
	Tool   string `json:"tool"`
	Server string `json:"server"`
	HeldID string `json:"held_id"`

	// Choice is the person's answer ([HeldOnce], [HeldAlways], [HeldDeny]).
	Choice string `json:"choice"`

	// Outcome is what became of the call; Failed whether the tool reported a
	// failure; Why why a call allowed was not made.
	Outcome LeadHeldOutcome `json:"outcome"`
	Failed  bool            `json:"failed"`
	Why     string          `json:"why"`

	// Result is what a call made returned, as the run's row sealed it
	// ([HeldKept]): once the next job opens, only the lead holds it.
	Result *HeldKept `json:"result"`
}

// LeadTask is a task or fresh lead's own facts; the rest is recomposed from the
// turn's records.
type LeadTask struct {
	// Rounds is the number of the last closed round the brief's prior-work
	// block rendered, zero for none: the task is told again with the rounds up
	// to it (a later round is told by its own rule), and whole with any span a
	// park has since folded them into, which holds their calls
	// (internal/agent/runner's roundsThrough). A NUMBER, NEVER A COUNT OF
	// RENDERED ENTRIES: once a park folds rounds into one span
	// (internal/agent/ledger's Iteration.Through), a count reaches past them.
	Rounds int `json:"rounds"`

	// Reviewed is the iteration whose review the task carried (a send-back's
	// continuation of that session), 0 for none.
	Reviewed int `json:"reviewed"`
}

// MaxLeadWordsBytes bounds an answer's words as a lead keeps them
// ([LeadAnswer.Words]), cut past it with the cut said: the page an answer by
// turn and a held call's result are held to ([MaxAnswerTextBytes],
// [MaxHeldResultBytes]). A chat reply can reach [MaxAnswerBytes], and one such
// reply, sealed, would overrun [MaxKeptLeadBytes]; a run that needs the rest
// asks again.
const MaxLeadWordsBytes = MaxAnswerTextBytes

// MaxKeptLeadBytes bounds the leads a run's leads part keeps, as the part holds
// them ([leadsPart.Leads]): past it the oldest are dropped, still counted
// ([leadsPart.Appends]), and the count is told wherever leads are told again.
//
// HALF A MEBIBYTE holds one lead at its worst (a held call's, words at
// [MaxLeadWordsBytes] escaped six-fold and result at [MaxHeldResultBytes],
// sealed twice and base64: about 300 KiB) and many of a few KiB. Every resume
// reads it, so it holds the newest leads, which a cut-off session most likely
// lacks, not every lead of a long turn.
const MaxKeptLeadBytes = 512 << 10

// MaxKeptLeads bounds how many leads a run's leads part keeps, so the numbers a
// job's record names them by ([LaunchRecord.LeadMarks]) are bounded by
// construction: 2,048, about 40 KiB on the row at their widest. 256 bytes is
// under the least any lead this build seals weighs in the part (318, a review
// lead of a one-byte turn under a one-byte key, as
// TestTheCountOfKeptLeadsNeverDropsALeadTheirBytesWouldKeep measures), so the
// count drops only the small leads of a build that seals smaller ones.
const MaxKeptLeads = MaxKeptLeadBytes / 256

// ToldLeads is the leads the job's brief told (its own, and every earlier one it
// told again or could not read), by their numbers in the run's leads part
// ([LaunchRecord.LeadMarks]): what a continuation tells again where the session
// may lack its brief. None where the job started a fresh session (a fresh brief,
// or a box that started one in place of a continued one), which holds its
// opening message wherever it can be continued.
func (l LaunchRecord) ToldLeads() []int {
	if l.Brief == BriefFresh || l.SessionStart == SessionStartFresh {
		return nil
	}
	return slices.Clone(l.LeadMarks)
}

// BriefLeads is the leads of the brief a launch hands its job's run, as the
// attach that names the job's command keeps them ([PendingStore.AttachSandbox]).
type BriefLeads struct {
	// Run and Job are as the launch opened the row ([PendingRun.LeadsRun],
	// [PendingRun.LaunchID]): another run's part takes none of it, and a row
	// naming another job is marked with none.
	Run, Job string

	// Counted is [PendingRun.LeadsAppended] as the launch opened the row: a
	// part the take starts anew, or one holding fewer, numbers its next lead
	// past it, so a lost or replaced part's leads are never numbered over and
	// still count as not kept.
	Counted int

	// Own is the brief's own leads, sealed for the job ([LeadSeal.Seal]); Told
	// the earlier leads it told again, by name; ToldUnread the earlier ones it
	// could not read, by number ([BriefTold.ToldUnread]), marked as Told is.
	// Fresh is the fresh brief's leads beside a brief that continues a session:
	// taken in after Own, never marked, since the brief the run read did not
	// tell them (package doc).
	Own        []KeptLead
	Fresh      []KeptLead
	Told       []LeadRef
	ToldUnread []int
}

// TurnLeads is the turn's leads as a resume reads them beside its run's row
// ([PendingStore.Leads]): the run's kept leads, oldest first, and how many
// earlier ones are not kept, past the bound or counted by the row
// ([PendingRun.LeadsAppended]) but not given back by the part.
type TurnLeads struct {
	Kept []KeptLead
	Cut  int

	// Unread says the store could not give the part back at all
	// ([Coordinator.leadsAt]): Kept is none and Cut every lead the row counts,
	// though the part may still keep them. The job's marked leads are then
	// handed on by number ([BriefTold.ToldUnread]), marked again on the
	// relaunched job, and told by the first later read that answers. False
	// wherever the store answered, even for a part gone, short or another
	// run's: what such a part lacks, no later read gives back.
	Unread bool
}

// leadsPart is the part beside a run's row holding the turn's leads
// ([coord.RunLeads]), oldest first, each numbered as taken ([KeptLead.Appended]).
// It is one store value, held to [RowBytes] as the row is, and split:
//
//	the leads it keeps .............. MaxKeptLeadBytes          524,288  (512 KiB)
//	its own fields, measured ........ measuredLeadsFields            85
//	to spare, measured .............. measuredLeadsSpare      5,765,973
//	                                                          ---------
//	                                  RowBytes                6,290,346
//
// The spare is room for a later build's part keys ([leadsPart.Extra]); a key
// added to a LEAD comes out of the leads' own share.
type leadsPart struct {
	// Run is the run the leads are of ([PendingRun.LeadsRun]): the store may
	// leave another run's part beside a row its turn created again.
	Run string `json:"run"`

	// Leads is the leads the part keeps, oldest first, held to
	// [MaxKeptLeadBytes] and [MaxKeptLeads] ([heldLeads]).
	Leads []KeptLead `json:"leads"`

	// Appends is how many leads the part has ever taken: the cut is Appends
	// less what Leads holds.
	Appends int `json:"appends"`

	// Extra is every key of the part this build does not know (wirekeys.go),
	// kept through every take.
	Extra map[string]json.RawMessage `json:"-"`
}

// The part table's measured figures: its own fields at their worst, and what
// [RowBytes] leaves beside them and its leads at their bound, as
// TestTheWholeLeadsPartFitsItsBudget measures the worst part. PINNED, as
// [measuredOtherFields] is: the test compares both exactly, so a change moving
// either restates both here and in the table.
const (
	measuredLeadsFields = 85
	measuredLeadsSpare  = 5_765_973
)

// MarshalJSON writes the part with the keys a later build added to it.
func (p leadsPart) MarshalJSON() ([]byte, error) {
	type plain leadsPart
	return marshalCarrying(plain(p), wireKeys(reflect.TypeFor[leadsPart]()), p.Extra)
}

// UnmarshalJSON reads the part and keeps the keys this build does not know.
func (p *leadsPart) UnmarshalJSON(raw []byte) error {
	type plain leadsPart
	q := plain(*p)
	extra, err := unmarshalCarrying(raw, &q, wireKeys(reflect.TypeFor[leadsPart]()))
	if err != nil {
		return err
	}
	*p = leadsPart(q)
	p.Extra = extra
	return nil
}

// took is p having taken in own, the brief's own leads, then fresh, the fresh
// brief's ([BriefLeads.Fresh]): each once by [KeptLead.Ref] (one already kept
// left as is, a new one numbered as taken), the whole held by [heldLeads].
// marks is the ascending numbers of every lead the brief told (own, told,
// toldUnread, never fresh's) that next keeps; grew whether it took anything in.
//
// A TOLD LEAD THE PART DOES NOT KEEP IS NOT MARKED: a reader counts it against
// the row ([PendingStore.Leads]). Names and numbers are looked up in sets, so
// retelling every kept lead stays linear.
func (p leadsPart) took(own, fresh []KeptLead, told []LeadRef, toldUnread []int) (next leadsPart, marks []int,
	grew bool) {
	next = p
	next.Leads = slices.Clone(p.Leads)
	named := make(map[LeadRef]bool, len(own)+len(told))
	for _, ref := range told {
		named[ref] = true
	}
	numbered := make(map[int]bool, len(toldUnread))
	for _, n := range toldUnread {
		numbered[n] = true
	}
	kept := make(map[LeadRef]bool, len(next.Leads)+len(own)+len(fresh))
	for _, lead := range next.Leads {
		kept[lead.Ref()] = true
	}
	for i, lead := range slices.Concat(own, fresh) {
		if i < len(own) {
			named[lead.Ref()] = true
		}
		if kept[lead.Ref()] {
			continue
		}
		next.Appends++
		lead.Appended = next.Appends
		next.Leads = append(next.Leads, lead)
		kept[lead.Ref()] = true
		grew = true
	}
	next.Leads = heldLeads(next.Leads)
	for _, lead := range next.Leads {
		if named[lead.Ref()] || numbered[lead.Appended] {
			marks = append(marks, lead.Appended)
		}
	}
	slices.Sort(marks)
	return next, marks, grew
}

// heldLeads is leads held to [MaxKeptLeadBytes] as the part holds them and to
// [MaxKeptLeads], oldest first, since the newest are what a cut-off session most
// likely lacks. A dropped lead stays counted under [leadsPart.Appends], which no
// drop lowers. A lead that cannot be encoded (a carried key that is not JSON) is
// dropped wherever it stands: kept, it would refuse every write of the part.
func heldLeads(leads []KeptLead) []KeptLead {
	var (
		kept  []KeptLead
		sizes []int
	)
	total := len("[]")
	for _, lead := range leads {
		raw, err := json.Marshal(lead)
		if err != nil {
			continue
		}
		if len(kept) > 0 {
			total += len(",")
		}
		kept, sizes, total = append(kept, lead), append(sizes, len(raw)), total+len(raw)
	}
	for len(kept) > 0 && (total > MaxKeptLeadBytes || len(kept) > MaxKeptLeads) {
		total -= sizes[0]
		if len(kept) > 1 {
			total -= len(",")
		}
		kept, sizes = kept[1:], sizes[1:]
	}
	return kept
}
