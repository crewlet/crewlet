package sandbox

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/provenance"
)

// WHAT CARRIED A SESSION ON, KEPT TO TELL AGAIN.
//
// A relaunch's brief opens with what carries the session on: the answer to the
// run's question, a review that sent the round back, or, for a job that
// continued the session its conversation's last turn left, the turn's task. A
// session is known to hold the brief it was handed only where its harness ended
// its own run: one cut off, the copy a job's own box holds whose harness was
// not known to have ended cleanly and a capture cut off alike, may lack it, and
// lack the lead it opened with. The next brief to such a session was told every
// call and every note again, and never that lead, since nothing kept it: the
// reset that opens the next job clears the answer, its question and the held
// call in the write that names the job, and a review and a task are the
// engine's to recompose but nothing said which of them a job's brief opened
// with. A session cut off before its harness wrote its brief, continued in
// place, was then told "continue from where you are", and continued the turn
// before it, or the round before the review.
//
// So THE RUN KEEPS THE TURN'S LEADS BESIDE ITS ROW ([leadsPart],
// [PendingStore.Leads]), oldest first: each sealed once, by the job whose
// brief first told it, taken in by the attach that names its job's command
// ([PendingStore.AttachSandbox]) and kept as sealed through every relaunch, so
// a lead a later build wrote keys into survives, and one under a key this node
// lacks is kept for a node that holds it. Each job's record marks the ones its
// own brief told ([LaunchRecord.LeadMarks]): its own, and every earlier one it
// told again. A continuation of a session that may lack its job's brief is
// told again the leads that brief told ([LaunchRecord.ToldLeads]), under a
// header that says the session may already show them; one whose harness ended
// its own run holds its brief, and is told none.
//
// WHY BESIDE THE ROW. Half a mebibyte of leads on the row would ride every read
// of it (every flip, every listing, every git request of a coding box) and take
// the room the row keeps for the fields a later build adds, though only a
// resume reads them and only the attach that names a job's command writes
// them. A part lives and dies with its record, held by the store
// ([coord.RunLeads]): a take racing the run's end is undone, and the record's
// delete takes the part with it. So the row keeps only what a reader needs
// beside the part: each job's marks, the run the part is of
// ([PendingRun.LeadsRun]) and how many leads the part has taken
// ([PendingRun.LeadsAppended]), so a part that is gone, short, unreadable or
// another run's tells every lead it cannot give back as not kept, never as
// none.
//
// WHAT IS KEPT, AND WHAT IS RECOMPOSED. An answer is kept as the run was told
// it ([LeadAnswer]), its words cut to [MaxLeadWordsBytes] with the cut said,
// and a held call with what it returned, sealed as its row sealed it
// ([HeldKept]): no record holds either once the next job opens. A review and a
// task are the engine's own words over records the turn keeps (the round's
// review notes, the turn's task, its closed rounds and the session its
// conversation's last turn left), so a lead of either keeps only what says
// which: the round a review closed, and how many closed rounds a task's brief
// rendered ([LeadTask]).

// LeadKind is what carried a session of the work on, as a later brief tells it
// again.
type LeadKind string

const (
	// LeadKindAnswer is an answer to the run's question, a withdrawal of it,
	// a refused reply, or a held call a person answered ([LeadAnswer]).
	LeadKindAnswer LeadKind = "answer"
	// LeadKindReview is a review that sent the round back, recomposed from
	// the round it closed ([KeptLead.Round]).
	LeadKindReview LeadKind = "review"
	// LeadKindTask is the turn's task, told to a job that continued the
	// session its conversation's last turn left, recomposed with the closed
	// rounds its brief rendered ([LeadTask]).
	LeadKindTask LeadKind = "task"
)

// LeadKinds are the three.
var LeadKinds = []LeadKind{LeadKindAnswer, LeadKindReview, LeadKindTask}

// Valid reports whether k is one of [LeadKinds]. A lead a newer build sealed may
// name one this build does not know, read as the value it is: kept as sealed
// beside the run's record, and told as a message that cannot be read here.
func (k LeadKind) Valid() bool { return slices.Contains(LeadKinds, k) }

// KeptLead is one lead as its run's leads part keeps it ([leadsPart]): in the
// clear what a reader orders, filters and logs it by, and the whole of it sealed
// once, by the job whose brief first told it ([LeadSeal]), and kept as sealed
// through every take of the part.
//
// THE JSON TAGS ARE A WIRE FORMAT, as the row's are, and the section carries the
// keys a later build adds to it ([KeptLead.Extra]).
type KeptLead struct {
	Kind LeadKind `json:"kind"`

	// Job is the job whose own brief opened with the lead, and Seq that job's
	// place among the turn's launches ([PendingRun.LaunchSeq]).
	Job string `json:"job"`
	Seq int64  `json:"seq"`

	// Round is the turn iteration the lead carried on: a review's, the one it
	// closed.
	Round int `json:"round"`

	// Appended is the lead's number in the order its run's leads part took
	// leads in, from 1 ([leadsPart.Appends]): what a job's record names it by
	// ([LaunchRecord.LeadMarks]). Given by the take, outside the seal, and
	// never changed.
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

	// Answer is an answer lead's source, Task a task lead's own facts; a
	// review keeps neither.
	Answer *LeadAnswer
	Task   *LeadTask

	// Unread is why the lead did not open on this node, set only on a lead a
	// resume read beside a run's record ([PendingStore.Leads],
	// [ErrLeadKeyUnknown], [ErrLeadUnopened]), and nothing else of its source
	// is then set: it is told as a message that cannot be read here, never
	// dropped.
	Unread string
}

// LeadAnswer is an answer lead's source: what the run was told, as it was told
// it, in the parts a later brief tells again in words of its own.
//
// SEALED WHOLE ([sealedLead]): a fixed struct and no omitempty, so the bytes a
// seal covers are a function of the values alone.
type LeadAnswer struct {
	// Question is the run's own question, as its brief read it back.
	Question string `json:"question"`

	// Words is the answer's words, or a withdrawal's reason, cut to
	// [MaxLeadWordsBytes]; Cut is how many bytes of them are not kept, said
	// where the lead is told again.
	Words string `json:"words"`
	Cut   int    `json:"cut"`

	// By is who answered, "" where the route that carried the reply did not
	// say; Label who wrote the words, as the fence around a chat reply
	// labelled them, nil for an answer by turn (ADR-0040, ADR-0068).
	By    string            `json:"by"`
	Label *provenance.Label `json:"label"`

	// Withdrawn says the seat withdrew the question, or the held call, itself
	// (ADR-0111); Refused that the outcome was the refusal of a reply too
	// large to hand the run (ADR-0114).
	Withdrawn bool         `json:"withdrawn"`
	Refused   *LeadRefused `json:"refused"`

	// Held is the call a person answered, nil for a question the run asked
	// itself (ADR-0077).
	Held *LeadHeld `json:"held"`
}

// LeadRefused is a refused reply's figures, as the run was told them: how many
// messages, what they came to as the engine records an answer, and what an
// answer to a run may come to.
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
	// LeadHeldMadeUnread is a call made whose recorded result could not be
	// opened where the lead was told: the lead keeps it sealed for a node
	// that holds its key.
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
	// held call's id: what a list of the round's calls leaves out where the
	// lead is told again, the lead telling that call ([BridgeCall.HeldID]).
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

	// Result is what the call returned, as the run's row sealed it ([HeldKept]),
	// for a call made: no record holds it once the next job opens, and the lead
	// is what told it.
	Result *HeldKept `json:"result"`
}

// LeadTask is a task lead's own facts; the rest is recomposed from the turn's
// records.
type LeadTask struct {
	// Rounds is how many closed rounds the brief's prior-work block rendered:
	// the task is told again with those alone, since a round closed after it
	// is told by its own rule.
	Rounds int `json:"rounds"`

	// Reviewed is the iteration whose review the task carried (a send-back's
	// continuation of that session), 0 for none.
	Reviewed int `json:"reviewed"`
}

// MaxLeadWordsBytes bounds an answer's words as a lead keeps them
// ([LeadAnswer.Words]), cut past it with the cut said: THE PAGE AN ANSWER BY TURN
// AND A HELD CALL'S RESULT ARE HELD TO ([MaxAnswerTextBytes],
// [MaxHeldResultBytes]). A chat reply can reach the row's answer share
// ([MaxAnswerBytes]), every byte of which a lead would otherwise carry into the
// run's leads, whose whole share ([MaxKeptLeadBytes]), read by every resume,
// one such reply would overrun once sealed; the session it is told again to
// may well show it, and a run that needs the rest asks again.
const MaxLeadWordsBytes = MaxAnswerTextBytes

// MaxKeptLeadBytes bounds the leads a run's leads part keeps, as the part holds
// them ([leadsPart.Leads]): past it the oldest are dropped, each still counted
// ([leadsPart.Appends]), and the count is told wherever leads are told again.
//
// HALF A MEBIBYTE holds one lead at its worst and many ordinary ones: a held
// call's lead with its words at [MaxLeadWordsBytes] in markup the encoder
// escapes six-fold and its result at [MaxHeldResultBytes], escaped and sealed,
// sealed again inside the lead and written as base64, weighs about 300 KiB as
// the part holds it, while an answer of a few lines weighs a few KiB. And it is
// what every resume reads beside the row and what a brief to a session cut off
// can tell again, so the bound holds the newest leads, the ones such a session
// most likely lacks, rather than every lead of a long turn.
const MaxKeptLeadBytes = 512 << 10

// MaxKeptLeads bounds how many leads a run's leads part keeps, beside their
// bytes: so that the numbers a job's record names them by
// ([LaunchRecord.LeadMarks]), each a lead the part keeps, are held to a count
// by construction, at most 2,048 of them, about 40 KiB as the row holds them
// at their widest. TWO HUNDRED AND FIFTY-SIX BYTES is under the least any lead
// this build seals weighs as the part holds it: a review lead, which carries
// no source, of a one-byte turn under a one-byte key weighs 318 bytes, its
// comma counted, as
// TestTheCountOfKeptLeadsNeverDropsALeadTheirBytesWouldKeep measures it. So
// the count never drops a lead the bytes would keep of this build's; it drops
// only the many small leads of a build that seals smaller ones.
const MaxKeptLeads = MaxKeptLeadBytes / 256

// ToldLeads is the leads the brief the job's run read told, its own and every
// earlier one it told again or was to and could not read, by their numbers in
// the run's leads part ([LaunchRecord.LeadMarks]): what a continuation of the
// job's session tells again where that session may lack its brief. NONE where
// the job started a session of its own, its brief fresh or its box having
// started one in a continued one's place: a fresh session holds its opening
// message wherever it can be continued.
func (l LaunchRecord) ToldLeads() []int {
	if l.Brief == BriefFresh || l.SessionStart == SessionStartFresh {
		return nil
	}
	return slices.Clone(l.LeadMarks)
}

// BriefLeads is the leads of the brief a launch hands its job's run, as the
// attach that names the job's command keeps them ([PendingStore.AttachSandbox]).
type BriefLeads struct {
	// Run and Job are the run and the job as the launch opened the row
	// ([PendingRun.LeadsRun], [PendingRun.LaunchID]): the part of another run
	// takes none of it, and a row naming another job is marked with none.
	Run, Job string

	// Counted is how many leads the row counted for the run as the launch
	// opened it ([PendingRun.LeadsAppended]): a part the take starts anew, or
	// one holding fewer, numbers its next lead past them, so a part the store
	// lost or one replaced as unreadable never has its leads numbered over,
	// and every one of them is still counted as not kept.
	Counted int

	// Own is the leads the brief opened with of its own, sealed for the job
	// ([LeadSeal.Seal]); Told the earlier leads it told again, by name; and
	// ToldUnread the earlier ones it was to tell again and could not read, by
	// their numbers ([BriefTold.ToldUnread]), marked as Told is.
	Own        []KeptLead
	Told       []LeadRef
	ToldUnread []int
}

// TurnLeads is the turn's leads as a resume reads them beside its run's row
// ([PendingStore.Leads]): every lead of the run the part keeps, oldest first,
// and how many earlier ones are not kept: past the part's bound, and every one
// the row counts that the part could not give back ([PendingRun.LeadsAppended]).
type TurnLeads struct {
	Kept []KeptLead
	Cut  int

	// Unread says the store could not give the part back at all
	// ([Coordinator.leadsAt]): Kept is none and Cut is every lead the row
	// counts, though the part may keep each of them still. So the leads the
	// job's record marks are handed on by their numbers instead
	// ([BriefTold.ToldUnread]), marked again on the job the relaunch opens,
	// and told by the first later read that answers. False wherever the
	// store answered, for a part gone, short or another run's too: what such
	// a part lacks, no later read gives back.
	Unread bool
}

// leadsPart is the part kept beside a run's row holding the turn's leads
// ([coord.RunLeads]): what carried each session of the run on, oldest first,
// each numbered as the part took it in ([KeptLead.Appended]).
//
// THE PART IS ONE VALUE OF ITS OWN, held to [RowBytes] as the row and the log
// are ([RowBytes]: one value is one broker message), and split:
//
//	the leads it keeps .............. MaxKeptLeadBytes          524,288  (512 KiB)
//	its own fields, measured ........ measuredLeadsFields            85
//	to spare, measured .............. measuredLeadsSpare      7,860,139
//	                                                          ---------
//	                                  RowBytes                8,384,512
//
// Its own fields are the run it is of, a launch id, and how many leads it has
// taken, at their widest, with the keys and the braces around the leads; the
// spare is the room a later build adds keys to the part in ([leadsPart.Extra]).
// A key a later build adds to a LEAD comes out of the leads' own share, which a
// take holds them to with every key they carry.
type leadsPart struct {
	// Run is the run the leads are of ([PendingRun.LeadsRun]): a part of any
	// other run of the turn, which the store may leave beside a row its turn
	// created again, holds none of this run's.
	Run string `json:"run"`

	// Leads is the leads the part keeps, oldest first, held to
	// [MaxKeptLeadBytes] and [MaxKeptLeads] ([heldLeads]).
	Leads []KeptLead `json:"leads"`

	// Appends is how many leads the part has taken in all, those it no longer
	// keeps among them: the cut is Appends less what Leads holds.
	Appends int `json:"appends"`

	// Extra is every key of the part this build does not know (wirekeys.go),
	// kept through every take.
	Extra map[string]json.RawMessage `json:"-"`
}

// The two figures the part's table measures rather than shares out: what its
// own fields weigh at their worst, and what [RowBytes] leaves to spare beside
// them and its leads at their bound, each as TestTheWholeLeadsPartFitsItsBudget
// measures its model of the worst part. PINNED, as the row's are
// ([measuredOtherFields]): that test compares both exactly, and fails until a
// change that moves either restates both here and in the table.
const (
	measuredLeadsFields = 85
	measuredLeadsSpare  = 7_860_139
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

// took is p once it took in own, the leads a brief opened with of its own:
// each once by its identity ([KeptLead.Ref]), one already kept left as it is,
// a new one numbered as taken ([KeptLead.Appended]); the whole held to its
// bounds ([heldLeads]); with marks, the numbers of every lead the brief told
// that p then keeps, its own, those told names and those toldUnread numbers,
// ascending, and grew, whether it took anything in.
//
// A TOLD LEAD THE PART DOES NOT KEEP IS NOT MARKED: it is counted under
// Appends, past the bound, or the part lost it, which a reader counts against
// the row ([PendingStore.Leads]). The names and the numbers are looked up in
// sets, so a take of every lead the part keeps, told again, stays linear.
func (p leadsPart) took(own []KeptLead, told []LeadRef, toldUnread []int) (next leadsPart, marks []int, grew bool) {
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
	kept := make(map[LeadRef]bool, len(next.Leads)+len(own))
	for _, lead := range next.Leads {
		kept[lead.Ref()] = true
	}
	for _, lead := range own {
		named[lead.Ref()] = true
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
// [MaxKeptLeads], the oldest dropped first. Every lead it drops is still
// counted, under the part's [leadsPart.Appends], which no drop lowers.
//
// THE OLDEST, because the newest are the ones a session cut off most likely
// lacks: a lead older than the newest job known to hold its brief is in that
// session already. A lead the part's encoder cannot write (a carried key that
// is not JSON) is dropped first, wherever it stands: kept, it would refuse
// every write of the part.
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
