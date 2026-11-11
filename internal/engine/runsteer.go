package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A person's note to a turn whose executor is out: it waits sealed on the
// run's record (ADR-0067).
//
// # Where a turn's notes wait, and who has to agree on them
//
// Before a turn launches its executor, its notes wait in the box its first
// segment holds, on the node running it (steer.go). From the run's launch to
// the turn's end they wait on the RUN'S RECORD in the coordination store
// ([sandbox.RunSteer]) — the whole company, now: the node a person's request
// reaches, the node serving the run's tool bridge, the node that resumes the
// turn and the node that finishes the run's ending can be four processes
// across a restart and a seat moving, and each already reads the record. So
// any node files a note there with one compare-and-swap, and nothing that
// holds a note lives in one node's memory alone: what a node keeps — a bridge
// session's view of the record, a held poll — is derived and rebuilt from it.
//
// # Sealed, because a note is an instruction
//
// The coordination store decides between writers by compare-and-set and
// authenticates none of them (ADR-0031), and a note on a record is still a
// person's words a model will read as theirs. So every note is SEALED under the
// fleet keyring by the node that files it, in a key domain of its own
// ([SteerNoteDomain]), over the note bound to its turn and its id; and the
// record carries a sealed ANCHOR naming the seat the run serves
// ([SteerSeatDomain]), written by the launch, on which a person's authority to
// steer the turn is decided. Every reader opens before use: a note whose seal
// does not open is never delivered, carried or attributed (logged at ERROR as
// steer_note_unverified), and one sealed under a key this node does not hold
// waits, with a WARN naming the keys it holds — the precedent is a person's
// answer to a parked run (internal/sandbox, answerseal.go). An anchor this node
// cannot open decides nothing, and one sealed under a key it does not hold is a
// WARN naming the keys it holds (steer_seat_key_unknown), and steer_turn says
// only who fixes it, as a condition of the fleet ([runSteers.openSeat]).
// What is NOT sealed is the `read` marker, which only the store's own
// compare-and-set guards: a party on the broker could re-deliver a genuine
// note to its own turn, or hide one from in-run delivery, and the note still
// reaches the reviewer or expires — the residual ADR-0067 states.
//
// # Every read is on the record, so the record is the carry
//
// A note is handed to a model only by the write that records it read: the
// launch's own for a brief ([sandbox.PendingStore.BeginLaunch]), a take for the
// run's harness or the reviewer ([sandbox.PendingStore.TakeNotes]). So the record's
// read notes are exactly the notes the turn has read, and a segment that
// resumes the turn starts from them ([runSteers.carried]) — whichever node it
// is on.
//
// # A take whose answer was lost is settled, never left saying it was read
//
// A take's write can land and its answer not: a hook's take is held to five
// seconds (internal/api/mcpbridge's steerTakeBudget), and a compare-and-swap the
// store answers after them, or one whose answer a broken connection lost, may
// still have landed — the record then says the job read a note its harness was
// never handed. So every take names itself ([sandbox.NoteRead.Take]), and one
// whose call answered an error is SETTLED by that name
// ([sandbox.PendingStore.SettleTake]), which also leaves its write, if it is
// still in flight, unable to land after. A hook's take settles OFF THE HOOK'S
// PATH, its notes marked unanswered, since the hook has already been answered
// nothing ([runSteers.settleUnanswered]): its job's session lacks them, so no
// capture of it counts them held, and a launch that continues it tells them
// again, while the review still reads them, carried as every read note is. A
// review's take settles ON ITS OWN PATH, in answer mode, and hands over what its
// write recorded, its round still waiting on it; where the store answers that
// settle no more than the take, the take is kept and settled again by the
// segment's next round or brief, standing meanwhile for a note waiting, and is
// handed off the path, marked, only when the segment ends ([rowSource]). A
// settle off the path asks until its store answers, or until the node stops
// ([runSteers.settleUnanswered]).
//
// # A note a harness may have delivered is never one the record says nothing of
//
// OpenCode's plugin is handed its notes by a poll and confirms each once
// OpenCode delivered it, and only the confirmation records the note read: a
// harness cut off between the two — or whose confirmation the store never took —
// leaves a session that may hold a note the record says waits, which the next
// launch's brief would tell it as one it had not read. So a poll answers a note
// only once the record says it was SERVED to the job
// ([sandbox.PendingStore.ServeNotes]), in a write before its answer, and a
// launch that continues the job's session tells a served note as one it may
// already show ([servedNotes], internal/agent/runner).
//
// # Who records what became of a note
//
// The node whose step read it: the runner for a round of the engine's loop or
// a launch's brief, the run inbox of the node serving the run's bridge for
// a read inside the run ([runInbox]), under ids derived from the turn and the
// note. And the node that finishes the run's ending, for every note no job ran
// with when the turn is over — still waiting, or read by the brief of a job
// that never started ([runSteers.expired]) — under ids derived from the ending,
// so a repeat of either is the same record. Never at a park: a question can
// wait days, and the turn that resumes reads what waited.

// SteerNoteDomain is the key domain a note on a run's record is sealed in: its
// own, so no frame of another channel or a state log opens as a note, and no
// note as anything else.
const SteerNoteDomain = "seat.steer.note"

// SteerSeatDomain is the key domain a run record's seat anchor is sealed in.
const SteerSeatDomain = "seat.steer.seat"

// steerTakeStatuses are the statuses a run's own harness takes notes in: from the
// launch that opens its job — the job may be working before its turn has
// parked into the record — to the claim of its completion.
var steerTakeStatuses = []string{sandbox.StatusLaunching, sandbox.StatusRunning}

// steerDeliveredNamespace and steerExpiredNamespace derive the ids a note's
// records are published under: delivered by a run's inbox, from the turn and
// the note — a note is read once — and expired by an ending, from the turn, the
// ending and the note.
var (
	steerDeliveredNamespace = uuid.MustParse("3c7a9e5d-4f1b-5a2c-8e6d-9b0f1a2c3d4e")
	steerExpiredNamespace   = uuid.MustParse("8d2e6f1a-7b3c-5d4e-9f0a-1b2c3d4e5f6a")
)

// steerPublishBudget bounds one publish of what became of a note, made off the
// path that read it.
const steerPublishBudget = 5 * time.Second

// steerSettlePatience is how long a settle the store has not answered is taken
// for a blip ([runSteers.settleUnanswered]): past it, the settle is said
// unsettled (steer_take_unsettled) and goes on asking, and a review stops
// holding its rounds open for one ([rowSource.Waiting]).
//
// THIRTY SECONDS. The take a settle follows failed on a store that did not
// answer in time — a hook's take within its five seconds — which is a store
// mid leader election (four seconds at nats-server's raft default, nine at
// worst) or reconnecting, and its answer comes once that is over. Thirty
// seconds outlasts it with room, as a capture's record does (internal/sandbox's
// sessionRecordBudget); a store silent past it is an outage its own alarms
// name.
const steerSettlePatience = 30 * time.Second

// steerSettleAttempt bounds one attempt at a settle: an attempt the store has
// not answered in TEN SECONDS — past a leader election's nine at worst — is
// asked again, so a request that hangs never holds a run's settle, nor keeps
// it from hearing that its node has stopped.
const steerSettleAttempt = 10 * time.Second

// steerSettleRetryBase and steerSettleRetryCeiling space a run's rounds of
// settle attempts: a second, doubling to eight, for as long as its store does
// not answer — a round every eight seconds at most is a few reads of one
// record a minute, which a store just back from an outage barely notices.
const (
	steerSettleRetryBase    = time.Second
	steerSettleRetryCeiling = 8 * time.Second
)

// runSteers is this node's half of a turn's notes on its run's record: it seals
// and opens them, files them for a person, takes them for a resumed segment and
// for a run's own harness, and records what became of them.
type runSteers struct {
	note *sealPair
	seat *sealPair

	store sandbox.PendingStore
	pub   queue.Publisher
	now   func() time.Time

	// warned is every note whose seal this node has logged it cannot open, so
	// a view that follows a busy record does not say so on every write.
	warned sync.Map

	// stopped is closed when this node stops ([Engine.teardownSignal]): a
	// settle this node makes off the path that took asks its store until the
	// store answers, or until then, and no longer
	// ([runSteers.settleUnanswered]). Nil is a node that never stops, as an
	// engine assembled by hand is.
	stopped <-chan struct{}

	// settleMu guards settling: every run this node is settling takes of off
	// the path that took, by turn id — one loop a run, whatever its takes.
	settleMu sync.Mutex
	settling map[string]*runSettle
}

// runSettle is one run's takes this node is settling off the path that took
// ([runSteers.settleUnanswered]): their names, when the first was lost, and
// whether the settle was said unsettled.
type runSettle struct {
	takes []string
	since time.Time
	said  bool
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

// steerSeals is the two seals a run's record carries notes under, built over the
// fleet keyring before anything is opened, as every channel's are.
type steerSeals struct{ note, seat *sealPair }

func newSteerSeals(ring recordseal.Keyring) (steerSeals, error) {
	note, err := newSealPair(SteerNoteDomain, ring)
	if err != nil {
		return steerSeals{}, err
	}
	seat, err := newSealPair(SteerSeatDomain, ring)
	if err != nil {
		return steerSeals{}, err
	}
	return steerSeals{note: note, seat: seat}, nil
}

// Domains is the key domains the seals sign in, for the gate that holds every
// channel's domains apart.
func (s steerSeals) Domains() (string, string) { return SteerNoteDomain, SteerSeatDomain }

// newRunSteers is a node's run steers over its run store and its queue.
func newRunSteers(seals steerSeals, store sandbox.PendingStore, pub queue.Publisher) *runSteers {
	return &runSteers{note: seals.note, seat: seals.seat, store: store, pub: pub, now: time.Now}
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
		OperatorID: note.OperatorID, At: note.At.UTC(),
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
		ByKind: iam.ActorKind(sealed.ByKind), OperatorID: sealed.OperatorID, At: sealed.At}, true
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
// condition of the fleet no wait clears ([steer.ErrSeatKeyUnknown]), so the
// keys this node holds and the advice go to its log, where whoever administers
// the keyring reads them, and nothing anybody steering reads names them: they
// were ids nobody steering chose, beside a file only that administrator
// changes.
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
// ([sandbox.LaunchRequest.BriefNotes]).
func (s *runSteers) briefNotes(turnID string, round int, notes []steer.Note) ([]sandbox.RunNote, error) {
	if s == nil || len(notes) == 0 {
		return nil, nil
	}
	out := make([]sandbox.RunNote, 0, len(notes))
	for _, n := range notes {
		sealed, err := s.seal(turnID, n)
		if err != nil {
			return nil, err
		}
		sealed.Read = &sandbox.NoteRead{Phase: string(types.PhaseExecute), Iteration: round,
			Round: 1, Boundary: string(types.SteerBoundaryBrief)}
		out = append(out, sealed)
	}
	return out, nil
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
		Unanswered: read.Unanswered}
}

// takeName names one take of a run's notes ([sandbox.NoteRead.Take]): fresh for
// every call, so a settle finds the reads of that call and no other's.
func takeName() string { return uuid.NewString() }

// settleUnanswered settles, off the path that took, a take of turnID's notes
// whose answer never reached its reader ([sandbox.PendingStore.SettleTake]):
// every note its write recorded is marked unanswered, and a write of it still
// in flight can no longer land.
//
// UNTIL THE STORE ANSWERS, never until a clock says so: a settle given up leaves
// the record saying the run read notes its reader was never handed — which a
// capture of its session then counts held — while the store that did not answer
// in time answers once it is back. So it is asked again, rounds a second apart
// doubling to eight, until an attempt is answered — the notes marked, or the
// record gone with the run — or the node stops ([runSteers.stopped]); said at
// ERROR once [steerSettlePatience] has passed unanswered, and again where the
// node stops with it unsettled, since what a stopping node did not settle no
// other node knows to.
//
// ONE LOOP A RUN: a take a run loses while its loop runs joins it, so a run
// whose store does not answer holds one loop, however many takes it loses. The
// loop keeps ctx's values — the first take's trace — and none of its ending:
// the take's own is spent, or its hook gone, or its segment over.
func (s *runSteers) settleUnanswered(ctx context.Context, turnID, take string) {
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	if run, ok := s.settling[turnID]; ok {
		if !slices.Contains(run.takes, take) {
			run.takes = append(run.takes, take)
		}
		return
	}
	if s.settling == nil {
		s.settling = map[string]*runSettle{}
	}
	run := &runSettle{takes: []string{take}, since: s.now()}
	s.settling[turnID] = run
	go s.settleRun(context.WithoutCancel(ctx), turnID, run)
}

// settleRun asks turnID's store to settle run's takes, round after round, until
// it has answered for every one, or the node stops — see
// [runSteers.settleUnanswered]. base is the loop's own context, which nothing
// but the node's stop ends ([runSteers.stopped]): watched by a goroutine of the
// loop's own, which goes with the loop.
func (s *runSteers) settleRun(base context.Context, turnID string, run *runSettle) {
	life, end := context.WithCancel(base)
	defer end()
	if s.stopped != nil {
		go func() {
			select {
			case <-s.stopped:
				end()
			case <-life.Done():
			}
		}()
	}
	for round := 1; ; round++ {
		s.settleMu.Lock()
		takes := slices.Clone(run.takes)
		s.settleMu.Unlock()
		var answered []string
		var unanswered error
		for _, take := range takes {
			ctx, cancel := context.WithTimeout(life, steerSettleAttempt)
			marked, err := s.store.SettleTake(ctx, turnID, sandbox.NoteSettle{Take: take, Unanswered: true})
			cancel()
			if err != nil {
				unanswered = err
				continue
			}
			answered = append(answered, take)
			if len(marked) > 0 {
				log.WarnContext(base, "steer_take_unanswered", "turn_id", turnID, "notes", len(marked),
					"detail", "a take of the run's notes was recorded and its answer lost: its reader never "+
						"had them, the turn carries them as read, and a launch that continues the run's "+
						"session tells them again")
			}
		}
		s.settleMu.Lock()
		run.takes = slices.DeleteFunc(run.takes, func(t string) bool { return slices.Contains(answered, t) })
		left := len(run.takes)
		if left == 0 {
			delete(s.settling, turnID)
			s.settleMu.Unlock()
			return
		}
		// SAID ONLY OF A ROUND THAT MET AN ERROR: one whose every attempt was
		// answered asked no take that is left — what is left joined the run's
		// loop while the round was asked, and has not been asked yet — so there
		// is nothing unanswered to say, and its error to say it with is none.
		overdue := unanswered != nil && !run.said && s.now().Sub(run.since) >= steerSettlePatience
		run.said = run.said || overdue
		s.settleMu.Unlock()
		if overdue {
			log.ErrorContext(base, "steer_take_unsettled", "turn_id", turnID, "takes", left,
				"error", unanswered.Error(),
				"detail", "a take of the run's notes answered an error, and the store has not answered its "+
					"settle: where the store recorded it, the record says the run read notes its reader was "+
					"never handed, and a continuation of its session is not told them. Asked again until "+
					"the store answers")
		}
		timer := time.NewTimer(backoff.Jitter(backoff.Doubling(round, steerSettleRetryBase,
			steerSettleRetryCeiling), 0.2))
		select {
		case <-timer.C:
		case <-life.Done():
			timer.Stop()
			s.settleMu.Lock()
			delete(s.settling, turnID)
			left = len(run.takes)
			s.settleMu.Unlock()
			log.ErrorContext(base, "steer_take_unsettled", "turn_id", turnID, "takes", left,
				"detail", "this node stopped before the store answered the settle of a take of the run's "+
					"notes: where the store recorded it, the record says the run read notes its reader was "+
					"never handed, and no other node knows to settle it")
			return
		}
	}
}

// carried is every note run's record holds read, opened, in the order read —
// what a segment resuming the turn carries ([runner.Resume.Steered]). A note
// that does not open is not carried: nobody can say it is a person's.
//
// THE ORDER READ IS THE RECORD'S OWN: the place of the write that recorded each
// read ([sandbox.NoteRead.Order]), and among the reads one write recorded, the
// order the notes were filed in — never a read's time. The writes that record
// reads are made by whichever nodes launch the run, serve its bridge and resume
// the turn, each stamping its own clock, and a note is the later word: two
// clocks a second apart would otherwise hand the reviewer and every later brief
// a correction ahead of the note it corrects.
//
// THE WAITING NOTES GO BEFORE THE SORT, never into it: a comparison that
// answered "equal" for any pair with a waiting note in it would not be an
// order at all — a waiting note between two read ones would tie with both
// while they did not tie with each other — and the sort could leave the read
// notes out of the order they were read in. A record holds such a note
// whenever one waits while a later one is read: a seal this node cannot open
// yet, or a later note a harness took first.
func (s *runSteers) carried(run sandbox.PendingRun) []steer.Read {
	if s == nil {
		return nil
	}
	notes := slices.DeleteFunc(slices.Clone(run.Steer.Notes), sandbox.RunNote.Waiting)
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
func servedNotes(run sandbox.PendingRun) []string {
	var out []string
	for _, n := range run.Steer.Notes {
		if n.Served != "" {
			out = append(out, n.ID)
		}
	}
	return out
}

// openedNote is a waiting note on a record, and the note it opens as.
type openedNote struct {
	held sandbox.RunNote
	note steer.Note
}

// waiting is the record's waiting notes that open, oldest first.
func (s *runSteers) waiting(turnID string, notes []sandbox.RunNote) []openedNote {
	var out []openedNote
	for _, n := range notes {
		if !n.Waiting() {
			continue
		}
		if note, ok := s.open(turnID, n); ok {
			out = append(out, openedNote{held: n, note: note})
		}
	}
	return out
}

// runIdentity is the turn a record of a note names, and the trace it is
// published under.
type runIdentity struct {
	turn  steer.Turn
	trace events.TraceContext
}

func identityOfRef(ref sandbox.TurnRef) runIdentity {
	return runIdentity{
		turn: steer.Turn{Agent: ref.AgentID, AgentHandle: ref.AgentHandle, Role: ref.Role,
			TurnID: ref.TurnID, WorkKey: ref.WorkKey},
		trace: events.TraceContext{TraceID: ref.TraceID, ParentSpanID: ref.SpanID},
	}
}

func identityOfRun(run sandbox.PendingRun) runIdentity {
	return runIdentity{
		turn: steer.Turn{Agent: run.AgentID, AgentHandle: run.AgentHandle, Role: run.Role,
			TurnID: run.TurnID, WorkKey: run.WorkKey},
		trace: events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID},
	}
}

// publish publishes what became of one note under the given identity, or a
// fresh one where id is nil.
func (s *runSteers) publish(ctx context.Context, ident runIdentity, note steer.Note,
	read *steer.Reading, id uuid.UUID, at time.Time,
) error {
	if s.pub == nil {
		return nil
	}
	ev := events.New(steer.Record(ident.turn, note, read), ident.trace)
	if id != uuid.Nil {
		ev.ID = id
	}
	if !at.IsZero() {
		ev.Timestamp = at.UTC()
	}
	ev.Source = ident.turn.Role
	return s.pub.Publish(ctx, topics.Event(ev.Type), ev)
}

// expired records every note on run's record that no job ran with as expired
// ([sandbox.PendingRun.UnreadNotes]: the ones still waiting, and the ones the
// brief of a job that never started recorded read) — the step a run's ending
// takes before it deletes the record ([sandbox.CoordinatorOptions.SteersExpired])
// — under ids derived from the turn, the ending and the note, at the ending's
// own instant, so a repeat of the ending publishes the same records. THE ONE
// PUBLISHER of an expiry for a note that reached a record: nothing else that
// read one reports it, so no note is reported twice. A note that does not open
// gets no record: nothing can attribute it.
func (s *runSteers) expired(ctx context.Context, run sandbox.PendingRun) error {
	if run.Ending == nil {
		return nil
	}
	ident := identityOfRun(run)
	for _, n := range run.UnreadNotes() {
		note, ok := s.open(run.TurnID, n)
		if !ok {
			log.Warn("steer_note_expired_unrecorded", "turn_id", run.TurnID, "note_id", n.ID,
				"detail", "the turn ended with this note unread, and its seal does not open "+
					"here, so no record says what became of it")
			continue
		}
		id := uuid.NewSHA1(steerExpiredNamespace, []byte(run.TurnID+"/"+run.Ending.ID+"/"+n.ID))
		if err := s.publish(ctx, ident, note, nil, id, run.Ending.At); err != nil {
			return fmt.Errorf("engine: recording note %s to turn %s expired: %w", n.ID, run.TurnID, err)
		}
	}
	return nil
}

// --- a resumed segment's notes: the run's record ---------------------------

// rowSource is the turn's notes as a segment that resumes it from its executor
// reads them: off the run's record, taken under the segment's claim
// ([runner.SteerSource]).
//
// # A take whose answer was lost is the segment's own to settle
//
// A take whose call answered an error may have landed, and what it recorded is
// read by the record's word, so no later take hands it over. Where its settle
// answers on the take's own path, what it recorded is handed over there and
// then; where the store does not answer that either, the take is LOST and kept
// here, by name: settled again on the segment's own path, in answer mode, at
// each later round's take and at a relaunch's brief, and handed over by the
// first that the store answers — the review reads it, or the relaunch's run
// does. Until then a lost take stands for a note waiting ([rowSource.Waiting]),
// so a round of the review asking for tools ends and the next one opens with
// the note, for as long as [steerSettlePatience] takes a silent store for a
// blip. A take still lost when the segment ends is handed off its path at
// [rowSource.Close], marked unanswered where it recorded anything
// ([runSteers.settleUnanswered]): nothing of the segment reads it any more, and
// a launch that continues the run's session tells it again.
type rowSource struct {
	steers *runSteers
	run    sandbox.PendingRun
	fence  sandbox.Fence
	watch  *noteWatch

	// mu guards lost, lostAt and briefed.
	mu sync.Mutex
	// lost is every take of this segment whose call and settle both answered
	// an error and whose settle has not answered since, by name, oldest first,
	// and lostAt when the first of them was lost.
	lost   []string
	lostAt time.Time
	// briefed is the take each note a relaunch's brief was handed from a
	// settle came from, by note id: a launch that fails before its write hands
	// the brief back, and its takes with it ([rowSource.Return]).
	briefed map[string]string
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
	candidates := r.steers.waiting(run.TurnID, run.Steer.Notes)
	return append(notesOf(settled), r.take(ctx, candidates, readOf(at))...)
}

// take records candidates read, and answers the ones the write took.
//
// A TAKE THAT ANSWERED AN ERROR IS SETTLED HERE, its round still waiting on it:
// its write may have landed with its answer lost, and what it recorded is read
// by the record's word, so no later take hands it over — the settle answers it,
// and it is handed over now. One whose settle the store does not answer either
// is LOST, and settled again by the segment's next take or brief
// ([rowSource.settleLost]).
func (r *rowSource) take(ctx context.Context, candidates []openedNote, read sandbox.NoteRead) []steer.Note {
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
	return notesTaken(candidates, taken)
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
// settle answers now ([rowSource.settleLost]), then the waiting notes that open
// — a peek, which the launch's own write records read — and every note the
// record marks unanswered, by id, read off the same record.
//
// THE MARKS AS THE RECORD HOLDS THEM NOW, never only as the segment's claim
// read them: a take's settle is made off the path that took, and can mark a
// read after this segment resumed from the record — so the relaunch's briefs,
// built from the reads the turn carries, count the mark as the record does.
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
		return steer.Briefing{Notes: notes}
	}
	if !found || run.Ending != nil {
		return steer.Briefing{Notes: notes}
	}
	for _, o := range r.steers.waiting(run.TurnID, run.Steer.Notes) {
		notes = append(notes, o.note)
	}
	return steer.Briefing{Notes: notes, Unanswered: unansweredNotes(run)}
}

// unansweredNotes is every note on run's record whose read the record marks
// unanswered ([sandbox.NoteRead.Unanswered]), by id.
func unansweredNotes(run sandbox.PendingRun) []string {
	var out []string
	for _, n := range run.Steer.Notes {
		if n.Read != nil && n.Read.Unanswered {
			out = append(out, n.ID)
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
// and the ending that deletes the record reports them ([runSteers.expired]) —
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
	view, opened, _ := r.watch.snapshot()
	return view.Found && r.live(view.Status, view.LaunchID, view.EndingDecided) && len(opened) > 0
}

// Close stops the segment's view of the record, and hands every take still
// lost off the segment's path, to be marked unanswered where it recorded
// anything ([runSteers.settleUnanswered]): nothing of the segment reads it any
// more. Under ctx stripped of its ending, which the settle takes, as a teardown
// does. Safe twice: the takes are handed off once.
func (r *rowSource) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.watch.stop()
	r.mu.Lock()
	lost := r.lost
	r.lost, r.briefed = nil, nil
	r.mu.Unlock()
	for _, take := range lost {
		r.steers.settleUnanswered(ctx, r.run.TurnID, take)
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

	mu     sync.Mutex
	launch string
	watch  *noteWatch
	closed bool
	pulled bool
}

var _ mcpbridge.SteerInbox = (*runInbox)(nil)

// inbox is the inbox of a run identified by ident, for the turn iteration round;
// it serves nothing until it is bound to the run's job.
//
// ONE INBOX WHATEVER THE RUN'S HARNESS: Claude Code's hooks take notes through
// [runInbox.Take] and OpenCode's plugin through [runInbox.Wait] and
// [runInbox.Read], and each reaches only the half its own harness calls, so the
// inbox has nothing to tell apart.
func (s *runSteers) inbox(ident runIdentity, round int) *runInbox {
	if s == nil {
		return nil
	}
	return &runInbox{steers: s, ident: ident, round: round}
}

// Bind starts serving the inbox for the job launch: its view of the run's
// record follows the record from here, under ctx stripped of its cancellation —
// the inbox lives as long as the job's session, which outlives the turn that
// launched it, and the view is stopped by [runInbox.Close] alone.
func (i *runInbox) Bind(ctx context.Context, launch string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.launch != "" {
		return
	}
	i.launch = launch
	i.watch = i.steers.watch(context.WithoutCancel(ctx), i.ident.turn.TurnID)
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

// Take answers one hook of the run's main agent — see [mcpbridge.SteerInbox].
//
// AFTER A BATCH, OFF THE VIEW: a step with nothing waiting is answered from the
// inbox's view of the record, with no store read — the harness waits on the answer
// before its next request, at every batch. AT THE FINISH, OFF THE STORE: it is
// the run's last boundary, and a note filed a moment before must not be missed
// for a view one write behind.
func (i *runInbox) Take(ctx context.Context, event string) string {
	launch, watch := i.bound()
	if launch == "" {
		return ""
	}
	i.markPulled(ctx, launch)
	boundary := types.SteerBoundaryToolBatch
	var candidates []openedNote
	if event == mcpbridge.SteerEventFinish {
		boundary = types.SteerBoundaryFinish
		run, found, err := i.steers.store.Get(ctx, i.ident.turn.TurnID)
		if err != nil {
			log.WarnContext(ctx, "steer_take_unread", "turn_id", i.ident.turn.TurnID,
				"error", err.Error(), "detail", "the run finishes without its waiting notes, "+
					"which its review reads")
			return ""
		}
		if !found || run.Ending != nil || run.LaunchID != launch ||
			!slices.Contains(steerTakeStatuses, run.Status) {
			return ""
		}
		candidates = i.steers.waiting(run.TurnID, run.Steer.Notes)
	} else {
		view, opened, _ := watch.snapshot()
		if !liveFor(view, launch) {
			return ""
		}
		candidates = opened
	}
	if len(candidates) == 0 {
		return ""
	}
	messages := make([]string, 0, len(candidates))
	for _, c := range candidates {
		messages = append(messages, prompts.SteerMessage(c.note.By, c.note.Text))
	}
	candidates = candidates[:executor.ClaudeNotesThatFit(messages)]
	taken := i.take(ctx, launch, candidates, boundary)
	if len(taken) == 0 {
		return ""
	}
	hook := make([]executor.HookNote, 0, len(taken))
	for _, n := range taken {
		hook = append(hook, executor.HookNote{By: n.By, Message: prompts.SteerMessage(n.By, n.Text)})
	}
	return executor.ClaudeHookAnswer(event, hook)
}

// take records candidates read by the job at boundary, publishes them delivered
// and answers what it took.
//
// A TAKE THAT ANSWERED AN ERROR HANDS THE HARNESS NOTHING, and is settled off
// this path ([runSteers.settleUnanswered]): its ctx is the hook's five seconds
// (internal/api/mcpbridge's steerTakeBudget), and a write the store answered
// after them may have landed — so where it did, the notes it recorded are marked
// unanswered, which no capture of the job's session counts held, rather than
// left read by a harness that was handed nothing; and where it did not, they
// wait for the run's next step, the settle's own write keeping a write of the
// take still in flight from landing after it.
func (i *runInbox) take(ctx context.Context, launch string, candidates []openedNote,
	boundary types.SteerBoundary,
) []steer.Note {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.held.ID)
	}
	read := sandbox.NoteRead{Phase: string(types.PhaseExecute), Iteration: i.round, Round: 1,
		Boundary: string(boundary), Launch: launch, Take: takeName()}
	taken, err := i.steers.store.TakeNotes(ctx, i.ident.turn.TurnID, sandbox.NoteTake{
		IDs: ids, Launch: launch, Statuses: steerTakeStatuses, Read: read,
	})
	if err != nil {
		log.WarnContext(ctx, "steer_take_failed", "turn_id", i.ident.turn.TurnID,
			"error", err.Error(), "detail", "the run's harness is handed nothing at this step: the notes "+
				"wait for its next one, or, where the store recorded this take, are marked unanswered, read "+
				"by its review and told again to a launch that continues its session")
		i.steers.settleUnanswered(ctx, i.ident.turn.TurnID, read.Take)
		return nil
	}
	notes := notesTaken(candidates, taken)
	i.delivered(ctx, taken, notes)
	return notes
}

// delivered publishes the notes a take recorded read as delivered there, off
// the path that read them, under ids derived from the turn and each note — under
// ctx stripped of its cancellation, because the take that read them is done and
// the hook it answered may have gone: the read is on the record, and its record
// is owed regardless.
func (i *runInbox) delivered(ctx context.Context, taken []sandbox.RunNote, notes []steer.Note) {
	if len(notes) == 0 {
		return
	}
	base := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(base, steerPublishBudget)
		defer cancel()
		for k, note := range notes {
			read := readingOf(*taken[k].Read)
			id := uuid.NewSHA1(steerDeliveredNamespace,
				[]byte(i.ident.turn.TurnID+"/"+note.ID+"/delivered"))
			if err := i.steers.publish(ctx, i.ident, note, &read, id, read.At); err != nil {
				log.ErrorContext(ctx, "steer_delivery_unrecorded", "turn_id", i.ident.turn.TurnID,
					"note_id", note.ID, "boundary", string(read.Boundary), "error", err.Error(),
					"detail", "the run read the note, and no record says so")
			}
		}
	}()
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
	base := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(base, steerPublishBudget)
		defer cancel()
		if _, err := i.steers.store.MarkPulled(ctx, i.ident.turn.TurnID, launch); err != nil {
			log.WarnContext(ctx, "steer_pull_unrecorded", "turn_id", i.ident.turn.TurnID,
				"error", err.Error())
			i.mu.Lock()
			i.pulled = false
			i.mu.Unlock()
		}
	}()
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
	var unserved []string
	for _, o := range opened {
		if o.held.Seq <= after {
			continue
		}
		due = append(due, o)
		if o.held.Served != launch {
			unserved = append(unserved, o.held.ID)
		}
	}
	served := map[string]bool{}
	if len(unserved) > 0 {
		marked, err := i.steers.store.ServeNotes(ctx, i.ident.turn.TurnID, sandbox.NoteServe{
			IDs: unserved, Launch: launch, Statuses: steerTakeStatuses})
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
	for _, o := range due {
		// UP TO THE FIRST NOTE NOT MARKED, never past it: the poller's cursor
		// moves past every note it is answered, and one past an unmarked note
		// would never ask for that note again.
		if o.held.Served != launch && !served[o.held.ID] {
			break
		}
		out = append(out, mcpbridge.SteerNote{
			Seq: o.held.Seq, ID: o.held.ID, Inbox: executor.OpenCodeInboxID(launch, o.held.ID),
			Message: prompts.SteerMessage(o.note.By, o.note.Text),
		})
	}
	return out, changed
}

// Read records the notes named read at the run's next step — see
// [mcpbridge.SteerInbox].
func (i *runInbox) Read(ctx context.Context, ids []string) error {
	launch, watch := i.bound()
	if launch == "" || len(ids) == 0 {
		return nil
	}
	_, opened, _ := watch.snapshot()
	var candidates []openedNote
	for _, o := range opened {
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
		for _, o := range i.steers.waiting(run.TurnID, run.Steer.Notes) {
			if slices.Contains(ids, o.held.ID) {
				candidates = append(candidates, o)
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	idsOf := make([]string, 0, len(candidates))
	for _, c := range candidates {
		idsOf = append(idsOf, c.held.ID)
	}
	taken, err := i.steers.store.TakeNotes(ctx, i.ident.turn.TurnID, sandbox.NoteTake{
		IDs: idsOf, Launch: launch, Statuses: steerTakeStatuses,
		Read: sandbox.NoteRead{Phase: string(types.PhaseExecute), Iteration: i.round, Round: 1,
			Boundary: string(types.SteerBoundaryStep), Launch: launch},
	})
	if err != nil {
		return err
	}
	i.delivered(ctx, taken, notesTaken(candidates, taken))
	return nil
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

// update takes a new state of the record into the view.
func (w *noteWatch) update(view sandbox.NotesView) {
	opened := w.steers.waiting(w.turnID, view.Steer.Notes)
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
