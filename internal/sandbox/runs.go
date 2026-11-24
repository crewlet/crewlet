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
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A seat's runs see and message each other (ADR-0176). A run lists the other
// runs its seat holds ([StageOf], [SameRequester]); sends one of them a
// message, filed as a wake item's entry that names its sending run
// ([SealedWake.From]), so it rides the wake items' serve, read, telling and
// end and their share of a session read's hook takes, counted against the
// receiving job's bound ([MaxJobRunMessages]) and the sending job's
// ([MaxJobRunSends], [PendingStore.CountRunSend]); and parks until another
// leaves [Holding] ([RunWait]), as a question parks it, which the engine
// answers with how that run went. ADR-0176 holds the decisions.

// RunStage is where one of a seat's runs stands, as `list_runs` and a launch's
// brief show it ([StageOf]).
type RunStage string

const (
	// StageLaunching is a run whose box is being made ready.
	StageLaunching RunStage = "launching"
	// StageRunning is a run whose harness is working, or whose turn resumed it.
	StageRunning RunStage = "running"
	// StageParkedQuestion is a run parked on a question it put to a person.
	StageParkedQuestion RunStage = "parked_on_question"
	// StageParkedHeldCall is a run parked on a call held for a person's approval.
	StageParkedHeldCall RunStage = "parked_on_held_call"
	// StageWaiting is a run parked until another run leaves its seat's slots.
	StageWaiting RunStage = "waiting_for_run"
	// StageAnswered is a run whose question is answered and whose resume waits.
	StageAnswered RunStage = "answered"
)

// RunStages is the closed set.
var RunStages = []RunStage{StageLaunching, StageRunning, StageParkedQuestion, StageParkedHeldCall,
	StageWaiting, StageAnswered}

// Valid reports whether s is one of [RunStages].
func (s RunStage) Valid() bool { return slices.Contains(RunStages, s) }

// StageOf is where run stands; "" for a status no stage names, a run ended.
func StageOf(run PendingRun) RunStage {
	switch run.Status {
	case StatusLaunching:
		return StageLaunching
	case StatusRunning, StatusResumed:
		return StageRunning
	case StatusAnswered:
		if run.WaitsOnOwnCalls() {
			return StageRunning
		}
		return StageAnswered
	case StatusAwaiting, StatusReseed:
		switch {
		case run.WaitsOnOwnCalls():
			// PARKED ON ITS OWN CALLS ALONE, which the engine answers at once
			// (callwait.go): working, to every other run, never waiting.
			return StageRunning
		case run.HeldCall != nil:
			return StageParkedHeldCall
		case run.WaitsFor != nil:
			return StageWaiting
		}
		return StageParkedQuestion
	}
	return ""
}

// SameRequester reports whether a and b work for one requester, each read as
// the ceiling composes it ([runPrincipal]): a seat by its handle, a credential
// by its login, a run's own work as its own seat's. A stranger is nobody's
// requester, so two strangers' runs are never one requester's: a run carries
// its requester's data, and words passed between two runs whose requesters
// differ, or cannot be told apart, could hand one's data to the other
// (ADR-0109).
func SameRequester(a, b PendingRun) bool {
	pa, pb := requesterOf(a), requesterOf(b)
	return pa.kind != principalStranger && pa == pb
}

// RequesterName is whom run works for as a line names them: the seat's
// handle, the credential's login, or "" for a stranger.
func RequesterName(run PendingRun) string { return requesterOf(run).id }

// requesterOf is [runPrincipal] with a run's own work read as its seat's, so
// runs of two seats are compared by whom each works for.
func requesterOf(run PendingRun) principal {
	p := runPrincipal(run)
	if p.kind == principalOwn {
		return principal{kind: principalSeat, id: run.AgentHandle}
	}
	return p
}

// The bounds of what a seat's runs pass each other, each refused naming the
// bound, never cut.
const (
	// MaxJobRunMessages bounds the messages from other runs one job takes
	// ([SealedWake.From]), within its [MaxJobWakes] wake items: TWO. A run's
	// message is information about work two runs share, a branch or a finding,
	// and one answer to it; a third between two jobs is a conversation that
	// belongs with their requester. And never more than half the job's wake
	// items, so another run's messages leave the requester's own follow-ups
	// room to join.
	MaxJobRunMessages = MaxJobWakes / 2

	// MaxJobRunSends bounds the messages one job sends other runs
	// ([PendingRun.RunSends]): TWO, the receiving bound's figure, so one job
	// answers what it was told and says what it found, and no job fills its
	// seat's runs with its own words.
	MaxJobRunSends = MaxJobRunMessages

	// MaxRunWaitReasonBytes bounds why a run waits on another ([RunWait]):
	// what the question's own bound leaves beside the engine's words before
	// it ([RunWaitQuestion]), since the reason is drawn where a parked
	// question's title is.
	MaxRunWaitReasonBytes = decision.MaxQuestion - len(runWaitPrefix)

	// MaxRunWaitHops bounds the walk that refuses a cycle of waits
	// ([WaitCycle]): EIGHT, the deepest a chain of runs each parked on the
	// next goes before a run waiting on it is refused as though it closed a
	// cycle, the safe direction. A seat's runs are held to its cap, and a
	// chain eight deep across seats is work nobody can follow.
	MaxRunWaitHops = 8
)

// runWaitPrefix opens a wait's question, the engine's words before the run's
// reason ([RunWaitQuestion]).
const runWaitPrefix = "Waiting for another run: "

// RunWaitQuestion is the question a run parked by `wait_for_run` is drawn
// under, as a parked question's title is: the engine's words, then reason.
func RunWaitQuestion(reason string) string { return runWaitPrefix + reason }

// ErrRunMessagesSpent is a message for a job holding [MaxJobRunMessages] from
// other runs already; ErrRunSendsSpent one from a job that sent
// [MaxJobRunSends].
var (
	ErrRunMessagesSpent = errors.New("sandbox: the run takes no more messages from other runs in this job")
	ErrRunSendsSpent    = errors.New("sandbox: this job sent every message to other runs it may")
)

// RunWait is what a run parked by `wait_for_run` waits on
// ([PendingRun.WaitsFor]): the run named by Turn, of the seat Seat, until it
// leaves [Holding]. Reason is the waiting run's own words; Item the work item
// the run waited for was charged to, as the person told of the wait reads it;
// SameRequester whether that run works for the waiting run's requester,
// decided at the wait while both rows are read, since the waited run's row is
// gone once it ends and only then may its report be told (ADR-0176).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go).
type RunWait struct {
	Turn          string `json:"turn"`
	Seat          string `json:"seat"`
	Reason        string `json:"reason"`
	Item          string `json:"item,omitempty"`
	SameRequester bool   `json:"same_requester"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// validate is every fault of w, named as `wait_for_run`'s arguments carry it.
func (w RunWait) validate() []error {
	var found []error
	if !validWakeID(w.Turn) || w.Seat == "" {
		found = append(found, fieldRefusal("turn_id", "names no run of a seat; give a turn id `"+
			toolname.ListRuns+"` showed"))
	}
	if strings.TrimSpace(w.Reason) == "" || len(w.Reason) > MaxRunWaitReasonBytes || !utf8.ValidString(w.Reason) {
		found = append(found, fieldRefusal("reason", fmt.Sprintf("is empty or past %d bytes: say in one "+
			"sentence why this run waits for the other", MaxRunWaitReasonBytes)))
	}
	return found
}

// MarshalJSON writes the wait with the keys a later build added to it.
func (w RunWait) MarshalJSON() ([]byte, error) {
	type plain RunWait
	return marshalCarrying(plain(w), wireKeys(reflect.TypeFor[RunWait]()), w.Extra)
}

// UnmarshalJSON reads the wait and keeps the keys this build does not know.
func (w *RunWait) UnmarshalJSON(raw []byte) error {
	type plain RunWait
	var p plain
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[RunWait]()))
	if err != nil {
		return err
	}
	p.Extra = extra
	*w = RunWait(p)
	return nil
}

// WaitCycle reports whether run waiting on target closes a cycle of waits:
// target, or a run it waits on, waits on run, following [PendingRun.WaitsFor]
// through the runs lookup finds by turn id (another seat's among them, read
// as the walk reaches it) for at most [MaxRunWaitHops] hops. A chain past the
// bound is a cycle, the safe direction: the wait is refused, never left to
// hang every run of a ring until its pause expires.
func WaitCycle(run, target string, lookup func(turn string) (PendingRun, bool)) bool {
	at := target
	for range MaxRunWaitHops {
		if at == run {
			return true
		}
		next, ok := lookup(at)
		if !ok || next.WaitsFor == nil {
			return false
		}
		at = next.WaitsFor.Turn
	}
	return true
}

// CountRunSend counts delta messages job sent other runs on turnID's record,
// within [MaxJobRunSends]. See the contract on [PendingStore].
//
// ONE LIVE WRITE, pinned to the job and [ItemStatuses]: a count for an older
// job is the job's own no longer, so the job it names starts at none.
func (s *CoordStore) CountRunSend(ctx context.Context, turnID, job string, delta int) error {
	var closed, spent bool
	_, won, found, err := s.writeFound(ctx, turnID, true, Fence{}, func(run *PendingRun) bool {
		closed, spent = false, false
		if job == "" || run.LaunchID != job || !slices.Contains(ItemStatuses, run.Status) {
			closed = true
			return false
		}
		sent := 0
		if run.RunSendsFor == job {
			sent = run.RunSends
		}
		next := max(sent+delta, 0)
		if delta > 0 && next > MaxJobRunSends {
			spent = true
			return false
		}
		run.RunSendsFor, run.RunSends = job, next
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return fmt.Errorf("sandbox: run %s sends nothing now: %w", turnID, ErrWakeClosed)
	case err != nil:
		return err
	case won:
		return nil
	case !found || closed:
		return fmt.Errorf("sandbox: run %s's job %s sends nothing now: %w", turnID, job, ErrWakeClosed)
	case spent:
		return fmt.Errorf("sandbox: run %s's job %s sent %d messages to other runs: %w", turnID, job,
			MaxJobRunSends, ErrRunSendsSpent)
	}
	return nil
}

// runMessagesIn is how many of job's wake items' entries in notes another run
// sent ([SealedWake.From]).
func runMessagesIn(notes []RunNote, job string) int {
	n := 0
	for _, note := range notes {
		if !note.IsWake() || note.For != job {
			continue
		}
		for _, w := range note.Wake.Wakes {
			if w.From != "" {
				n++
			}
		}
	}
	return n
}

// waitedOf is what the engine's carrying run on says of the run it waited for
// ([CoordinatorOptions.WaitedFor]): nil for none, and refused where the run
// waits on no run, or on another than the one named, so no run is told a wait
// ended that it never waited on. Its report and refs are redacted, as an
// answer's words are.
func waitedOf(run PendingRun, waited *types.RunWaited) (*types.RunWaited, error) {
	switch {
	case waited == nil:
		return nil, nil
	case run.WaitsFor == nil || run.WaitsFor.Turn != waited.Turn:
		return nil, fmt.Errorf("sandbox: an answer says how run %s left its seat, and run %s does not wait on it",
			waited.Turn, run.TurnID)
	case !waited.Left.Valid() || !waited.Pushed.Valid():
		return nil, fmt.Errorf("sandbox: an answer says run %s left its seat as %q, pushing %q, which this "+
			"build does not know", waited.Turn, waited.Left, waited.Pushed)
	}
	out := *waited
	out.Report = redact.Secrets(out.Report)
	out.Refs = make([]string, 0, len(waited.Refs))
	for _, ref := range waited.Refs {
		if redact.Secrets(ref) == ref {
			out.Refs = append(out.Refs, ref)
		}
	}
	return &out, nil
}

// ReadsNotes reports whether a run of harness takes a note between its steps
// here: whether the runner this node's manager holds for it reads them, which
// another run's message needs ([SealedWake.From]). False with no manager or no
// such runner.
func (c *Coordinator) ReadsNotes(harness string) bool {
	manager := c.mgr()
	if manager == nil {
		return false
	}
	runner, err := manager.RunnerFor(harness)
	if err != nil {
		return false
	}
	reader, ok := runner.(noteReader)
	return ok && reader.ReadsNotes()
}

// MaxRunsAround bounds the runs of other seats a run is shown on its work
// ([PendingStore.ListActiveOn]): EIGHT, oldest first. A launch's brief lists
// each whole on every launch, and eight other seats' runs on one item or
// conversation is more than a team works one piece of work with; past it the
// newest are the ones a run is not shown, said as the bound.
const MaxRunsAround = 8

// RunsOn is the work other seats' runs are found on (ADR-0176's amendment): the
// caller's work item by reference ([types.WorkItem.Ref]) and the items linked
// to it, its conversation by its identity, the keys of what its calls created
// or wrote, as its row holds them ([PendingRun.WroteKeys]), and the seat whose
// runs are not wanted, the caller's own, which [PendingStore.ListActiveForSeat]
// lists.
type RunsOn struct {
	Item         string
	Linked       []string
	Conversation string
	Wrote        []string
	Except       string
}

// on reports whether run, of a seat other than the one excepted, is on the
// work on names: on its item or one linked to it, in its conversation, on what
// its caller wrote, or having written its item or conversation.
func (on RunsOn) on(run PendingRun) bool {
	if run.AgentHandle == on.Except || !slices.Contains(Active, run.Status) {
		return false
	}
	if on.Conversation != "" && run.ConversationKey == on.Conversation {
		return true
	}
	if run.WorkItem != nil && (run.WorkItem.Ref() == on.Item || slices.Contains(on.Linked, run.WorkItem.Ref())) {
		return true
	}
	return on.OnWritten(run) || on.WrittenBy(run)
}

// OnWritten reports whether run is charged to an item, or in a conversation,
// that on's caller created or wrote: a ticket its calls filed, a thread they
// posted in.
func (on RunsOn) OnWritten(run PendingRun) bool {
	if run.WorkItem != nil && slices.Contains(on.Wrote, writtenKey(types.ItemWritten(*run.WorkItem))) {
		return true
	}
	return slices.Contains(on.Wrote, writtenKey(types.ConversationWritten(run.ConversationKey)))
}

// WrittenBy reports whether run's calls created or wrote on's item or
// conversation: the run that filed the ticket on's caller works on.
func (on RunsOn) WrittenBy(run PendingRun) bool {
	item := types.WrittenRecord{Kind: types.WrittenItem, ID: on.Item}
	return slices.Contains(run.WroteKeys, writtenKey(item)) ||
		slices.Contains(run.WroteKeys, writtenKey(types.ConversationWritten(on.Conversation)))
}

// writtenKeyBytes is how much of a written record's digest its key keeps
// ([writtenKey]): SIXTEEN BYTES, 128 bits, at which two records sharing a key
// is a chance no fleet meets, in half the whole digest's width.
const writtenKeyBytes = 16

// writtenKey is r as a run's row keeps it ([PendingRun.WroteKeys]), THE ONE KEY
// the append writes, the listing matches and the tests read: the first
// [writtenKeyBytes] of SHA-256 over its kind, a colon and its id (a kind holds
// none, so no two records share an input), in hex. AN EXACT MEMBERSHIP INDEX,
// never a likeness (ADR-0167), and never the authority: the log keeps the
// records. Hex at a fixed width, so a set at [types.MaxWrittenRecords] weighs
// 4.5 KiB of the row whatever its ids hold, where the records weigh up to 200
// KB escaped. "" for a record no set holds.
func writtenKey(r types.WrittenRecord) string {
	if !r.Valid() {
		return ""
	}
	sum := sha256.Sum256([]byte(string(r.Kind) + ":" + r.ID))
	return hex.EncodeToString(sum[:writtenKeyBytes])
}

// foldWrittenKeys is have with the key of each of records it lacks appended,
// in their order, and whether any was: never one dropped, and held to
// [types.MaxWrittenRecords], which the records, a log's set, already are.
func foldWrittenKeys(have []string, records []types.WrittenRecord) ([]string, bool) {
	out := have
	for _, r := range records {
		key := writtenKey(r)
		if key == "" || slices.Contains(out, key) || len(out) >= types.MaxWrittenRecords {
			continue
		}
		out = append(slices.Clip(out), key)
	}
	return out, len(out) != len(have)
}

// ListActiveOn returns the unfinished runs of other seats on the work on
// names. See the contract on [PendingStore].
//
// ONE LISTING of the fleet's runs, the read [CoordStore.ListActive] makes,
// never one per seat, nor a log read per run: what each run wrote is
// matched off its row ([PendingRun.WroteKeys]).
func (s *CoordStore) ListActiveOn(ctx context.Context, on RunsOn) ([]PendingRun, error) {
	if on.Item == "" && len(on.Linked) == 0 && on.Conversation == "" && len(on.Wrote) == 0 {
		return nil, nil
	}
	runs, err := s.list(ctx, on.on)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(runs, func(a, b PendingRun) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.TurnID, b.TurnID)
	})
	return runs[:min(len(runs), MaxRunsAround)], nil
}

// CarrierOf is whom run works for as an ask carries it ([types.WorkingFor],
// ADR-0109): what handed the run on where something did, else its requester's
// seat, else its requesting credential at the apps its writes carried, else
// its own seat's work; nil for none of these.
func CarrierOf(run PendingRun) *types.WorkingFor {
	switch {
	case run.WorkingFor != nil:
		carried := *run.WorkingFor
		return &carried
	case run.Requester != "":
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: run.Requester}
	case run.RequesterLogin != "":
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: run.RequesterLogin,
			Apps: slices.Clone(run.RequesterApps)}
	case run.AgentHandle != "":
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: run.AgentHandle}
	}
	return nil
}
