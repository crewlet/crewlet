package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A person's note to a turn whose executor is out: it waits sealed on the
// run's record (ADR-0067), the whole company's, which any node files on with
// one compare-and-swap and every reader opens before use. Every read is on the
// record, in the order it numbers them, so the record is the carry; a hook's
// serve is read only once its job's session shows it; a take whose answer was
// lost is settled by its name; a poll answers a note only once the record says
// it was served; a confirmation the inbox cannot take is turned away to ask
// again; the run's ending reports what no step did; and an item for one job is
// that job's alone, a wake's sealed in a domain of its own (wakeitem.go). The
// package doc's sections from "A note to a turn whose executor is out" to "A
// late call is handed to the job working as it lands" hold the decisions and
// why.

// SteerNoteDomain is the key domain a note on a run's record is sealed in: its
// own, so no frame of another channel or a state log opens as a note, and no
// note as anything else.
const SteerNoteDomain = "seat.steer.note"

// SteerSeatDomain is the key domain a run record's seat anchor is sealed in.
const SteerSeatDomain = "seat.steer.seat"

// SteerWakeDomain is the key domain a wake on a run's record is sealed in: its
// own, so no note opens as a wake nor a wake as a person's note.
const SteerWakeDomain = "seat.wake.item"

// SteerRunDomain is the key domain a message one run sends another with
// `message_run` is sealed in on the receiving run's record (ADR-0176): its own,
// so no wake opens as a run's message nor a run's message as a wake.
const SteerRunDomain = "seat.run.message"

// steerTakeStatuses are the statuses a run's own harness takes notes in: from the
// launch that opens its job — the job may be working before its turn has
// parked into the record — to the claim of its completion.
var steerTakeStatuses = []string{sandbox.StatusLaunching, sandbox.StatusRunning}

// steerDeliveredNamespace and steerEndedNamespace derive the ids a note's
// records are published under: delivered by a run's inbox, from the turn and
// the note, since a note is read once; and whatever an ending reports of it,
// from the turn, the ending and the note, never the outcome, since an ending
// reports a note once and an older build's ending of the same record reported
// it under the same id.
var (
	steerDeliveredNamespace = uuid.MustParse("3c7a9e5d-4f1b-5a2c-8e6d-9b0f1a2c3d4e")
	steerEndedNamespace     = uuid.MustParse("8d2e6f1a-7b3c-5d4e-9f0a-1b2c3d4e5f6a")
)

// steerPublishBudget bounds one publish of what became of a note, made off the
// path that read it; and it is the reason a job's pull mark on its run's row is
// bounded by ([runInbox.recordPulled]), beside the time the row takes to cross
// to the store (sandbox.RowBudget), which a publish of a note's record never
// carries.
const steerPublishBudget = 5 * time.Second

// steerSettlePatience is how long a settle the store has not answered is taken
// for a blip ([runSteers.settleUnanswered]): past it, the settle is said
// unsettled (steer_take_unsettled) and goes on asking, and a review stops
// holding its rounds open for one ([rowSource.Waiting]).
//
// THIRTY SECONDS outlasts a store mid leader election (four seconds at
// nats-server's raft default, nine at worst) or reconnecting; a store silent
// past it is an outage its own alarms name. SAID ONCE AN ATTEMPT HAS FAILED,
// never while one is still asking: an attempt gets its ten seconds plus the
// row's crossing ([steerSettleAttempt]), so on the slowest allowed link the
// line can come about thirteen minutes in.
const steerSettlePatience = 30 * time.Second

// steerSettleAttempt is the reason one attempt at a settle is bounded by,
// beside the time the run's row takes to cross to the store
// (sandbox.RowBudget), since a settle writes the row whole: an attempt the
// store has not answered in TEN SECONDS beyond that — past a leader election's
// nine at worst — is asked again, so a request that hangs never holds a run's
// settle, nor keeps it from hearing that its node has stopped, which ends an
// attempt at once ([runSteers.settleOnce]). Held to the ten alone, a row of
// megabytes across a slow link was cut while its bytes were still crossing and
// asked again from its first byte, never settled (crewlet/crewlet#1076).
const steerSettleAttempt = 10 * time.Second

// steerSettleRetryBase and steerSettleRetryCeiling space a run's rounds of
// settle attempts: a second, doubling to eight, for as long as its store does
// not answer — a round every eight seconds at most is a few reads of one
// record a minute, which a store just back from an outage barely notices.
const (
	steerSettleRetryBase    = time.Second
	steerSettleRetryCeiling = 8 * time.Second
)

// steerSettleStopBudget is the window a stopping node's last attempts at the
// settles it holds are made in ([runSteers.settleRun]), all runs together from
// the start of teardown, so the wait ([runSteers.waitSettles]) ends within it.
//
// A WINDOW OF ITS OWN, NEVER A STEP OF THE STOP'S ALLOWANCE (internal/seat's
// StopBudget, "Only what lapses"): charged to it, settles held by a stream that
// lost quorum would spend what later give-backs are owed, and those leases
// would lapse on their TTL. FIVE SECONDS: the JetStream client's bound for a
// request with no deadline, and the ledgers' last-flush bound
// ([ledgerStopBudget]), so the stop's longest case is unchanged. NO CROSSING OF
// THE ROW beside it ([steerSettleAttempt]): minutes would outlast a pod's grace.
// A last attempt the window cuts leaves its take unsettled, which the stop says.
const steerSettleStopBudget = 5 * time.Second

// runSteers is this node's half of a turn's notes on its run's record: it seals
// and opens them, files them for a person, takes them for a resumed segment and
// for a run's own harness, and records what became of them.
type runSteers struct {
	note *sealPair
	seat *sealPair
	wake *sealPair
	run  *sealPair

	store sandbox.PendingStore
	pub   queue.Publisher
	now   func() time.Time

	// completions is the completion ledger a wake a step of a run read is
	// recorded worked in ([runSteers.claim]); nil records nothing.
	completions ledgerstore.Completions

	// seen is the seen-through marks a wake a step read moves ([runSteers.seeThrough]);
	// nil moves none.
	seen *ledgerstore.SeenThrough

	// handedOver is told, for a seat, the deliveries a step of one of its runs
	// read as wakes once they are claimed ([Engine.settleWaiting]): a person's
	// message queued behind the busy seat is queued no longer. Nil tells nobody.
	handedOver func(ctx context.Context, handle string, triggers []string)

	// warned is every note whose seal this node has logged it cannot open, so
	// a view that follows a busy record does not say so on every write.
	warned sync.Map

	// turnedAway is every note a confirmation named that this node has logged
	// it turned away, the note sealed under a key it does not hold
	// ([runSteers.waitsForItsKey]), so a harness that confirms it again on its
	// own backoff for as long as its run lasts does not have it said at every
	// try.
	turnedAway sync.Map

	// stopped is closed when this node's teardown begins
	// ([Engine.teardownSignal]), and stopWindow ends [steerSettleStopBudget]
	// after it: a settle this node makes off the path that took asks its store
	// until the store answers, or until the teardown begins — and then once
	// more, in that window ([runSteers.settleRun]). Nil is a node that never
	// stops, as an engine assembled by hand is. Both set by
	// [runSteers.stopWith].
	stopped    <-chan struct{}
	stopWindow context.Context

	// settleMu guards settling and settlesShut: every run this node is
	// settling takes of off the path that took, by turn id — one loop a run,
	// whatever its takes — and whether the teardown has waited the loops out
	// ([runSteers.waitSettles]), after which no loop starts.
	settleMu    sync.Mutex
	settling    map[string]*runSettle
	settlesShut bool

	// settles is the settle loops running, which the teardown waits out.
	settles sync.WaitGroup
}

// runSettle is what this node is settling of one run's notes off the path that
// took ([runSteers.joinSettle]): its review's takes to mark, by name, ONE
// pending hook settle a job, when the first ask came, whether the loop said it
// unsettled, and the takes a round begun once the node stopped has asked —
// each its last attempt ([runSteers.settleRun]).
type runSettle struct {
	takes []string
	hooks map[string]*hookSettle
	since time.Time
	said  bool
	tried []string
}

var _ builtin.RunSteers = (*runSteers)(nil)

// sealPair is one key domain's seal and its opener.
type sealPair struct {
	signer   *recordseal.Signer
	verifier *recordseal.Verifier
}

func newSealPair(domain string, ring recordseal.Keyring) (*sealPair, error) {
	signer, err := recordseal.NewSigner(domain, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the seal on %s: %w", domain, err)
	}
	verifier, err := recordseal.NewVerifier(domain, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the seal on %s: %w", domain, err)
	}
	return &sealPair{signer: signer, verifier: verifier}, nil
}

// steerSeals is the four seals a run's record carries notes, wakes and other
// runs' messages under, built over the fleet keyring before anything is
// opened, as every channel's are.
type steerSeals struct{ note, seat, wake, run *sealPair }

func newSteerSeals(ring recordseal.Keyring) (steerSeals, error) {
	note, err := newSealPair(SteerNoteDomain, ring)
	if err != nil {
		return steerSeals{}, err
	}
	seat, err := newSealPair(SteerSeatDomain, ring)
	if err != nil {
		return steerSeals{}, err
	}
	wake, err := newSealPair(SteerWakeDomain, ring)
	if err != nil {
		return steerSeals{}, err
	}
	run, err := newSealPair(SteerRunDomain, ring)
	if err != nil {
		return steerSeals{}, err
	}
	return steerSeals{note: note, seat: seat, wake: wake, run: run}, nil
}

// Domains is the key domains the seals sign in, for the gate that holds every
// channel's domains apart.
func (s steerSeals) Domains() (string, string, string, string) {
	return SteerNoteDomain, SteerSeatDomain, SteerWakeDomain, SteerRunDomain
}

// newRunSteers is a node's run steers over its run store and its queue.
func newRunSteers(seals steerSeals, store sandbox.PendingStore, pub queue.Publisher) *runSteers {
	return &runSteers{note: seals.note, seat: seals.seat, wake: seals.wake, run: seals.run, store: store, pub: pub,
		now: time.Now}
}

// claimingIn is s recording the wakes its runs read in completions, the
// dispatcher's own ledger, which drops such a wake's delivery as worked
// ([runSteers.claim]).
func (s *runSteers) claimingIn(completions ledgerstore.Completions) *runSteers {
	s.completions = completions
	return s
}

// seeingThrough is s moving the seat's seen-through marks in seen for every
// wake its runs read ([runSteers.seeThrough]).
func (s *runSteers) seeingThrough(seen *ledgerstore.SeenThrough) *runSteers {
	s.seen = seen
	return s
}

// telling has s tell handedOver every delivery a run read as a wake, once
// claimed ([runSteers.handedOver]).
func (s *runSteers) telling(handedOver func(ctx context.Context, handle string, triggers []string)) {
	s.handedOver = handedOver
}

// RunSteers is where a person's note to a turn whose executor is out is filed
// ([builtin.SteerDeps.Runs]); nil on an engine assembled without one.
func (e *Engine) RunSteers() builtin.RunSteers {
	if e.runSteers == nil {
		return nil
	}
	return e.runSteers
}

// noteSealBody is the body a note's seal covers. A FIXED STRUCT AND NO omitempty,
// so the bytes are a function of the values alone.
type noteSealBody struct {
	TurnID     string    `json:"turn_id"`
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	By         string    `json:"by"`
	ByKind     string    `json:"by_kind"`
	OperatorID string    `json:"operator_id"`
	At         time.Time `json:"at"`
	Owes       string    `json:"owes"`
}

// seatSealBody is the body a record's seat anchor covers.
type seatSealBody struct {
	TurnID  string `json:"turn_id"`
	AgentID string `json:"agent_id"`
	Handle  string `json:"handle"`
}

// seal is note as a run's record holds it, sealed to turnID and the note's id.
func (s *runSteers) seal(turnID string, note steer.Note) (_ sandbox.RunNote, err error) {
	defer origin.Stamp(&err)
	if note.At.IsZero() {
		note.At = s.now().UTC()
	}
	body, err := json.Marshal(noteSealBody{
		TurnID: turnID, ID: note.ID, Text: note.Text, By: note.By, ByKind: string(note.ByKind),
		OperatorID: note.OperatorID, At: note.At.UTC(), Owes: note.Owes,
	})
	if err != nil {
		return sandbox.RunNote{}, fmt.Errorf("engine: sealing a note to turn %s: %w", turnID, err)
	}
	return sandbox.RunNote{ID: note.ID, Sealed: s.note.signer.Seal(body), At: note.At.UTC()}, nil
}

// open is a note on turnID's record, opened, and whether it opened. A note it
// cannot open is logged once, at WARN where this node lacks the key it is
// sealed under and at ERROR where it does not open under a key this node holds
// or opens as another turn's or another note's.
func (s *runSteers) open(turnID string, n sandbox.RunNote) (steer.Note, bool) {
	body, verdict := s.note.verifier.Open(n.Sealed)
	switch verdict {
	case recordseal.Verified:
	case recordseal.KeyUnknown:
		s.warnOnce(turnID, n.ID, func() {
			log.Warn("steer_note_key_unknown", "turn_id", turnID, "note_id", n.ID,
				"held_keys", s.note.verifier.KeyIDs(),
				"detail", "the note is sealed under a key this node does not hold, so it waits; "+
					"add the key to secrets.keys, or a node that holds it reads the note")
		})
		return steer.Note{}, false
	default:
		s.unverified(turnID, n.ID, "the frame does not verify under the key it names")
		return steer.Note{}, false
	}
	var sealed noteSealBody
	if err := json.Unmarshal(body, &sealed); err != nil {
		s.unverified(turnID, n.ID, "the sealed body does not decode")
		return steer.Note{}, false
	}
	if sealed.TurnID != turnID || sealed.ID != n.ID {
		s.unverified(turnID, n.ID, fmt.Sprintf("it was sealed as note %q of turn %q",
			sealed.ID, sealed.TurnID))
		return steer.Note{}, false
	}
	return steer.Note{ID: sealed.ID, Text: sealed.Text, By: sealed.By,
		ByKind: iam.ActorKind(sealed.ByKind), OperatorID: sealed.OperatorID, At: sealed.At,
		Owes: sealed.Owes}, true
}

func (s *runSteers) unverified(turnID, noteID, why string) {
	s.warnOnce(turnID, noteID, func() {
		log.Error("steer_note_unverified", "turn_id", turnID, "note_id", noteID, "detail",
			why+"; it is never handed to a model, carried or recorded as anybody's")
	})
}

func (s *runSteers) warnOnce(turnID, noteID string, say func()) {
	if _, seen := s.warned.LoadOrStore(turnID+"/"+noteID, struct{}{}); !seen {
		say()
	}
}

// anchor is the seat a run serves, sealed for its record ([sandbox.RunSteer.Seat]).
func (s *runSteers) anchor(turnID, agentID, handle string) []byte {
	if s == nil {
		return nil
	}
	body, err := json.Marshal(seatSealBody{TurnID: turnID, AgentID: agentID, Handle: handle})
	if err != nil {
		// Unreachable: three strings.
		return nil
	}
	return s.seat.signer.Seal(body)
}

// openSeat is the seat run's anchor names, or why it names none anybody can
// vouch for ([builtin.RunSeat.Unverified]): a VERDICT, which the steer tool
// words for each of its readers, and never a sentence written here. A sentence
// this node wrote was handed whole to the tool's caller, and a person steering
// from the dashboard read none of it, answered in the refusal's code alone
// (ADR-0060).
//
// AN ANCHOR UNDER A KEY THIS NODE DOES NOT HOLD IS THE OPERATOR'S TO FIX, a
// condition no wait clears ([steer.ErrSeatKeyUnknown]): the keys held and the
// advice go to the log, where the keyring's administrator reads them, and
// nothing a steerer reads names them.
func (s *runSteers) openSeat(run sandbox.PendingRun) builtin.RunSeat {
	if len(run.Steer.Seat) == 0 {
		return builtin.RunSeat{Unverified: steer.ErrSeatUnnamed}
	}
	body, verdict := s.seat.verifier.Open(run.Steer.Seat)
	switch verdict {
	case recordseal.Verified:
	case recordseal.KeyUnknown:
		log.Warn("steer_seat_key_unknown", "turn_id", run.TurnID,
			"held_keys", s.seat.verifier.KeyIDs(),
			"detail", "the seat its run's record names is sealed under a key this node does not "+
				"hold, so no authority is decided on it and nothing is filed; add the key the "+
				"fleet signs with to this node's secrets.keys")
		return builtin.RunSeat{Unverified: steer.ErrSeatKeyUnknown}
	default:
		log.Error("steer_seat_unverified", "turn_id", run.TurnID,
			"detail", "the seat its run's record names does not verify under the key it names; "+
				"no authority is decided on it")
		return builtin.RunSeat{Unverified: steer.ErrSeatUnverified}
	}
	var sealed seatSealBody
	if err := json.Unmarshal(body, &sealed); err != nil || sealed.TurnID != run.TurnID {
		log.Error("steer_seat_unverified", "turn_id", run.TurnID,
			"detail", "the seat its run's record names was sealed for another turn; no authority "+
				"is decided on it")
		return builtin.RunSeat{Unverified: steer.ErrSeatUnverified}
	}
	return builtin.RunSeat{Agent: sealed.AgentID, Handle: sealed.Handle}
}

// Seat is the seat a turn's run record names — see [builtin.RunSteers].
func (s *runSteers) Seat(ctx context.Context, turnID string) (builtin.RunSeat, bool, error) {
	run, found, err := s.store.Get(ctx, turnID)
	if err != nil || !found {
		return builtin.RunSeat{}, false, err
	}
	return s.openSeat(run), true, nil
}

// File seals a person's note and files it on the turn's run record — see
// [builtin.RunSteers].
func (s *runSteers) File(ctx context.Context, turnID string, note steer.Note) (builtin.RunFiled, error) {
	sealed, err := s.seal(turnID, note)
	if err != nil {
		return builtin.RunFiled{}, err
	}
	filed, run, err := s.store.FileNote(ctx, turnID, sandbox.NoteFiling{
		Note: sealed, MaxWaiting: steer.MaxPendingNotes, MaxNotes: steer.MaxTurnNotes,
	})
	if err != nil {
		return builtin.RunFiled{}, err
	}
	out := builtin.RunFiled{Reads: readsOf(run.Status), AgentHandle: run.AgentHandle}
	switch filed {
	case sandbox.NoteAccepted, sandbox.NoteDuplicate:
		out.Status = steer.StatusAccepted
	case sandbox.NoteFull:
		out.Status = steer.StatusFull
	case sandbox.NoteSpent:
		out.Status = steer.StatusSpent
	default:
		out.Status = steer.StatusClosed
	}
	if out.Status == steer.StatusAccepted && filed == sandbox.NoteAccepted {
		log.InfoContext(ctx, "steer_note_filed", "turn_id", turnID, "note_id", note.ID,
			"seat", run.AgentHandle, "status", run.Status)
	}
	return out, nil
}

// readsOf is when a note a run in status will read: its next step while the
// run works or is being collected, and after the answer while it is parked on a
// question.
func readsOf(status string) steer.Reads {
	if slices.Contains(sandbox.Awaiting, status) || status == sandbox.StatusAnswered {
		return steer.ReadsAfterAnswer
	}
	return steer.ReadsNextStep
}

// briefNotes is notes a launch's brief carries, sealed for the record the
// launch opens and marked read at the brief — the launch names the job
// ([sandbox.LaunchRequest.BriefNotes]) — each one a lost take's settle handed
// the brief of from, the segment that briefed, naming that take, whose read
// the launch's write takes over ([rowSource.lostTakes]). from is nil for a
// first segment, whose brief's notes are its box's.
func (s *runSteers) briefNotes(turnID string, round int, notes []steer.Note, from *rowSource,
) ([]sandbox.RunNote, error) {
	if s == nil || len(notes) == 0 {
		return nil, nil
	}
	lost := from.lostTakes(notes)
	out := make([]sandbox.RunNote, 0, len(notes))
	for _, n := range notes {
		sealed, err := s.seal(turnID, n)
		if err != nil {
			return nil, err
		}
		sealed.Read = briefRead(round)
		sealed.LostTake = lost[n.ID]
		out = append(out, sealed)
	}
	return out, nil
}

// retoldNotes is the notes a launch's brief tells again, by id, each read at
// the brief ([sandbox.LaunchRequest.RetoldNotes]): read notes whose read the
// record marked unanswered as the brief was built, which the launch's write
// records read by the job it opens, told again. Never sealed: the record holds
// each note already, and the write finds it by its id.
func retoldNotes(round int, ids []string) []sandbox.RunNote {
	out := make([]sandbox.RunNote, 0, len(ids))
	for _, id := range ids {
		out = append(out, sandbox.RunNote{ID: id, Read: briefRead(round)})
	}
	return out
}

// briefRead is a read at the brief of the launch of round: the executor's
// phase, at its one round, the job named by the launch's write.
func briefRead(round int) *sandbox.NoteRead {
	return &sandbox.NoteRead{Phase: string(types.PhaseExecute), Iteration: round, Round: 1,
		Boundary: string(types.SteerBoundaryBrief)}
}

// handedNotes is the notes a first segment's box held unread as its launch
// began, sealed for the record the launch opens, which files them waiting
// ([sandbox.LaunchRequest.HandedNotes]).
func (s *runSteers) handedNotes(turnID string, notes []steer.Note) ([]sandbox.RunNote, error) {
	if s == nil || len(notes) == 0 {
		return nil, nil
	}
	out := make([]sandbox.RunNote, 0, len(notes))
	for _, n := range notes {
		sealed, err := s.seal(turnID, n)
		if err != nil {
			return nil, err
		}
		out = append(out, sealed)
	}
	return out, nil
}

// readOf is where a note is read, as a run's record holds it.
func readOf(at steer.Reading) sandbox.NoteRead {
	return sandbox.NoteRead{Phase: string(at.Phase), Iteration: at.Iteration, Round: at.Round,
		Boundary: string(at.Boundary), Launch: at.Launch}
}

// readingOf is a record's read as the agent layer states it.
func readingOf(read sandbox.NoteRead) steer.Reading {
	return steer.Reading{Phase: types.Phase(read.Phase), Iteration: read.Iteration, Round: read.Round,
		Boundary: types.SteerBoundary(read.Boundary), Launch: read.Launch, At: read.At, Order: read.Order,
		Unanswered: read.Unanswered, Restarted: read.Restarted}
}

// takeName names one take of a run's notes ([sandbox.NoteRead.Take]): fresh for
// every call, so a settle finds the reads of that call and no other's.
func takeName() string { return uuid.NewString() }

// settleUnanswered settles, off the path that took, a review's take of
// turnID's notes whose answer never reached its reader
// ([sandbox.PendingStore.SettleTake]): every note its write recorded is marked
// unanswered, and a write of it still in flight can no longer land. It joins
// the run's one settle loop ([runSteers.settleRun]).
func (s *runSteers) settleUnanswered(ctx context.Context, turnID, take string) {
	s.joinSettle(ctx, turnID, func(run *runSettle) {
		if !slices.Contains(run.takes, take) {
			run.takes = append(run.takes, take)
		}
	})
}

// settleHook settles, off the hook's path, what a run inbox learned of its
// job's hooks' serves once the hook was answered
// ([sandbox.PendingStore.SettleHook]): merged into the one settle the run's
// loop holds for that job ([runSettle.hooks]), so a box that loses serves in
// a burst makes the loop ask its store once a round, whatever it loses.
func (s *runSteers) settleHook(ctx context.Context, turnID string, settle sandbox.HookSettle) {
	s.joinSettle(ctx, turnID, func(run *runSettle) {
		if run.hooks == nil {
			run.hooks = map[string]*hookSettle{}
		}
		held, ok := run.hooks[settle.Launch]
		if !ok {
			held = &hookSettle{settle: sandbox.HookSettle{Launch: settle.Launch}}
			run.hooks[settle.Launch] = held
		}
		held.merge(settle)
	})
}

// joinSettle hands join the run's settle loop's state, starting the loop where
// the run has none ([runSteers.settleRun]).
//
// UNTIL THE STORE ANSWERS, never until a clock says so: a settle given up leaves
// the record saying what its reader was never handed, while the store that did
// not answer in time answers once it is back. Said at ERROR once
// [steerSettlePatience] has passed unanswered, and again where the node stops
// with it unsettled, since no other node knows to settle it. A settle asked
// once the teardown has waited the loops out ([runSteers.waitSettles]) is said
// so at once: the store it would be asked of is closing. ONE LOOP A RUN,
// whatever it settles; it keeps ctx's values (the first ask's trace) and none
// of its ending.
func (s *runSteers) joinSettle(ctx context.Context, turnID string, join func(*runSettle)) {
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	if run, ok := s.settling[turnID]; ok {
		join(run)
		return
	}
	if s.settlesShut {
		probe := &runSettle{}
		join(probe)
		log.ErrorContext(ctx, "steer_take_unsettled", "turn_id", turnID, "settles", probe.left(),
			"detail", "a settle of the run's notes was asked once this node had stopped settling: where "+
				"the store recorded what it settles, the record says the run's harness was handed notes it "+
				"never had, and no other node knows to settle it")
		return
	}
	if s.settling == nil {
		s.settling = map[string]*runSettle{}
	}
	run := &runSettle{since: s.now()}
	join(run)
	s.settling[turnID] = run
	s.settles.Add(1)
	go func() {
		defer s.settles.Done()
		s.settleRun(context.WithoutCancel(ctx), turnID, run)
	}()
}

// settleRun asks turnID's store to settle what run holds, round after round,
// until it has answered for all of it, or the node stops — see
// [runSteers.joinSettle]. base is the loop's own context, which nothing ends:
// an attempt is held to [steerSettleAttempt], and to the window the node's
// teardown opens too ([steerSettleStopBudget], [runSteers.attempt]).
//
// ONE LAST ATTEMPT AT EVERY SETTLE AS THE NODE STOPS, never none: the round in
// flight as the teardown begins goes on, held to the window; a loop asleep
// between rounds wakes; and a round begun once the teardown has asks every
// settle no such round has asked yet, after which what is left is said
// unsettled, and the loop ends.
func (s *runSteers) settleRun(base context.Context, turnID string, run *runSettle) {
	for round := 1; ; round++ {
		last := s.stopping()
		s.settleMu.Lock()
		takes, hooks := run.asks(last)
		if last && len(takes) == 0 && len(hooks) == 0 {
			delete(s.settling, turnID)
			left := run.left()
			s.settleMu.Unlock()
			log.ErrorContext(base, "steer_take_unsettled", "turn_id", turnID, "settles", left,
				"detail", "this node stopped before the store answered a settle of the run's notes, its "+
					"last attempt included, made in the window that opened as the stop's teardown began "+
					"(none for a settle asked once the window was over): where the store recorded what it "+
					"settles, the record says the run's harness was handed notes it never had, and no "+
					"other node knows to settle it")
			return
		}
		s.settleMu.Unlock()
		var answered []string
		var settled []sandbox.HookSettle
		var unanswered error
		for _, take := range takes {
			marked, err := s.settleOnce(base, turnID, sandbox.NoteSettle{Take: take, Unanswered: true})
			if err != nil {
				unanswered = err
				continue
			}
			answered = append(answered, take)
			sayUnanswered(base, turnID, marked)
		}
		for _, settle := range hooks {
			lost, err := s.settleHookOnce(base, turnID, settle)
			if err != nil {
				unanswered = err
				continue
			}
			settled = append(settled, settle)
			sayServesLost(base, turnID, settle.Launch,
				servesOfCalls(lost, settle.Launch, settle.Cancelled, sandbox.ServeCancelled))
		}
		s.settleMu.Lock()
		run.answered(answered, settled)
		left := run.left()
		if left == 0 {
			delete(s.settling, turnID)
			s.settleMu.Unlock()
			return
		}
		// SAID ONLY OF A ROUND THAT MET AN ERROR: what is left of one whose
		// every attempt was answered joined the loop while the round was
		// asked. And never of a last round, whose end says what is left.
		overdue := !last && unanswered != nil && !run.said && s.now().Sub(run.since) >= steerSettlePatience
		run.said = run.said || overdue
		s.settleMu.Unlock()
		if overdue {
			log.ErrorContext(base, "steer_take_unsettled", "turn_id", turnID, "settles", left,
				"error", unanswered.Error(),
				"detail", "the store has not answered a settle of the run's notes: where it recorded what "+
					"the settle undoes, the record says the run's harness was handed notes it never had, and "+
					"a continuation of its session is not told them. Asked again until the store answers")
		}
		if last {
			continue
		}
		timer := time.NewTimer(backoff.Jitter(backoff.Doubling(round, steerSettleRetryBase,
			steerSettleRetryCeiling), 0.2))
		select {
		case <-timer.C:
		case <-s.stopped:
			timer.Stop()
		}
	}
}

// servesOfCalls counts the unread notes of notes holding a serve to launch
// that answered one of calls and whose loss is lost: "" for a live serve,
// what a serve's write naming the calls loses ([sandbox.HookServe.Cancelled]),
// and [sandbox.ServeCancelled] for one a settle's answer shows lost.
func servesOfCalls(notes []sandbox.RunNote, launch string, calls []sandbox.NoteCall,
	lost sandbox.ServeLoss) int {
	n := 0
	for _, note := range notes {
		if note.Read != nil || (lost == "" && note.Served != launch) {
			continue
		}
		if slices.ContainsFunc(note.Serves, func(h sandbox.HookServed) bool {
			return h.Launch == launch && h.Lost == lost && h.Call != nil && slices.ContainsFunc(calls, h.Call.Is)
		}) {
			n++
		}
	}
	return n
}

// sayServesLost says that notes of launch's serves were lost to its harness's
// cancels, where any were: by the settle loop's write or a serve's own.
func sayServesLost(ctx context.Context, turnID, launch string, notes int) {
	if notes == 0 {
		return
	}
	log.InfoContext(ctx, "steer_serve_lost", "turn_id", turnID, "launch_id", launch, "why", "cancelled",
		"notes", notes, "detail", "the run's harness gave up a hook's call whose answer served notes: they "+
			"wait to be served again at the job's next step")
}

// asks is what a round asks of run's settles: every review take and every
// job's hook settle, or, in a round begun once the node stopped (last), only
// those no such round asked yet, which it marks asked. Under settleMu.
func (run *runSettle) asks(last bool) ([]string, []sandbox.HookSettle) {
	takes := slices.Clone(run.takes)
	if last {
		takes = slices.DeleteFunc(takes, func(t string) bool { return slices.Contains(run.tried, t) })
		run.tried = append(run.tried, takes...)
	}
	var hooks []sandbox.HookSettle
	for _, launch := range slices.Sorted(maps.Keys(run.hooks)) {
		held := run.hooks[launch]
		if last && held.tried {
			continue
		}
		held.tried = held.tried || last
		hooks = append(hooks, held.copied())
	}
	return takes, hooks
}

// answered drops from run what the store answered: the review takes named,
// and of each job's hook settle what the answered copy held, so what joined
// it while the round asked is asked by the next. Under settleMu.
func (run *runSettle) answered(takes []string, hooks []sandbox.HookSettle) {
	run.takes = slices.DeleteFunc(run.takes, func(t string) bool { return slices.Contains(takes, t) })
	for _, settle := range hooks {
		held := run.hooks[settle.Launch]
		if held == nil {
			continue
		}
		held.drop(settle)
		if held.empty() {
			delete(run.hooks, settle.Launch)
		}
	}
}

// left is how much run still has to settle: each review take, and each take
// and call a job's hook settle names. Under settleMu.
func (run *runSettle) left() int {
	n := len(run.takes)
	for _, held := range run.hooks {
		n += len(held.settle.Unanswered) + len(held.settle.Cancelled)
	}
	return n
}

// hookSettle is one job's pending settle of its hooks' serves in a run's loop
// ([runSettle.hooks]), and whether a round begun once the node stopped has
// asked it as it stands.
type hookSettle struct {
	settle sandbox.HookSettle
	tried  bool
}

// merge adds what settle names to the pending one, each once: a newer ask is
// one more fact about the job, never a replacement of what is still unasked.
// Each list keeps its newest [steerLossesKept], as the inbox's rings do.
func (h *hookSettle) merge(settle sandbox.HookSettle) {
	for _, take := range settle.Unanswered {
		if !slices.Contains(h.settle.Unanswered, take) {
			h.settle.Unanswered = kept(append(h.settle.Unanswered, take))
			h.tried = false
		}
	}
	for _, call := range settle.Cancelled {
		if !slices.ContainsFunc(h.settle.Cancelled, call.Is) {
			h.settle.Cancelled = kept(append(h.settle.Cancelled, call))
			h.tried = false
		}
	}
}

// drop removes from the pending settle what the answered copy asked.
func (h *hookSettle) drop(asked sandbox.HookSettle) {
	h.settle.Unanswered = slices.DeleteFunc(h.settle.Unanswered, func(t string) bool {
		return slices.Contains(asked.Unanswered, t)
	})
	h.settle.Cancelled = slices.DeleteFunc(h.settle.Cancelled, func(c sandbox.NoteCall) bool {
		return slices.ContainsFunc(asked.Cancelled, c.Is)
	})
}

func (h *hookSettle) empty() bool {
	return len(h.settle.Unanswered) == 0 && len(h.settle.Cancelled) == 0
}

// copied is the pending settle as a round asks it, its lists its own.
func (h *hookSettle) copied() sandbox.HookSettle {
	out := h.settle
	out.Unanswered = slices.Clone(h.settle.Unanswered)
	out.Cancelled = slices.Clone(h.settle.Cancelled)
	return out
}

// settleOnce is one attempt at a review's or a confirmation's settle
// ([runSteers.attempt]): the settle loop's attempts mark
// ([runSteers.settleRun]); a confirmation's answers ([runInbox.confirm]).
func (s *runSteers) settleOnce(base context.Context, turnID string, settle sandbox.NoteSettle) (
	[]sandbox.RunNote, error) {
	ctx, cancel := s.attempt(base)
	defer cancel()
	return s.store.SettleTake(ctx, turnID, settle)
}

// settleHookOnce is one attempt at a job's hook settle ([runSteers.attempt]).
func (s *runSteers) settleHookOnce(base context.Context, turnID string, settle sandbox.HookSettle) (
	[]sandbox.RunNote, error) {
	ctx, cancel := s.attempt(base)
	defer cancel()
	return s.store.SettleHook(ctx, turnID, settle)
}

// attempt is the context of one attempt at a settle, held to
// [steerSettleAttempt] and the time the run's row takes to cross to the store
// (sandbox.RowBudget), and to the window this node's teardown opens
// ([runSteers.stopWindow]): the teardown waits for it ([runSteers.waitSettles]),
// and a store that does not answer must hold the stop no longer than that
// window, never on the stop's allowance ([steerSettleStopBudget]). One begun
// once the window is over has already ended, so it fails at once.
func (s *runSteers) attempt(base context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(base, sandbox.RowBudget(s.store, steerSettleAttempt))
	window := s.stopWindow
	if window == nil {
		return ctx, cancel
	}
	//nolint:contextcheck // the window is the node's teardown's, never base's: it ends the attempt beside base's own bound, and is ended by nothing base holds
	stop := context.AfterFunc(window, cancel)
	if window.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

// sayUnanswered says a settle marked the notes a take recorded unanswered,
// where it marked any.
func sayUnanswered(ctx context.Context, turnID string, marked []sandbox.RunNote) {
	if len(marked) == 0 {
		return
	}
	log.WarnContext(ctx, "steer_take_unanswered", "turn_id", turnID, "notes", len(marked),
		"detail", "a take of the run's notes was recorded and its answer lost: its reader never "+
			"had them, the turn carries them as read, and a launch that continues the run's "+
			"session tells them again")
}

// stopping reports whether this node's teardown has begun ([runSteers.stopped]).
func (s *runSteers) stopping() bool {
	select {
	case <-s.stopped:
		return true
	default:
		return false
	}
}

// stopWith ends this node's settles with e's teardown, as [New] wires them: the
// moment it begins ([Engine.teardownSignal]), and the window every attempt is
// held to from then on, which ends [steerSettleStopBudget] after that moment
// ([windowFrom]) — one window for every settle the node holds, opened as the
// teardown begins whether or not one is being asked then, so a take handed off
// later in the stop is asked inside it too.
func (s *runSteers) stopWith(e *Engine) {
	begun := e.teardownSignal()
	s.stopped, s.stopWindow = begun.Done(), windowFrom(begun, steerSettleStopBudget)
}

// windowFrom is a context that ends d after begun does, and not before: the
// window of d that opens as begun ends, counted from the instant its cause
// names ([teardownAt]), never from when the callback arming it ran, which a
// loaded node runs late.
//
// MADE FROM BEGUN AND NOTHING ELSE. It carries begun's values and none of its
// ending, which would close the window the moment it opens; and none of a
// caller's context either: [New], which opens the settles' window, holds one
// that may end as the stop begins, and the window would end with it.
func windowFrom(begun context.Context, d time.Duration) context.Context {
	window, end := context.WithCancel(context.WithoutCancel(begun))
	context.AfterFunc(begun, func() {
		at, _ := teardownAt(begun)
		time.AfterFunc(time.Until(at.Add(d)), end)
	})
	return window
}

// waitSettles waits out every settle loop this node runs, for its teardown,
// which has begun: each makes its last attempt in the window the teardown
// opened and ends ([runSteers.settleRun]), so this returns within
// [steerSettleStopBudget] of the teardown's start. From here a take lost is
// said unsettled, and no loop starts — the store it would ask is closing.
// Nil-safe: an engine whose boot failed before it built its steers has no
// settle to wait for.
func (s *runSteers) waitSettles() {
	if s == nil {
		return
	}
	s.settleMu.Lock()
	s.settlesShut = true
	s.settleMu.Unlock()
	s.settles.Wait()
}

// carried is every note run's record holds read, opened, in the order read:
// what a segment resuming the turn carries ([runner.Resume.Steered]). A note
// that does not open is not carried: nobody can say it is a person's, and an
// item for one job is never carried, read or not ([sandbox.RunNote.For]).
//
// THE ORDER READ IS THE RECORD'S OWN ([sandbox.NoteRead.Order], then the order
// filed), never a read's time, which each node stamps from its own clock. THE
// WAITING NOTES GO BEFORE THE SORT, never into it: a comparison answering
// "equal" for any pair with a waiting note is no order. The package doc's
// "Every read is on the record, so the record is the carry" says why.
func (s *runSteers) carried(run sandbox.PendingRun) []steer.Read {
	if s == nil {
		return nil
	}
	notes := slices.DeleteFunc(slices.Clone(run.Steer.Notes), func(n sandbox.RunNote) bool {
		return n.Waiting() || n.For != ""
	})
	slices.SortStableFunc(notes, func(a, b sandbox.RunNote) int {
		return cmp.Or(cmp.Compare(a.Read.Order, b.Read.Order), cmp.Compare(a.Seq, b.Seq))
	})
	var out []steer.Read
	for _, n := range notes {
		note, ok := s.open(run.TurnID, n)
		if !ok {
			continue
		}
		out = append(out, steer.Read{Note: note, Reading: readingOf(*n.Read)})
	}
	return out
}

// servedNotes is every note on run's record its bridge served a harness
// ([sandbox.RunNote.Served]), by id, in the order filed — read since, or waiting
// still: what a segment resuming the turn tells a continued session as notes it
// may already show ([runner.Resume.Served]). In the clear, as a note's id is:
// a note whose seal does not open is never told, served or not.
//
// AND EVERY NOTE A HOOK'S SERVE TO IT WAS LOST TO A CANCEL, though the loss
// cleared its Served ([sandbox.HookServed.MayHold]): the harness gave the
// hook's call up, and its answer may have reached the session all the same.
// Told plainly only where every lost serve of it is proven absent from its
// session. An item for one job is none of them ([sandbox.RunNote.For]).
func servedNotes(run sandbox.PendingRun) []string {
	var out []string
	for _, n := range run.Steer.Notes {
		if n.For == "" && (n.Served != "" || slices.ContainsFunc(n.Serves, sandbox.HookServed.MayHold)) {
			out = append(out, n.ID)
		}
	}
	return out
}

// openedNote is a waiting note on a record, and the note it opens as; or, for
// a late-call item, which opens no seal, the call it hands its job; or, for a
// wake item, the wakes of it that open, oldest first.
type openedNote struct {
	held  sandbox.RunNote
	note  steer.Note
	call  *sandbox.LateCall
	wakes []steer.Wake
}

// waiting is the record's waiting notes that open, oldest first, that a reader
// inside job is handed ([forJob]): every reader names the job it reads inside,
// the run inbox its bound job and the review and a relaunch's brief none.
func (s *runSteers) waiting(turnID string, notes []sandbox.RunNote, job string) []openedNote {
	return forJob(s.waitingItems(turnID, notes), job)
}

// waitingItems is every waiting note and item on the record that opens, oldest
// first, whatever job an item is for: what a view of the record keeps
// ([noteWatch]), each reader of it taking what its own job is handed
// ([forJob]). ROUTED BY KIND BEFORE ANY SEAL IS OPENED: a late-call item, for
// one job and unsealed, is kept with its call, a wake item, for one job, with
// the wakes its own seals open as, and one claiming either any other way is
// said once and handed to nobody (package doc, "A late call is handed to the
// job working as it lands"; wakeitem.go).
func (s *runSteers) waitingItems(turnID string, notes []sandbox.RunNote) []openedNote {
	var out []openedNote
	for _, n := range notes {
		switch {
		case !n.Waiting():
		case n.LateCall != nil && n.Wake == nil && n.For != "" && len(n.Sealed) == 0:
			out = append(out, openedNote{held: n, call: n.LateCall})
		case n.Wake != nil && n.LateCall == nil && n.For != "" && len(n.Sealed) == 0:
			if wakes := s.openWakes(turnID, n); len(wakes) > 0 {
				out = append(out, openedNote{held: n, wakes: wakes})
			}
		case n.LateCall != nil:
			s.unverified(turnID, n.ID, "it claims a call of the run's log, which only the store files for one "+
				"job and never seals")
		case n.Wake != nil:
			s.unverified(turnID, n.ID, "it claims to hand a job somebody's message, which only the store files "+
				"for one job, under the message's own seal")
		default:
			if note, ok := s.open(turnID, n); ok {
				out = append(out, openedNote{held: n, note: note})
			}
		}
	}
	return out
}

// inAnswerOrder is candidates with a person's notes first, then wake items,
// then late-call items, each kind in the order it held: so an answer that fits
// only some never leaves a person's note waiting behind the store's items, nor
// a message somebody wrote behind the log's account of a call.
func inAnswerOrder(candidates []openedNote) []openedNote {
	out := slices.Clone(candidates)
	slices.SortStableFunc(out, func(a, b openedNote) int { return cmp.Compare(answerRank(a), answerRank(b)) })
	return out
}

// answerRank is where o's kind goes in one answer ([inAnswerOrder]).
func answerRank(o openedNote) int {
	switch {
	case o.call != nil:
		return 2
	case o.wakes != nil:
		return 1
	}
	return 0
}

// forJob is the notes of opened a reader inside job is handed
// ([sandbox.RunNote.IsFor]): every person's note, and an item for job alone. A
// reader inside no job names "" and is handed no item.
func forJob(opened []openedNote, job string) []openedNote {
	return slices.DeleteFunc(slices.Clone(opened), func(o openedNote) bool { return !o.held.IsFor(job) })
}

// runIdentity is the turn a record of a note names, the trace it is published
// under, and the conversation the turn works in, which a wake says whether it
// was written in ([prompts.WakeMessage]).
type runIdentity struct {
	turn         steer.Turn
	trace        events.TraceContext
	conversation string
}

func identityOfRef(ref sandbox.TurnRef) runIdentity {
	return runIdentity{
		turn: steer.Turn{Agent: ref.AgentID, AgentHandle: ref.AgentHandle, Role: ref.Role,
			TurnID: ref.TurnID, WorkKey: ref.WorkKey},
		trace:        events.TraceContext{TraceID: ref.TraceID, ParentSpanID: ref.SpanID},
		conversation: ref.ConversationKey,
	}
}

func identityOfRun(run sandbox.PendingRun) runIdentity {
	return runIdentity{
		turn: steer.Turn{Agent: run.AgentID, AgentHandle: run.AgentHandle, Role: run.Role,
			TurnID: run.TurnID, WorkKey: run.WorkKey},
		trace:        events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID},
		conversation: run.ConversationKey,
	}
}

// publish publishes what became of one note under the given identity, or a
// fresh one where id is nil.
func (s *runSteers) publish(ctx context.Context, ident runIdentity, note steer.Note,
	end steer.End, id uuid.UUID, at time.Time,
) error {
	if s.pub == nil {
		return nil
	}
	ev := events.New(steer.Record(ident.turn, note, end), ident.trace)
	if id != uuid.Nil {
		ev.ID = id
	}
	if !at.IsZero() {
		ev.Timestamp = at.UTC()
	}
	ev.Source = ident.turn.Role
	return s.pub.Publish(ctx, topics.Event(ev.Type), ev)
}

// ended records what became of every note on run's record that no step
// reported reading, as the record knows it ([sandbox.PendingRun.EndedNotes]):
// delivered (a started job's brief told it again), unconfirmed (handed to a
// harness), missed (a take read it and lost its answer) or expired (still
// waiting, or first read by a never-started job's brief). The step a run's
// ending takes before it deletes the record
// ([sandbox.CoordinatorOptions.SteersEnded]), under ids derived from the turn,
// the ending and the note, at the ending's own instant, so a repeat publishes
// the same records. THE ONE PUBLISHER of these for a note that reached a
// record, so none is reported twice. A note that does not open gets no record.
// And every wake on the record, as its ending knows it ([runSteers.endWakes]).
func (s *runSteers) ended(ctx context.Context, run sandbox.PendingRun) error {
	if run.Ending == nil {
		return nil
	}
	ident := identityOfRun(run)
	for _, n := range run.EndedNotes() {
		note, ok := s.open(run.TurnID, n.Note)
		if !ok {
			log.Warn("steer_note_end_unrecorded", "turn_id", run.TurnID, "note_id", n.Note.ID,
				"outcome", string(n.Outcome),
				"detail", "the turn ended with no step having reported this note read, and its seal does "+
					"not open here, so no record says what became of it")
			continue
		}
		id := uuid.NewSHA1(steerEndedNamespace, []byte(run.TurnID+"/"+run.Ending.ID+"/"+n.Note.ID))
		if err := s.publish(ctx, ident, note, noteEnd(run, n), id, run.Ending.At); err != nil {
			return fmt.Errorf("engine: recording what became of note %s to turn %s: %w", n.Note.ID,
				run.TurnID, err)
		}
	}
	return s.endWakes(ctx, run, wakeItems(run.Steer.Notes), run.Ending.At)
}

// noteEnd is what an ending reports of n, at the step its record names
// ([sandbox.EndedNote.Read]): the brief that told a delivered note again; the
// read a missed note's take recorded, kept beside a telling whose job never
// started; or the job an unconfirmed note was handed to, a step of the run's
// harness in its execute phase, at the iteration the record keeps for that job
// where it is the record's own and none where the record has moved on from it.
// A note a hook was served is reported at that hook's serve instead
// ([sandbox.EndedNote.Serve]): unconfirmed at the serve that was live before
// every read of it that handed nothing over, or at the last of its lost serves
// where its session may hold each (its harness gave the call up), and missed
// at the last of its serves, every one lost, one absent, each at the step the
// hook answered.
func noteEnd(run sandbox.PendingRun, n sandbox.EndedNote) steer.End {
	switch n.Outcome {
	case types.SteerDelivered:
		return steer.Retold(readingOf(*n.Read))
	case types.SteerMissed:
		if n.Serve != nil {
			return steer.Missed(servedAt(*n.Serve))
		}
		return steer.Missed(readingOf(*n.Read))
	case types.SteerUnconfirmed:
		if n.Serve != nil {
			return steer.Unconfirmed(servedAt(*n.Serve))
		}
		at := steer.Reading{Phase: types.PhaseExecute, Round: 1, Boundary: types.SteerBoundaryStep,
			Launch: n.Note.Served}
		if n.Note.Served == run.LaunchID {
			at.Iteration = run.Launch.Iteration
		}
		return steer.Unconfirmed(at)
	default:
		return steer.Expired()
	}
}

// servedAt is the step a hook's serve handed a note over at, as the agent layer
// states one: a step of the run's harness in its execute phase, at its one
// round, the boundary and iteration the hook answered, of the serve's job.
func servedAt(s sandbox.HookServed) steer.Reading {
	return steer.Reading{Phase: types.PhaseExecute, Iteration: s.Iteration, Round: 1,
		Boundary: types.SteerBoundary(s.Boundary), Launch: s.Launch, At: s.At}
}

// --- a resumed segment's notes: the run's record ---------------------------

// rowSource is the turn's notes as a segment that resumes it reads them, off
// the run's record under the segment's claim ([runner.SteerSource]).
//
// # A take whose answer was lost is the segment's own to settle
//
// A take that answered an error may have landed. If its settle does not answer
// on the take's own path either, the take is LOST and kept here by name, settled
// again at each later take and at a relaunch's brief, and handed over by the
// first the store answers. Meanwhile it stands for a note waiting
// ([rowSource.Waiting]) for [steerSettlePatience]. One still lost when the
// segment ends is marked unanswered ([rowSource.Close],
// [runSteers.settleUnanswered]).
type rowSource struct {
	steers *runSteers
	run    sandbox.PendingRun
	fence  sandbox.Fence
	watch  *noteWatch

	// mu guards lost, lostAt, briefed and orders.
	mu sync.Mutex
	// lost is every take of this segment whose call and settle both answered
	// an error and whose settle has not answered since, by name, oldest first,
	// and lostAt when the first of them was lost.
	lost   []string
	lostAt time.Time
	// briefed is the take each note a relaunch's brief was handed from a
	// settle came from, by note id: a launch that fails before its write hands
	// the brief back, and its takes with it ([rowSource.Return]), and one that
	// writes takes each take's read over ([rowSource.lostTakes]).
	briefed map[string]string
	// orders is the place among the record's reads ([sandbox.NoteRead.Order])
	// of every note this segment's takes recorded and handed over — its
	// rounds', and a lost take's a settle handed a round or a brief — by note
	// id: what a relaunch's brief tells the turn's read notes in the order of
	// ([steer.Briefing.Orders]), its runner holding them unnumbered.
	orders map[string]int
}

// source is the resumed segment's notes for run, which it claimed under fence:
// its takes pinned to the claimed job, the resumed status and that lease. Close
// it when the segment ends.
func (s *runSteers) source(ctx context.Context, run sandbox.PendingRun, fence sandbox.Fence) *rowSource {
	if s == nil {
		return nil
	}
	return &rowSource{steers: s, run: run, fence: fence, watch: s.watch(ctx, run.TurnID)}
}

// claimedNotes is run with the notes its record holds now, where the record
// still holds the claim run was read under: the same job and the same lease's
// epoch. A collection's read settles its job's hooks' serves after the claim
// read the row ([sandbox.CoordinatorOptions.SteersConfirmed]), so the segment
// carries a note the job's session showed it took as read there. The whole of
// the record's Steer, never its Notes alone, so whatever it keeps beside them
// comes too. An error is the resume's retry.
func (s *runSteers) claimedNotes(ctx context.Context, run sandbox.PendingRun) (sandbox.PendingRun, error) {
	if s == nil {
		return run, nil
	}
	row, found, err := s.store.Get(ctx, run.TurnID)
	if err != nil {
		return run, fmt.Errorf("engine: reading run %s's notes again before its segment resumes: %w",
			run.TurnID, err)
	}
	if found && row.LaunchID == run.LaunchID && row.Owner == run.Owner && row.OwnerEpoch == run.OwnerEpoch {
		run.Steer = row.Steer
	}
	return run, nil
}

// live reports whether the record still holds this segment's claim: the job it
// claimed, resumed, its ending not decided.
func (r *rowSource) live(status, launch string, ending bool) bool {
	return status == sandbox.StatusResumed && launch == r.run.LaunchID && !ending
}

// Take takes every waiting note that opens, read where at says — after the
// notes the segment's lost takes recorded, where their settle answers now
// ([rowSource.settleLost]), which were read before them.
func (r *rowSource) Take(ctx context.Context, at steer.Reading) []steer.Note {
	if r == nil {
		return nil
	}
	settled := r.opened(r.settleLost(ctx))
	run, found, err := r.steers.store.Get(ctx, r.run.TurnID)
	if err != nil {
		log.WarnContext(ctx, "steer_notes_unread", "turn_id", r.run.TurnID, "error", err.Error(),
			"detail", "the record could not be read; its notes wait for the next step")
		return notesOf(settled)
	}
	if !found || !r.live(run.Status, run.LaunchID, run.Ending != nil) {
		return notesOf(settled)
	}
	// INSIDE NO JOB: the review reads a person's notes, and no job's item.
	candidates := r.steers.waiting(run.TurnID, run.Steer.Notes, "")
	return append(notesOf(settled), r.take(ctx, candidates, run.Steer.Notes, readOf(at))...)
}

// take records candidates read, and answers the ones the write took, each said
// sent before where record places it before a read its reader holds
// ([placedBefore]).
//
// A TAKE THAT ANSWERED AN ERROR IS SETTLED HERE, its round still waiting on it:
// its write may have landed with its answer lost, and what it recorded is read
// by the record's word, so no later take hands it over — the settle answers it,
// and it is handed over now. One whose settle the store does not answer either
// is LOST, and settled again by the segment's next take or brief
// ([rowSource.settleLost]).
func (r *rowSource) take(ctx context.Context, candidates []openedNote, record []sandbox.RunNote,
	read sandbox.NoteRead) []steer.Note {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.held.ID)
	}
	read.Take = takeName()
	taken, err := r.steers.store.TakeNotes(ctx, r.run.TurnID, sandbox.NoteTake{
		IDs: ids, Launch: r.run.LaunchID, Statuses: []string{sandbox.StatusResumed},
		Fence: r.fence, Read: read,
	})
	if err != nil {
		settled, settleErr := r.steers.store.SettleTake(ctx, r.run.TurnID, sandbox.NoteSettle{Take: read.Take})
		switch {
		case settleErr != nil:
			log.WarnContext(ctx, "steer_notes_untaken", "turn_id", r.run.TurnID, "error", err.Error(),
				"settle_error", settleErr.Error(),
				"detail", "the notes wait for the next step, or, where the store recorded this take, are "+
					"handed over by the segment's next round or brief that the store answers, and marked "+
					"unanswered if none does before the segment ends")
			r.lose(read.Take)
			return nil
		case len(settled) == 0:
			log.WarnContext(ctx, "steer_notes_untaken", "turn_id", r.run.TurnID, "error", err.Error(),
				"detail", "the notes wait for the next step")
			return nil
		}
		// RECORDED, ITS ANSWER LOST: handed over now.
		taken = settled
	}
	r.numbered(taken)
	notes := notesTaken(candidates, taken)
	for k := range notes {
		notes[k].SentBefore = placedBefore(candidates[slices.IndexFunc(candidates, func(c openedNote) bool {
			return c.note.ID == notes[k].ID
		})].held, record)
	}
	return notes
}

// placedBefore reports whether n, a person's note a hook's serve placed among
// the turn's reads ([sandbox.RunNote.Place]), sits before a read of a person's
// note on record, the record the take chose on: a note filed later that the
// reader's conversation already holds, so n is told as sent before it
// ([steer.Note.SentBefore]) rather than as the later word.
func placedBefore(n sandbox.RunNote, record []sandbox.RunNote) bool {
	if n.For != "" || n.Place == 0 {
		return false
	}
	return slices.ContainsFunc(record, func(m sandbox.RunNote) bool {
		return m.For == "" && m.ID != n.ID && m.Seq > n.Seq && m.Read != nil && !m.Read.Unanswered &&
			m.Read.Order > n.Place
	})
}

// numbered keeps the place among the record's reads of every note of recorded,
// which this segment hands over ([rowSource.orders]).
func (r *rowSource) numbered(recorded []sandbox.RunNote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range recorded {
		if n.Read == nil {
			continue
		}
		if r.orders == nil {
			r.orders = map[string]int{}
		}
		r.orders[n.ID] = n.Read.Order
	}
}

// lose keeps take as one of the segment's lost takes.
func (r *rowSource) lose(take string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.lost, take) {
		return
	}
	if len(r.lost) == 0 {
		r.lostAt = r.steers.now()
	}
	r.lost = append(r.lost, take)
}

// settledNote is a note a lost take recorded, as its settle answered it.
type settledNote struct {
	take string
	held sandbox.RunNote
}

// settleLost settles the segment's lost takes on its own path, in answer mode,
// and answers the notes they recorded, in the order the takes were lost — for
// the caller to hand over now. A settle the store answers ends its take: what
// it recorded is answered, and where it recorded nothing its notes wait again
// for the take the caller makes next. One the store does not answer stays lost.
func (r *rowSource) settleLost(ctx context.Context) []settledNote {
	r.mu.Lock()
	lost := slices.Clone(r.lost)
	r.mu.Unlock()
	var out []settledNote
	var answered []string
	for _, take := range lost {
		recorded, err := r.steers.store.SettleTake(ctx, r.run.TurnID, sandbox.NoteSettle{Take: take})
		if err != nil {
			continue
		}
		answered = append(answered, take)
		r.numbered(recorded)
		for _, n := range recorded {
			out = append(out, settledNote{take: take, held: n})
		}
	}
	r.mu.Lock()
	r.lost = slices.DeleteFunc(r.lost, func(t string) bool { return slices.Contains(answered, t) })
	r.mu.Unlock()
	return out
}

// openedSettled is a settled note, and the note it opens as.
type openedSettled struct {
	take string
	note steer.Note
}

// opened is the settled notes that open, in their order. One that does not is
// never handed over: nobody can say it is a person's.
func (r *rowSource) opened(settled []settledNote) []openedSettled {
	out := make([]openedSettled, 0, len(settled))
	for _, s := range settled {
		if note, ok := r.steers.open(r.run.TurnID, s.held); ok {
			out = append(out, openedSettled{take: s.take, note: note})
		}
	}
	return out
}

// notesOf is the notes of opened settled notes.
func notesOf(settled []openedSettled) []steer.Note {
	out := make([]steer.Note, 0, len(settled))
	for _, s := range settled {
		out = append(out, s.note)
	}
	return out
}

// notesTaken is the notes of candidates a take recorded, in the take's order.
func notesTaken(candidates []openedNote, taken []sandbox.RunNote) []steer.Note {
	out := make([]steer.Note, 0, len(taken))
	for _, t := range taken {
		for _, c := range candidates {
			if c.held.ID == t.ID {
				out = append(out, c.note)
				break
			}
		}
	}
	return out
}

// Brief is what a relaunch's brief takes of the turn's notes
// ([steer.Briefing]): the notes the segment's lost takes recorded, where their
// settle answers now ([rowSource.settleLost]), which the launch's write records
// read in their take's place ([rowSource.lostTakes]); then the waiting notes
// that open, a peek the launch's write records read; and every note the record
// marks unanswered, as the record holds the marks now. With them goes the
// place among the record's reads of every note the segment's takes handed over
// ([rowSource.orders]) and of every peeked note a hook's serve placed
// ([sandbox.RunNote.Place]), so the brief tells the notes in the record's
// order. The package doc's "Every read is on the record, so the record is the
// carry" says why.
func (r *rowSource) Brief(ctx context.Context) steer.Briefing {
	if r == nil {
		return steer.Briefing{}
	}
	settled := r.opened(r.settleLost(ctx))
	r.mu.Lock()
	for _, s := range settled {
		if r.briefed == nil {
			r.briefed = map[string]string{}
		}
		r.briefed[s.note.ID] = s.take
	}
	r.mu.Unlock()
	notes := notesOf(settled)
	run, found, err := r.steers.store.Get(ctx, r.run.TurnID)
	if err != nil {
		log.WarnContext(ctx, "steer_notes_unread", "turn_id", r.run.TurnID, "error", err.Error(),
			"detail", "the record could not be read; the launch's brief carries no new note, and "+
				"its notes wait for the run's own steps")
		return steer.Briefing{Notes: notes, Orders: r.ordersNow()}
	}
	if !found || run.Ending != nil {
		return steer.Briefing{Notes: notes, Orders: r.ordersNow()}
	}
	// INSIDE NO JOB: the brief is the next job's, and an item on the record is
	// the job before it's, which the launch's write removes. Peeked here, one
	// would be written back as a person's note the brief read.
	orders := r.ordersNow()
	for _, o := range r.steers.waiting(run.TurnID, run.Steer.Notes, "") {
		notes = append(notes, o.note)
		if o.held.Place > 0 {
			if orders == nil {
				orders = map[string]int{}
			}
			orders[o.held.ID] = o.held.Place
		}
	}
	return steer.Briefing{Notes: notes, Unanswered: unansweredNotes(run), Orders: orders}
}

// ordersNow is the places among the record's reads of the notes this segment's
// takes handed over, a lost take's this brief carries among them
// ([rowSource.orders]): a copy, the brief's to keep.
func (r *rowSource) ordersNow() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.orders)
}

// unansweredNotes is every note on run's record whose read the record marks
// unanswered ([sandbox.NoteRead.Unanswered]), by id: a person's, since a brief
// tells no job's item ([sandbox.RunNote.For]).
func unansweredNotes(run sandbox.PendingRun) []string {
	var out []string
	for _, n := range run.Steer.Notes {
		if n.For == "" && n.Read != nil && n.Read.Unanswered {
			out = append(out, n.ID)
		}
	}
	return out
}

// lostTakes is the take each of notes was handed to a relaunch's brief from,
// by note id, where a lost take's settle handed it ([rowSource.Brief]) — for
// the launch's write, which records such a note read by the job it opens in
// that take's place ([sandbox.RunNote.LostTake]): the take's reader never had
// the note, and the brief is the first to tell it. Nil for a nil source, whose
// brief took none so.
func (r *rowSource) lostTakes(notes []steer.Note) map[string]string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out map[string]string
	for _, n := range notes {
		if take, ok := r.briefed[n.ID]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[n.ID] = take
		}
	}
	return out
}

// Return takes back the notes a relaunch's brief took where the launch failed
// before it wrote the record ([runner.SteerSource.Return]). A waiting note it
// peeked needs nothing: a peek takes nothing, and the note still waits on the
// record for whatever reads the turn next, or for its ending. A note a lost
// take's settle handed the brief is read already, so its take is lost again,
// for the segment's next round or brief to hand over. A launch that failed
// after its write is never handed back: the record holds what its brief read,
// and the ending that deletes the record reports them ([runSteers.ended]) —
// which is also why nothing here may publish: a note this source peeked can be
// reported expired by another node's ending while the launch is still failing.
func (r *rowSource) Return(_ context.Context, notes []steer.Note) {
	if r == nil {
		return
	}
	var takes []string
	r.mu.Lock()
	for _, n := range notes {
		if take, ok := r.briefed[n.ID]; ok {
			delete(r.briefed, n.ID)
			takes = append(takes, take)
		}
	}
	r.mu.Unlock()
	for _, take := range takes {
		r.lose(take)
	}
}

// Waiting reports a waiting note that opens, from the segment's view of the
// record — never a store read: the tool loop asks it under its dispatcher's
// lock — or a lost take the store may yet settle: one lost less than
// [steerSettlePatience] ago, whose notes the next round's take hands over once
// the store answers.
func (r *rowSource) Waiting() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	pending := len(r.lost) > 0 && r.steers.now().Sub(r.lostAt) < steerSettlePatience
	r.mu.Unlock()
	if pending {
		return true
	}
	// A PERSON'S NOTE ALONE: the review takes no job's item, and a round
	// ended for one would open the next with nothing.
	view, opened, _ := r.watch.snapshot()
	return view.Found && r.live(view.Status, view.LaunchID, view.EndingDecided) && len(forJob(opened, "")) > 0
}

// Close stops the segment's view of the record, and marks every take still
// lost unanswered where it recorded anything: nothing of the segment reads it
// any more. Safe twice: the takes are marked or handed off once.
//
// MARKED IN THE SEGMENT'S PATH FIRST, one attempt ([runSteers.settleOnce]):
// a segment that ends the turn is followed at once by the run's ending, which
// reports what no step reported reading ([runSteers.ended]), and an unmarked
// note a lost take recorded reads as a step's report. Handed off the path
// ([runSteers.settleUnanswered]), the mark could land after that read or find
// the record deleted, and no node would report the note. The first take the
// store does not answer is handed off with every take after it, not each asked
// in the path. Under ctx stripped of its ending, as a teardown does.
func (r *rowSource) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.watch.stop()
	r.mu.Lock()
	lost := r.lost
	r.lost, r.briefed = nil, nil
	r.mu.Unlock()
	for k, take := range lost {
		marked, err := r.steers.settleOnce(context.WithoutCancel(ctx), r.run.TurnID,
			sandbox.NoteSettle{Take: take, Unanswered: true})
		if err != nil {
			for _, left := range lost[k:] {
				r.steers.settleUnanswered(ctx, r.run.TurnID, left)
			}
			return
		}
		sayUnanswered(ctx, r.run.TurnID, marked)
	}
}

// --- a run's own harness's notes: the run inbox --------------------------------

// runInbox is an executor's notes as its bridge session serves them to its harness
// between steps ([mcpbridge.SteerInbox]): built for the run's job from its
// record alone, so the node that takes a run over builds the same one.
type runInbox struct {
	steers *runSteers
	ident  runIdentity
	round  int
	// harness is the run's harness, whose delivery the inbox answers in.
	harness string

	mu     sync.Mutex
	launch string
	watch  *noteWatch
	closed bool
	pulled bool

	// boundary is the fence token the job's session's tool answers carry, an
	// item neutralised under it ([lateItemMessage]); set by [runInbox.Bind].
	boundary provenance.Boundary

	// hooks is what the inbox knows of its hooks' serves that the record
	// cannot say yet ([hookMemory]).
	hooks hookMemory
}

var _ mcpbridge.SteerInbox = (*runInbox)(nil)

// steerLossesKept is how many of its hooks' losses a run inbox remembers
// ([hookMemory]): each set its newest THIRTY-TWO. A loss is settled on the
// record within a round of the run's settle loop (eight seconds at most once
// its store answers), and a job's hooks answer one take a boundary, so a run
// of losses a store answering at all leaves unsettled is far shorter; and a
// row holds at most sixty-four serves to its job (twenty notes and items three
// times each, four wake items once), of which a box that POSTs in a loop can
// only ever name these.
const steerLossesKept = 32

// hookMemory is what a run inbox knows of its hooks' serves that the record
// cannot say yet: the calls whose take runs now, the calls it ran a take for,
// the calls it heard cancelled, its takes answered nothing whose serve may
// have landed (each of which its next serve settles in its own write,
// [sandbox.HookServe]), and the notes it said at their bound. Every method
// holds the lock for its whole body, so no caller splits a check from what it
// does: a cancel either finds its call running, and the take sees it, or finds
// it gone, and asks the store itself.
type hookMemory struct {
	mu                        sync.Mutex
	running, taken, cancelled []sandbox.NoteCall
	unanswered                []string
	bounded                   []string
}

// begin registers call's take as running and taken, false where the call was
// cancelled before it arrived: the hook gave it up, and is answered nothing.
func (m *hookMemory) begin(call sandbox.NoteCall) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.ContainsFunc(m.cancelled, call.Is) {
		return false
	}
	m.running = append(m.running, call)
	if !slices.ContainsFunc(m.taken, call.Is) {
		m.taken = kept(append(m.taken, call))
	}
	return true
}

// end ends call's take (nil: a take of no registered call) and reports whether
// its answer is lost: where lost says so, or call was heard cancelled while
// it ran. A lost take keeps take as answered nothing, the newest
// [steerLossesKept] kept.
func (m *hookMemory) end(call *sandbox.NoteCall, take string, lost bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if call != nil {
		if k := slices.IndexFunc(m.running, call.Is); k >= 0 {
			m.running = slices.Delete(m.running, k, k+1)
		}
		lost = lost || slices.ContainsFunc(m.cancelled, call.Is)
	}
	if lost && take != "" {
		m.unanswered = kept(append(m.unanswered, take))
	}
	return lost
}

// cancel keeps call as cancelled and reports whether the store must be asked
// to lose its serve: not where the call was heard before (its first hearing
// asked, or left it to the take below, so a box POSTing one cancel again asks
// nothing more), not where its take still runs (that take settles itself),
// not on an inbox bound to no job (its first take reads the ring), and not
// where the inbox took calls and this is none of them (a model's call given
// up, or another session's, which no serve of this job names).
func (m *hookMemory) cancel(call sandbox.NoteCall, bound bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.ContainsFunc(m.cancelled, call.Is) {
		return false
	}
	m.cancelled = kept(append(m.cancelled, call))
	switch {
	case !bound, slices.ContainsFunc(m.running, call.Is):
		return false
	case len(m.taken) > 0 && !slices.ContainsFunc(m.taken, call.Is):
		return false
	}
	return true
}

// losses is a copy of the calls heard cancelled and the takes answered
// nothing, for a serve's write to settle.
func (m *hookMemory) losses() ([]sandbox.NoteCall, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.cancelled), slices.Clone(m.unanswered)
}

// atBound reports whether note's being at its bound is news to the inbox,
// remembering it: said once a note.
func (m *hookMemory) atBound(note string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.Contains(m.bounded, note) {
		return false
	}
	m.bounded = kept(append(m.bounded, note))
	return true
}

// kept is the newest [steerLossesKept] of list.
func kept[T any](list []T) []T {
	if over := len(list) - steerLossesKept; over > 0 {
		return slices.Clone(list[over:])
	}
	return list
}

// inbox is the inbox of a run identified by ident, on harness, for the turn
// iteration round; it serves nothing until it is bound to the run's job.
//
// ONE INBOX WHATEVER THE RUN'S HARNESS: a harness answered by hooks takes notes
// through [runInbox.Take] and one whose plugin polls through [runInbox.Wait]
// and [runInbox.Read], each reaching only the half its own harness calls; the
// answer's shape and an admitted note's id are the harness's own
// ([executor.HookAnswer], [executor.InboxID]).
func (s *runSteers) inbox(ident runIdentity, round int, harness string) *runInbox {
	if s == nil {
		return nil
	}
	return &runInbox{steers: s, ident: ident, round: round, harness: harness}
}

// Bind starts serving the inbox for the job launch, whose session fences its
// tool answers under boundary: its view of the run's record follows the record
// from here, under ctx stripped of its cancellation — the inbox lives as long
// as the job's session, which outlives the turn that launched it, and the view
// is stopped by [runInbox.Close] alone. Bound before the bridge files the
// session's opening ([Engine.openBridge]), so no opening serves an inbox that
// serves no job yet.
func (i *runInbox) Bind(ctx context.Context, launch string, boundary provenance.Boundary) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.launch != "" {
		return
	}
	i.launch, i.boundary = launch, boundary
	i.watch = i.steers.watch(context.WithoutCancel(ctx), i.ident.turn.TurnID)
}

// message is o as the inbox hands it over, one message an item: a person's
// note in their words, a late call in the engine's ([lateItemMessage]), and a
// wake item's messages each in their writer's fence ([wakeItemMessage]).
func (i *runInbox) message(ctx context.Context, o openedNote) string {
	switch {
	case o.call != nil:
		return lateItemMessage(ctx, *o.call, i.fence())
	case o.wakes != nil:
		return wakeItemMessage(o.wakes, i.fence(), i.ident.conversation)
	}
	return prompts.SteerMessage(o.note.By, o.note.Text)
}

// fence is the token the job's session fences its tools' answers under, which
// the inbox's items are drawn under ([runInbox.Bind]).
func (i *runInbox) fence() provenance.Boundary {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.boundary
}

// bound is the job the inbox serves and its view, or empty where it serves none.
func (i *runInbox) bound() (string, *noteWatch) {
	if i == nil {
		return "", nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return "", nil
	}
	return i.launch, i.watch
}

// liveFor reports whether a view of the record is one the job's harness takes notes
// from: the job's own, working, its ending not decided.
func liveFor(view sandbox.NotesView, launch string) bool {
	return view.Found && !view.EndingDecided && view.LaunchID == launch &&
		slices.Contains(steerTakeStatuses, view.Status)
}

// Take answers one hook of the run's main agent — see [mcpbridge.SteerInbox]:
// it SERVES the job the waiting notes it may still be handed, in a write
// before its answer ([sandbox.PendingStore.ServeHook]), and answers exactly
// what that write served, under the serve's take. The note is read only once
// the job's session shows the hook handed it over ([runSteers.confirmed]), so
// nothing is published here.
//
// AFTER A BATCH, OFF THE VIEW: a step with nothing waiting is answered from the
// inbox's view of the record, with no store read — the harness waits on the answer
// before its next request, at every batch. AT THE FINISH, OFF THE STORE: it is
// the run's last boundary, and a note filed a moment before must not be missed
// for a view one write behind.
func (i *runInbox) Take(ctx context.Context, event string, call *sandbox.NoteCall) string {
	launch, watch := i.bound()
	if launch == "" {
		return ""
	}
	if call != nil && !i.hooks.begin(*call) {
		return ""
	}
	i.markPulled(ctx, launch)
	boundary, notes, candidates := i.candidates(ctx, event, launch, watch)
	cancelled, unanswered := i.hooks.losses()
	candidates = i.servable(ctx, launch, candidates, cancelled, unanswered)
	if len(candidates) == 0 {
		i.hooks.end(call, "", false)
		return ""
	}
	candidates = inAnswerOrder(candidates)
	messages := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.call != nil || c.wakes != nil {
			messages = append(messages, i.message(ctx, c))
			continue
		}
		messages = append(messages, hookMessage(c, notes, launch, unanswered, candidates))
	}
	fit := executor.HookNotesThatFit(i.harness, messages)
	candidates, messages = candidates[:fit], messages[:fit]
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.held.ID)
	}
	take := takeName()
	cancelLost := servesOfCalls(notes, launch, cancelled, "")
	served, err := i.steers.store.ServeHook(ctx, i.ident.turn.TurnID, sandbox.HookServe{
		IDs: ids, Launch: launch, Statuses: steerTakeStatuses,
		Serve:     sandbox.HookServed{Take: take, Boundary: string(boundary), Iteration: i.round, Call: call},
		Cancelled: cancelled, Unanswered: unanswered, Wakes: wakesHanded(candidates),
	})
	if err != nil {
		i.hooks.end(call, take, true)
		log.WarnContext(ctx, "steer_serve_failed", "turn_id", i.ident.turn.TurnID,
			"error", err.Error(), "detail", "the run's harness is handed nothing at this step: the notes "+
				"wait for its next one, served again there, or for its review")
		i.answeredNothing(ctx, launch, take)
		return ""
	}
	sayServesLost(ctx, i.ident.turn.TurnID, launch, cancelLost)
	// A HOOK GIVEN UP WHILE ITS TAKE RAN takes nothing from it: its call
	// heard cancelled, or its request cancelled, by the SDK's canceller or the
	// harness aborting it. The take's own budget ends it DeadlineExceeded,
	// which is no cancel: the hook still waits for its answer.
	if i.hooks.end(call, take, errors.Is(ctx.Err(), context.Canceled)) {
		if len(served) > 0 {
			i.answeredNothing(ctx, launch, take)
		}
		return ""
	}
	hook := make([]executor.HookNote, 0, len(served))
	token := i.fence()
	for k, c := range candidates {
		if slices.ContainsFunc(served, func(n sandbox.RunNote) bool { return n.ID == c.held.ID }) {
			hook = append(hook, executor.HookNote{By: c.note.By, Message: messages[k], Call: c.call != nil,
				Writers: wakeWriters(c.wakes, token)})
		}
	}
	if len(hook) == 0 {
		return ""
	}
	return executor.HookAnswer(i.harness, event, take, hook)
}

// candidates is the boundary a hook at event answers, the record it chose on
// (the notes of the view after a batch, of the store at the finish) and the
// waiting notes and items on it a reader inside launch is handed, oldest
// first: ONE PLACE a take's candidates are chosen, per event. No late call at
// the finish, which waits for the next launch's continuation (package doc, "A
// late call is handed to the job working as it lands").
func (i *runInbox) candidates(ctx context.Context, event, launch string, watch *noteWatch) (
	types.SteerBoundary, []sandbox.RunNote, []openedNote) {
	if event != mcpbridge.SteerEventFinish {
		view, opened, _ := watch.snapshot()
		if !liveFor(view, launch) {
			return types.SteerBoundaryToolBatch, nil, nil
		}
		return types.SteerBoundaryToolBatch, view.Steer.Notes, forJob(opened, launch)
	}
	run, found, err := i.steers.store.Get(ctx, i.ident.turn.TurnID)
	if err != nil {
		log.WarnContext(ctx, "steer_take_unread", "turn_id", i.ident.turn.TurnID,
			"error", err.Error(), "detail", "the run finishes without its waiting notes, "+
				"which its review reads")
		return types.SteerBoundaryFinish, nil, nil
	}
	if !found || run.Ending != nil || run.LaunchID != launch || !slices.Contains(steerTakeStatuses, run.Status) {
		return types.SteerBoundaryFinish, nil, nil
	}
	return types.SteerBoundaryFinish, run.Steer.Notes, slices.DeleteFunc(
		i.steers.waiting(run.TurnID, run.Steer.Notes, launch), func(o openedNote) bool { return o.call != nil })
}

// servable is the candidates a hook of launch may still be served: none
// whose live serve to launch its harness may hold (one whose call is named in
// cancelled, or whose take is named in unanswered, is settled by the serve's
// own write), and none at its bound of serves to launch
// ([sandbox.RunNote.MaxServes]), said once a note (steer_serve_bound): a
// person's note waits for the review or the next launch's brief, a late call's
// item for the next launch, which removes it and whose continuation tells the
// call, and a wake item's deliveries run as turns of their own.
func (i *runInbox) servable(ctx context.Context, launch string, candidates []openedNote,
	cancelled []sandbox.NoteCall, unanswered []string) []openedNote {
	return slices.DeleteFunc(slices.Clone(candidates), func(c openedNote) bool {
		if live, ok := c.held.LiveServe(); ok && live.Launch == launch && !slices.Contains(unanswered, live.Take) &&
			(live.Call == nil || !slices.ContainsFunc(cancelled, live.Call.Is)) {
			return true
		}
		serves := 0
		for _, h := range c.held.Serves {
			if h.Launch == launch && !slices.Contains(unanswered, h.Take) {
				serves++
			}
		}
		if serves < c.held.MaxServes() {
			return false
		}
		if i.hooks.atBound(c.held.ID) {
			detail := "the note was served to the run's job as often as it may be, and never shown taken: " +
				"it waits for the run's review or the next launch's brief"
			switch {
			case c.held.LateCall != nil:
				detail = "the late call's item was served to the run's job as often as it may be, and never " +
					"shown taken: the next launch removes it, and its continuation tells the call"
			case c.held.IsWake():
				detail = "the wake item was served to the run's job once, as a wake is, and never shown " +
					"taken: its deliveries run as turns of their own once the run is over"
			}
			log.WarnContext(ctx, "steer_serve_bound", "turn_id", i.ident.turn.TurnID, "note_id", c.held.ID,
				"serves", serves, "detail", detail)
		}
		return true
	})
}

// hookMessage is a person's note c as a hook's answer hands it to job's model:
// ONE RENDERING a note, said as sent before ([prompts.SteerMessageSentBefore])
// where a person's note filed after it was already served to job or read by
// it on notes, the record the take chose on, since the harness would otherwise
// take it as the later word. A later note among this answer's candidates
// reaches the harness after c anyway, in this answer or a later one, so it
// cues nothing.
func hookMessage(c openedNote, notes []sandbox.RunNote, job string, unanswered []string,
	candidates []openedNote) string {
	if c.held.For == "" && slices.ContainsFunc(notes, func(n sandbox.RunNote) bool {
		return n.For == "" && n.Seq > c.held.Seq && handedTo(n, job, unanswered) &&
			!slices.ContainsFunc(candidates, func(o openedNote) bool { return o.held.ID == n.ID })
	}) {
		return prompts.SteerMessageSentBefore(c.note.By, c.note.Text)
	}
	return prompts.SteerMessage(c.note.By, c.note.Text)
}

// handedTo reports whether n was read by job, or served to its hook by a serve
// its session may hold: none whose take the inbox answered nothing
// (unanswered, [hookMemory]).
func handedTo(n sandbox.RunNote, job string, unanswered []string) bool {
	if n.Read != nil {
		return n.Read.Launch == job
	}
	return slices.ContainsFunc(n.Serves, func(h sandbox.HookServed) bool {
		return h.Launch == job && h.MayHold() && !slices.Contains(unanswered, h.Take)
	})
}

// answeredNothing hands the run's settle loop take, whose hook launch's inbox
// answered nothing and kept so ([hookMemory.end]), for a job with no next
// take to remove its serve in its own write ([runSteers.settleHook]): the note
// waits again, and its write, were it still in flight, can no longer land.
func (i *runInbox) answeredNothing(ctx context.Context, launch, take string) {
	i.steers.settleHook(ctx, i.ident.turn.TurnID, sandbox.HookSettle{Launch: launch, Unanswered: []string{take}})
}

// Cancelled hears the box's harness give a hook's call up — see
// [mcpbridge.SteerInbox]: kept in the inbox's ring for its next take, and,
// where no take of it runs now and the inbox may have served it, handed the
// run's settle loop for the job the inbox was bound to, closed or not, since
// the job is a fact and the write acts on its serves alone. Never blocks.
func (i *runInbox) Cancelled(ctx context.Context, call sandbox.NoteCall) {
	if i == nil {
		return
	}
	i.mu.Lock()
	launch := i.launch
	i.mu.Unlock()
	if i.hooks.cancel(call, launch != "") {
		i.steers.settleHook(ctx, i.ident.turn.TurnID, sandbox.HookSettle{Launch: launch,
			Cancelled: []sandbox.NoteCall{call}})
	}
}

// delivered publishes the notes a read recorded as delivered there, off the
// path that read them, under ids derived from the turn and each note, under
// ctx stripped of its cancellation: the read is on the record, and its record
// is owed regardless. A PERSON'S NOTES ALONE: an item for one job
// ([sandbox.RunNote.For]) is the store's to say. The one publish every reader
// that reports a read shares: OpenCode's confirmation and a collection's read
// of its job's session ([runSteers.confirmed]), so a note both reported is one
// record.
func (s *runSteers) delivered(ctx context.Context, ident runIdentity, taken []sandbox.RunNote,
	notes []steer.Note) {
	if len(notes) == 0 {
		return
	}
	base := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(base, steerPublishBudget)
		defer cancel()
		for k, note := range notes {
			if taken[k].For != "" {
				continue
			}
			read := readingOf(*taken[k].Read)
			id := uuid.NewSHA1(steerDeliveredNamespace,
				[]byte(ident.turn.TurnID+"/"+note.ID+"/delivered"))
			if err := s.publish(ctx, ident, note, steer.Delivered(read), id, read.At); err != nil {
				log.ErrorContext(ctx, "steer_delivery_unrecorded", "turn_id", ident.turn.TurnID,
					"note_id", note.ID, "boundary", string(read.Boundary), "error", err.Error(),
					"detail", "the run read the note, and no record says so")
			}
		}
	}()
}

// confirmed publishes as delivered every note of notes that opens: the notes a
// hook's serve to run's job handed over that the job's session showed it took
// ([sandbox.CoordinatorOptions.SteersConfirmed]), each at its read, through the
// one publish ([runSteers.delivered]); and claims every wake such a serve
// handed over ([runSteers.joined]). Made on every read of the job's box, so a
// read made again publishes the same records.
func (s *runSteers) confirmed(ctx context.Context, run sandbox.PendingRun, notes []sandbox.RunNote) {
	if s == nil {
		return
	}
	var taken []sandbox.RunNote
	var opened []steer.Note
	var wakes []openedNote
	for _, n := range notes {
		switch {
		case n.Read == nil:
		case n.IsWake():
			if read := s.openWakes(run.TurnID, n); len(read) > 0 {
				wakes = append(wakes, openedNote{held: n, wakes: read})
			}
		case n.For != "":
		default:
			if note, ok := s.open(run.TurnID, n); ok {
				taken = append(taken, n)
				opened = append(opened, note)
			}
		}
	}
	s.delivered(ctx, identityOfRun(run), taken, opened)
	s.joined(ctx, identityOfRun(run), wakes)
}

// markPulled records, once per job, that the job's harness has asked for notes —
// off the path that asked, under ctx stripped of its cancellation, so a hook
// answered from the view never waits on the write.
func (i *runInbox) markPulled(ctx context.Context, launch string) {
	i.mu.Lock()
	if i.pulled {
		i.mu.Unlock()
		return
	}
	i.pulled = true
	i.mu.Unlock()
	go i.recordPulled(context.WithoutCancel(ctx), launch)
}

// recordPulled is the write [runInbox.markPulled] makes off the path that
// asked, under base, within [steerPublishBudget] and the time the run's row
// takes to cross to the store (sandbox.RowBudget): a mark the store does not
// take is said, and asked for again at the job's next pull.
func (i *runInbox) recordPulled(base context.Context, launch string) {
	ctx, cancel := context.WithTimeout(base, sandbox.RowBudget(i.steers.store, steerPublishBudget))
	defer cancel()
	if _, err := i.steers.store.MarkPulled(ctx, i.ident.turn.TurnID, launch); err != nil {
		log.WarnContext(ctx, "steer_pull_unrecorded", "turn_id", i.ident.turn.TurnID,
			"error", err.Error())
		i.mu.Lock()
		i.pulled = false
		i.mu.Unlock()
	}
}

// inboxItems is how the run's harness admits each note of due not yet served
// to launch, by note id, where its session record confirms a take
// ([executor.ConfirmsByRecord]): the id it admits the note under and the
// digest of messages' text for it, the very text the poll hands over, so the
// serve recorded under them is one the job's receipts settle. Nil for a
// harness confirmed otherwise.
func (i *runInbox) inboxItems(launch string, due []openedNote, messages []string) map[string]sandbox.InboxItem {
	if !executor.ConfirmsByRecord(i.harness) {
		return nil
	}
	out := make(map[string]sandbox.InboxItem, len(due))
	for k, o := range due {
		if o.held.Served != launch {
			out[o.held.ID] = sandbox.InboxItem{ID: executor.InboxID(i.harness, launch, o.held.ID),
				Digest: executor.ItemDigest(messages[k])}
		}
	}
	return out
}

// Wait is the waiting notes past the cursor after, each served to the job on
// the record first — see [mcpbridge.SteerInbox].
//
// SERVED BEFORE IT IS ANSWERED ([sandbox.PendingStore.ServeNotes]), in a write
// of its own wherever the view does not show the mark yet: the harness admits
// what it is answered and delivers it, and records it read only once it has, so
// a note answered without the mark could sit in a session the record says
// never read it. A note the serve did not answer — its write failed, or the
// record moved on from the job — is not answered either, nor is any after it,
// and they wait for the next poll.
func (i *runInbox) Wait(ctx context.Context, after int) ([]mcpbridge.SteerNote, <-chan struct{}) {
	launch, watch := i.bound()
	if launch == "" {
		return nil, closedChan
	}
	i.markPulled(ctx, launch)
	view, opened, changed := watch.snapshot()
	if !liveFor(view, launch) {
		return nil, changed
	}
	var due []openedNote
	var messages []string
	var unserved []string
	// THE JOB'S OWN ITEMS AND A PERSON'S NOTES ALONE: one for another job is
	// never served to this one, so the cursor below would stop at it for good.
	for _, o := range forJob(opened, launch) {
		// A NOTE AT ITS POLL BUDGET is no new job's ([sandbox.RunNote.PollServes]):
		// passed over, never stopped at, so the notes after it are served.
		if o.held.Seq <= after || (o.held.Served != launch && o.held.PollServes >= o.held.MaxServes()) {
			continue
		}
		due = append(due, o)
		messages = append(messages, i.message(ctx, o))
		if o.held.Served != launch {
			unserved = append(unserved, o.held.ID)
		}
	}
	served := map[string]bool{}
	if len(unserved) > 0 {
		marked, err := i.steers.store.ServeNotes(ctx, i.ident.turn.TurnID, sandbox.NoteServe{
			IDs: unserved, Launch: launch, Statuses: steerTakeStatuses, Wakes: wakesHanded(due),
			Inbox: i.inboxItems(launch, due, messages)})
		if err != nil {
			log.WarnContext(ctx, "steer_notes_unserved", "turn_id", i.ident.turn.TurnID, "error", err.Error(),
				"detail", "the poll is answered without the notes the record could not mark served; they "+
					"wait for the next poll")
		}
		for _, n := range marked {
			served[n.ID] = true
		}
	}
	var out []mcpbridge.SteerNote
	for k, o := range due {
		// UP TO THE FIRST NOTE NOT MARKED, never past it: the poller's cursor
		// moves past every note it is answered, and one past an unmarked note
		// would never ask for that note again.
		if o.held.Served != launch && !served[o.held.ID] {
			break
		}
		out = append(out, mcpbridge.SteerNote{
			Seq: o.held.Seq, ID: o.held.ID, Inbox: executor.InboxID(i.harness, launch, o.held.ID),
			Message: messages[k],
		})
	}
	return out, changed
}

// Again grants the job's harness its second admission of the notes named that
// a compaction folded, each under its own id ([executor.AgainID]), and names
// those at their poll budget — see [mcpbridge.SteerInbox]. COUNTED WHEN SERVED,
// never offered: a note that never folds spends nothing.
func (i *runInbox) Again(ctx context.Context, ids []string) (map[string]string, []string, error) {
	launch, _ := i.bound()
	if launch == "" {
		return nil, nil, mcpbridge.ErrNoteUntaken
	}
	granted, spent, err := i.steers.store.AgainNotes(ctx, i.ident.turn.TurnID, sandbox.NoteAgain{IDs: ids,
		Launch: launch, Statuses: steerTakeStatuses})
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]string, len(granted))
	for _, id := range granted {
		out[id] = executor.AgainID(i.harness, launch, id)
	}
	return out, spent, nil
}

// Folded says each note named that the job's harness gave up once a compaction
// folded it and it could not be admitted again, once a note — see [mcpbridge.SteerInbox]. Only a note
// the job holds, by its id: the log carries no word of the note's.
func (i *runInbox) Folded(ctx context.Context, ids []string) {
	launch, watch := i.bound()
	if launch == "" {
		return
	}
	_, opened, _ := watch.snapshot()
	for _, o := range forJob(opened, launch) {
		if !slices.Contains(ids, o.held.ID) {
			continue
		}
		turn, id := i.ident.turn.TurnID, o.held.ID
		i.steers.warnOnce(turn, "folded/"+id, func() {
			log.WarnContext(ctx, "steer_note_folded", "turn_id", turn, "launch_id", launch, "note_id", id,
				"detail", "the run's harness compacted over the note and could not admit it again (compacted "+
					"twice, or its poll serves spent), so its model was never handed it as a current instruction; "+
					"it waits for the review")
		})
	}
}

// Read records the notes named read at the run's next step, and publishes them
// delivered there; see [mcpbridge.SteerInbox].
//
// A CONFIRMATION'S READ IS TRUE WHETHER OR NOT ITS ANSWER ARRIVES, but its
// report rides on that answer, so a take that answered an error is settled on
// this path ([runInbox.confirm]), and a note named that the record holds read
// at a step of this job is published delivered all the same, as one record
// ([runInbox.confirmedBefore]). A note the inbox cannot take (it serves no job,
// [runInbox.bound]; its record takes no read of the job, [waitsUntaken]; it
// waits for a key this node lacks, [runSteers.waitsForItsKey]) is turned away
// to ask again, never answered done. The package doc's "An OpenCode
// confirmation is recorded once, and reported" says why.
func (i *runInbox) Read(ctx context.Context, ids []string) error {
	launch, watch := i.bound()
	switch {
	case launch == "":
		return mcpbridge.ErrNoteUntaken
	case len(ids) == 0:
		return nil
	}
	_, opened, _ := watch.snapshot()
	var candidates []openedNote
	for _, o := range forJob(opened, launch) {
		if slices.Contains(ids, o.held.ID) {
			candidates = append(candidates, o)
		}
	}
	if len(candidates) < len(ids) {
		// A NOTE THE VIEW HAS NOT CAUGHT UP WITH, or one already read: the
		// record decides, read now.
		run, found, err := i.steers.store.Get(ctx, i.ident.turn.TurnID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		candidates = nil
		for _, o := range i.steers.waiting(run.TurnID, run.Steer.Notes, launch) {
			if slices.Contains(ids, o.held.ID) {
				candidates = append(candidates, o)
			}
		}
	}
	taken, err := i.confirm(ctx, launch, candidates)
	if err != nil || len(taken) == len(ids) {
		return err
	}
	// A NOTE NAMED THAT THIS CONFIRMATION DID NOT TAKE: the record decides,
	// read after the take, which another reader may have overtaken.
	run, found, err := i.steers.store.Get(ctx, i.ident.turn.TurnID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	i.confirmedBefore(ctx, run, launch, ids, taken)
	// ASKED FIRST, so a note waiting for its key is said wherever the
	// confirmation is turned away.
	keyless := i.steers.waitsForItsKey(ctx, run, launch, ids)
	if keyless || waitsUntaken(run, candidates) {
		return mcpbridge.ErrNoteUntaken
	}
	return nil
}

// waitsForItsKey reports whether run's record, read after a confirmation's
// take by job launch ([runInbox.Read]), still holds waiting a note the
// confirmation names, one that job is handed ([sandbox.RunNote.IsFor]),
// sealed under a key this node does not hold ([recordseal.KeyUnknown]). No
// take of this node's records such a note, since a take hands over only a
// note it opened, and a node that holds the key takes it as it takes any
// ([runSteers.open]). Each is said once, at WARN with the keys this node
// holds, as its open is ([runSteers.turnedAway]): the harness confirms it
// again on its own backoff, for as long as its run lasts.
func (s *runSteers) waitsForItsKey(ctx context.Context, run sandbox.PendingRun, launch string,
	ids []string) bool {
	found := false
	for _, n := range run.Steer.Notes {
		if !n.Waiting() || !n.IsFor(launch) || n.LateCall != nil || !slices.Contains(ids, n.ID) {
			continue
		}
		if !s.keyUnknown(n) {
			continue
		}
		found = true
		if _, said := s.turnedAway.LoadOrStore(run.TurnID+"/"+n.ID, struct{}{}); !said {
			log.WarnContext(ctx, "steer_read_key_unknown", "turn_id", run.TurnID, "note_id", n.ID,
				"held_keys", s.note.verifier.KeyIDs(),
				"detail", "the run's harness confirmed delivering a note sealed under a key this node "+
					"does not hold, which no read of this node's can take: the confirmation is turned "+
					"away to ask again until the run's session reaches a node that holds the key, and a "+
					"run that ends first leaves the note unconfirmed; add the key to secrets.keys")
		}
	}
	return found
}

// keyUnknown reports whether n waits for a key this node does not hold: a
// note's seal, or the seal of a wake its item carries ([runSteers.wakeKeyUnknown]).
func (s *runSteers) keyUnknown(n sandbox.RunNote) bool {
	if n.IsWake() {
		return s.wakeKeyUnknown(n)
	}
	_, verdict := s.note.verifier.Open(n.Sealed)
	return verdict == recordseal.KeyUnknown
}

// waitsUntaken reports whether run's record, read after a confirmation's take
// ([runInbox.Read]), still holds waiting a note of the take's candidates: one
// the take left. A take records every waiting note it names for its job while
// the record takes reads of the job, so such a note waits on a record that took
// none: its job's completion claimed the run, its ending is decided, or a later
// job carries it on. A candidate another reader took meanwhile waits no more,
// and is no such note.
func waitsUntaken(run sandbox.PendingRun, candidates []openedNote) bool {
	for _, c := range candidates {
		if slices.ContainsFunc(run.Steer.Notes, func(n sandbox.RunNote) bool {
			return n.ID == c.held.ID && n.Waiting()
		}) {
			return true
		}
	}
	return false
}

// confirm records candidates read by job launch at the run's next step, and
// publishes delivered what it recorded ([runInbox.Read]).
//
// A TAKE THAT ANSWERED AN ERROR IS SETTLED HERE, in answer mode: its write may
// have landed, and the read it recorded is true, so it is never marked, and
// what the settle finds recorded is published delivered now. Under a bound of
// its own ([runSteers.settleOnce]), never the request's: the plugin gives up at
// ten seconds, a settle under that request would fail at once, and no node
// would report the note once the run is over. A settle that finds nothing
// recorded leaves the notes waiting and answers the take's error, so the plugin
// confirms again; so does one the store does not answer, and that confirmation
// finds what the take recorded ([runInbox.confirmedBefore]).
func (i *runInbox) confirm(ctx context.Context, launch string, candidates []openedNote) ([]sandbox.RunNote,
	error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.held.ID)
	}
	read := sandbox.NoteRead{Phase: string(types.PhaseExecute), Iteration: i.round, Round: 1,
		Boundary: string(types.SteerBoundaryStep), Launch: launch, Take: takeName()}
	// ITS HARNESS'S OWN WORD THAT IT DELIVERED THEM, so a late call among them
	// is told as one the job's session holds ([sandbox.NoteTake.Confirms]).
	taken, err := i.steers.store.TakeNotes(ctx, i.ident.turn.TurnID, sandbox.NoteTake{
		IDs: ids, Launch: launch, Statuses: steerTakeStatuses, Read: read, Confirms: true,
	})
	if err != nil {
		settled, settleErr := i.steers.settleOnce(context.WithoutCancel(ctx), i.ident.turn.TurnID,
			sandbox.NoteSettle{Take: read.Take})
		if settleErr != nil || len(settled) == 0 {
			return nil, err
		}
		// RECORDED, ITS ANSWER LOST: reported now.
		taken = settled
	}
	i.steers.delivered(ctx, i.ident, taken, notesTaken(candidates, taken))
	i.steers.joined(ctx, i.ident, wakesTaken(candidates, taken))
	return taken, nil
}

// wakesTaken is the wake items of candidates a take recorded read, each with
// the read it recorded and the wakes it opened as, in the take's order.
func wakesTaken(candidates []openedNote, taken []sandbox.RunNote) []openedNote {
	var out []openedNote
	for _, t := range taken {
		for _, c := range candidates {
			if c.held.ID == t.ID && c.wakes != nil && t.Read != nil {
				out = append(out, openedNote{held: t, wakes: c.wakes})
				break
			}
		}
	}
	return out
}

// confirmedBefore publishes delivered every note of ids that run's record holds
// read at a step of job launch and that this confirmation did not take: an
// earlier confirmation's, whose answer never reached the plugin, which
// confirms the note again ([runInbox.Read]). Under the id that confirmation's
// report took, from the turn and the note, and at the instant of its read, so
// a note both reported is one record; a wake item so is claimed again too.
// Only a step's read of this job is one: a read by its brief, a review or
// another job is reported by whatever read it, and a note gone by nobody here.
func (i *runInbox) confirmedBefore(ctx context.Context, run sandbox.PendingRun, launch string, ids []string,
	taken []sandbox.RunNote) {
	var held []sandbox.RunNote
	var notes []steer.Note
	var wakes []openedNote
	for _, n := range run.Steer.Notes {
		read := n.Read
		if read == nil || read.Launch != launch || read.Boundary != string(types.SteerBoundaryStep) ||
			read.Unanswered || !slices.Contains(ids, n.ID) ||
			slices.ContainsFunc(taken, func(t sandbox.RunNote) bool { return t.ID == n.ID }) {
			continue
		}
		switch {
		case n.IsWake():
			if opened := i.steers.openWakes(run.TurnID, n); len(opened) > 0 {
				wakes = append(wakes, openedNote{held: n, wakes: opened})
			}
		case n.For != "":
		default:
			if note, ok := i.steers.open(run.TurnID, n); ok {
				held = append(held, n)
				notes = append(notes, note)
			}
		}
	}
	i.steers.delivered(ctx, i.ident, held, notes)
	i.steers.joined(ctx, i.ident, wakes)
}

// Close ends the inbox — see [mcpbridge.SteerInbox].
func (i *runInbox) Close() {
	if i == nil {
		return
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return
	}
	i.closed = true
	watch := i.watch
	i.mu.Unlock()
	watch.stop()
}

// closedChan is a channel that is always closed: what a wait on an inbox that
// serves nothing is handed, so it never holds.
var closedChan = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// --- a view of one run's notes ---------------------------------------------

// noteWatch is this node's view of one run's notes, following its record: the
// newest state, the waiting notes in it that open, and a channel closed at each
// change. DERIVED, never the only copy of anything — rebuilt from the record by
// any node that needs one.
type noteWatch struct {
	steers *runSteers
	turnID string

	mu      sync.Mutex
	view    sandbox.NotesView
	opened  []openedNote
	changed chan struct{}
	stopped bool

	cancel context.CancelFunc
	done   chan struct{}
}

// steerRewatch bounds how fast a view whose watch broke watches again.
const (
	steerRewatchBase    = 100 * time.Millisecond
	steerRewatchCeiling = 5 * time.Second
)

// watch starts a view of turnID's notes, which follows the record until stopped.
func (s *runSteers) watch(ctx context.Context, turnID string) *noteWatch {
	ctx, cancel := context.WithCancel(ctx)
	w := &noteWatch{steers: s, turnID: turnID, changed: make(chan struct{}), cancel: cancel,
		done: make(chan struct{})}
	go w.follow(ctx)
	return w
}

// follow keeps the view on the record, watching again whenever the watch stops
// hearing — the record's newest state is what a new watch starts with, so
// nothing written meanwhile is lost.
func (w *noteWatch) follow(ctx context.Context) {
	defer close(w.done)
	for attempt := 0; ctx.Err() == nil; attempt++ {
		views, err := w.steers.store.WatchNotes(ctx, w.turnID)
		if err == nil {
			for view := range views {
				attempt = 0
				w.update(view)
			}
		} else if !errors.Is(err, context.Canceled) {
			log.Debug("steer_notes_unwatched", "turn_id", w.turnID, "error", err.Error())
		}
		pause := backoff.Jitter(backoff.Doubling(attempt, steerRewatchBase, steerRewatchCeiling), 0.2)
		timer := time.NewTimer(pause)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// update takes a new state of the record into the view: every waiting note and
// item that opens, each reader taking what its own job is handed ([forJob]).
func (w *noteWatch) update(view sandbox.NotesView) {
	opened := w.steers.waitingItems(w.turnID, view.Steer.Notes)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.view, w.opened = view, opened
	close(w.changed)
	w.changed = make(chan struct{})
}

// snapshot is the view now: the record's state, its waiting notes that open,
// and a channel closed at the next change or when the view stops.
func (w *noteWatch) snapshot() (sandbox.NotesView, []openedNote, <-chan struct{}) {
	if w == nil {
		return sandbox.NotesView{}, nil, closedChan
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return w.view, w.opened, closedChan
	}
	return w.view, slices.Clone(w.opened), w.changed
}

// stop ends the view and wakes everyone waiting on it. Safe twice.
func (w *noteWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	close(w.changed)
	w.mu.Unlock()
	w.cancel()
	<-w.done
}
