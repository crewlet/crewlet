package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
)

// THE LIVE TURNS' HOLDS: what keeps a seat's scratch repository from the code
// host's expiry while a turn that has named it is UNENDED — still running, or
// detached with a run row (M3-decisions M3-O61).
//
// # One hold, kept where the turn lives
//
// A turn's holds are [turnctx.Holds], taken before every request it makes
// about a repository (internal/agent/builtin's one seam). While the turn RUNS
// they live in this process's memory — the turn lives nowhere else, and a
// durable copy would need a rule to release the hold of a turn whose process
// crashed — and this file keeps every running turn's set by turn
// ([runningTurns]), registered where a turn starts (the dispatch and the
// resume) and forgotten where it returns. Once the turn has a RUN ROW the set
// is on the row too ([sandbox.PendingRun.CodeHolds]): a launch writes the
// turn's set on the row it opens and attaches the writer every later hold
// goes through first ([holdWriter]), and a resumed turn starts from the row's
// set with that writer attached ([Engine.rowHolds]) — so a parked run holds
// what its turn named for as long as its row exists, which is until it ends.
//
// # One reader, asked once a page of due repositories
//
// [Engine.codeHolds] is the code-retention pass's [code.Retention.Holds]: the
// keys of a page held by (a) any run row the store holds, WHATEVER ITS STATUS
// — a status a newer build added included ([sandbox.PendingStore.ListRuns]),
// since a run is unended exactly while it has a record — and (b) any running
// turn of any process holding a seat lease, asked over the queue's ephemeral
// verbs on [topics.CodeHolds]; and the rows are read AGAIN after the ask,
// because a turn's first launch moves its holds from its memory onto a new
// row while the ask is on its way ([holdsReader.holds]). A turn runs only
// under its seat's lease, and seat leases are released after a drain's turns
// finish, while the presence lease is given back first, so the seat holders
// are who must answer: a presence listing would miss a draining node's
// running turns. A node that holds no seat lease is stopping its turns,
// whatever such a turn launched is a row, and the rest of its work is redone
// under a new turn id by the seat's next holder.
//
// THE ASK ENDS WHEN EVERY HOLDER THE LEASES NAME HAS ANSWERED, never at a
// count of replies: every engine answers the question, whatever its roles
// ([Engine.armCodeHolds]), so a node holding no seat lease — a data, git or
// ingress node without seats, a node a fleet with fewer seats than nodes left
// none, a draining one — answers it too, and a count as long as the holders'
// list was filled by such answers while a holder's went unread.
//
// NEVER ON AN UNKNOWN: a run store that cannot be listed, seat leases that
// cannot be read, or a seat holder that does not answer inside
// [holdsAskBudget] is an error naming what was not read, and the pass expires
// nothing that page — a holder silent past the budget is unknown, never
// "holds nothing".
//
// AN UNSIGNED READ SCATTER, internal/signedask's rule for a question a node
// answers about itself with nothing carried out: like the fleet's history and
// a held seat's memory, it trusts the fleet's broker to be the fleet's, which
// is the operator's half of the rule (SECURITY.md) — and a forged answer that
// claims a hold only keeps a repository longer.

// holdsAskBudget bounds the ask of the seat holders: eventfan.FleetReadBudget,
// two seconds, the bound every fleet read is held to, borrowed for its reason
// rather than chosen (ADR-0015's rule, kept for a wait): an answer is a set
// lookup in each holder's memory one round trip away, so a holder that has not
// answered inside it is one worth naming rather than waiting for — and its
// silence costs the pass only that page's expiry, which the next pass asks
// again.
const holdsAskBudget = eventfan.FleetReadBudget

// codeHoldsProtocol is the version of a holds question and its answer. An
// answerer refuses a question at a version it does not speak — answering
// nothing, which its asker reads as a holder that did not answer, so the page
// expires nothing — and an asker drops an answer at a version it does not.
const codeHoldsProtocol = 1

// codeHoldsQuestion is what the reader scatters on topics.CodeHolds: a page's
// keys, at most the page the pass reads (code's retention page, 256 scratch
// repository ids).
type codeHoldsQuestion struct {
	Version int      `json:"version"`
	Keys    []string `json:"keys"`
}

// codeHoldsAnswer is one seat holder's answer: the lease identity it holds its
// seats under, and the keys of the question its running turns hold.
type codeHoldsAnswer struct {
	Version int      `json:"version"`
	Owner   string   `json:"owner"`
	Held    []string `json:"held,omitempty"`
}

// errCodeHoldsQuestion is a question this node cannot read: not JSON, or at a
// version it does not speak. Answered with nothing.
var errCodeHoldsQuestion = errors.New("engine: a code holds question this node cannot read")

// errRunHoldsNothing is a hold a turn's run row refused: the row is gone, or
// its ending is decided. The request the hold was taken for is refused, with
// nothing sent.
var errRunHoldsNothing = errors.New("engine: the run's row is gone or its ending is decided, so it " +
	"holds nothing new; the request was not sent")

// runningTurns is this process's running turns' holds, each set counted by
// how many of its turns' frames run now — the dispatch and the resume of one
// run never overlap, but a set registered twice is forgotten only by its last
// frame's return. Its zero value is ready, and it is safe for concurrent use.
type runningTurns struct {
	mu    sync.Mutex
	turns map[*turnctx.Holds]int
}

// running registers h as a running turn's and answers what forgets it, which
// the turn's frame calls as it returns. A nil set holds nothing and registers
// nothing.
func (r *runningTurns) running(h *turnctx.Holds) (done func()) {
	if h == nil {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turns == nil {
		r.turns = map[*turnctx.Holds]int{}
	}
	r.turns[h]++
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.turns[h]--; r.turns[h] <= 0 {
				delete(r.turns, h)
			}
		})
	}
}

// held is the keys among keys some running turn holds, in keys' order.
func (r *runningTurns) held(keys []string) []string {
	r.mu.Lock()
	sets := make([]*turnctx.Holds, 0, len(r.turns))
	for h := range r.turns {
		sets = append(sets, h)
	}
	r.mu.Unlock()
	holding := map[string]bool{}
	for _, h := range sets {
		for _, key := range h.IDs() {
			holding[key] = true
		}
	}
	var out []string
	for _, key := range keys {
		if holding[key] {
			out = append(out, key)
		}
	}
	return out
}

// holdWriter is the writer a turn's holds are attached to once the turn has a
// run row: every new key is written on the row ([sandbox.PendingStore.AddCodeHolds])
// BEFORE the request it was taken for is sent, and a row that is gone or
// ending refuses it ([errRunHoldsNothing]).
func holdWriter(pending sandbox.PendingStore, turnID string) func(context.Context, []string) error {
	return func(ctx context.Context, keys []string) error {
		held, err := pending.AddCodeHolds(ctx, turnID, keys)
		switch {
		case err != nil:
			return fmt.Errorf("engine: hold %s on run %s: %w", words.And(keys), turnID, err)
		case !held:
			return fmt.Errorf("%w (run %s)", errRunHoldsNothing, turnID)
		}
		return nil
	}
}

// rowHolds is the holds of a turn the engine builds from a run row — a resumed
// one: what the row holds, with the row's writer attached, so its every new
// hold is on the row before its request. A node with no run store keeps them
// in memory only, where the registry answers for them while the turn runs.
func (e *Engine) rowHolds(run sandbox.PendingRun) *turnctx.Holds {
	h := turnctx.NewHolds(run.CodeHolds)
	if rt := e.sandbox.Load(); rt != nil && rt.pending != nil {
		h.Attach(holdWriter(rt.pending, run.TurnID))
	}
	return h
}

// armCodeHolds makes this node the answerer for its running turns' holds, on
// every node — each builds a seat host, whatever its roles, so one holding no
// seat answers too, which is why the reader waits for the holders by name —
// and in every mode: answering is not publishing. BEFORE ITS SEAT HOST CLAIMS
// ITS FIRST SEAT — after the node, whose incarnation is what its seat leases
// name, and before Start runs the host — so no seat holder is ever unable to
// answer. A node with no broker is the fleet, and answers its own reader
// directly ([holdsReader.running]).
func (e *Engine) armCodeHolds(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil || e.node == nil {
		return nil
	}
	stop, err := e.backends.Queue.Serve(ctx, topics.CodeHolds, queue.ConcurrentAnswers,
		answerCodeHolds(e.node.Owner(), &e.turnHolds))
	if err != nil {
		return fmt.Errorf("engine: serve the running turns' code holds: %w", err)
	}
	e.stopCodeHoldsServe = stop
	return nil
}

// stopCodeHolds withdraws this node as the answerer for its turns' holds —
// AFTER its seat host has released its seats ([Engine.Stop]): until then a
// pass reads this node among the seat holders, and a holder that stops
// answering first costs that pass its expiry.
func (e *Engine) stopCodeHolds(ctx context.Context) {
	if e.stopCodeHoldsServe == nil {
		return
	}
	if err := e.stopCodeHoldsServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "code_holds_answerer_not_withdrawn", "error", err)
	}
	e.stopCodeHoldsServe = nil
}

// answerCodeHolds answers one question: the keys of it the running turns hold,
// under owner, the identity this process holds its seat leases under. An
// IN-MEMORY answer, so it runs at queue.ConcurrentAnswers.
func answerCodeHolds(owner string, turns *runningTurns) queue.AnswerFunc {
	return func(_ context.Context, raw []byte) ([]byte, error) {
		var q codeHoldsQuestion
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("%w: %w", errCodeHoldsQuestion, err)
		}
		if q.Version != codeHoldsProtocol {
			return nil, fmt.Errorf("%w: version %d, and this build speaks %d", errCodeHoldsQuestion,
				q.Version, codeHoldsProtocol)
		}
		return json.Marshal(codeHoldsAnswer{Version: codeHoldsProtocol, Owner: owner, Held: turns.held(q.Keys)})
	}
}

// codeHolds is the code-retention pass's [code.Retention.Holds]: which of keys
// a live turn holds — see the file's doc.
func (e *Engine) codeHolds(ctx context.Context, keys []string) (map[string]bool, error) {
	return e.holdsReader().holds(ctx, keys)
}

// holdsReader is the reader over this node's backends: the fleet's run store,
// the seat leases, the broker's ask and this process's own running turns.
func (e *Engine) holdsReader() holdsReader {
	r := holdsReader{self: &e.turnHolds, budget: holdsAskBudget}
	if e.backends == nil {
		return r
	}
	if e.backends.Fleet != nil {
		r.runs = sandbox.NewCoordStore(e.backends.Fleet)
	}
	if e.backends.Coord != nil {
		r.leases = e.backends.Coord
	}
	if e.backends.Queue != nil {
		r.askEach = e.backends.Queue.AskEach
	}
	return r
}

// holdsReader is what the live turns' holds are read from, each a seam this
// file declares for what it needs — so a case drives a store that cannot list,
// a lease listing that fails and a holder that stays silent, each alone.
type holdsReader struct {
	// runs lists every run row, whatever its status.
	runs interface {
		ListRuns(ctx context.Context) ([]sandbox.PendingRun, error)
	}
	// leases lists the live leases of a class: the seat leases, whose owners
	// are who must answer.
	leases interface {
		ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
	}
	// askEach scatters a question and hands each reply to each as it arrives
	// (queue's AskEach), until ctx is done or each answers false — the shape
	// that lets the reader stop on WHO has answered rather than on how many;
	// nil is a node with no broker, which is the whole fleet and answers from
	// self.
	askEach func(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error
	// self is this process's own running turns.
	self   *runningTurns
	budget time.Duration
}

// holds answers which of keys a live turn holds, or an error naming what it
// could not read — never an answer on an unknown.
//
// THE ROWS ARE READ ON BOTH SIDES OF THE ASK, and a key any of the three
// reads shows is held, because a turn's holds MOVE between the two places it
// keeps them, and each move must land inside some read:
//
//   - A FIRST LAUNCH moves them from the turn's memory onto a NEW row: the
//     launch writes the sealed set on the row it opens (BeginLaunch), the
//     turn suspends, and its frame returns and forgets the set
//     ([runningTurns]). A row written after (a) listed the store, by a turn
//     whose frame returned before (b)'s question reached its node, is in
//     neither answer — and between the two reads the pass waits on the
//     seat-lease listing, a certified listing of the coordination store
//     whose p99 on a loaded fleet reached seconds, against a move that takes
//     two coordination writes and a box's start. The row was written before
//     the frame returned, so before (b) missed it: (c), which starts after
//     (b) ends, lists it — unless the run has ENDED and deleted it, when
//     nothing holds the repository any more.
//   - A RESUME moves them the other way, from the row into the resumed
//     turn's memory, while the row stays until the run ends: (a) lists it.
//
// One more listing of the run store a page of DUE repositories — the bucket
// every completion poll reads whole — is what closing the first move costs.
func (r holdsReader) holds(ctx context.Context, keys []string) (map[string]bool, error) {
	asked := make(map[string]bool, len(keys))
	for _, key := range keys {
		asked[key] = true
	}
	held := map[string]bool{}
	if len(keys) == 0 {
		return held, nil
	}
	if r.runs == nil {
		return nil, errors.New("engine: this node has no run store to read the parked runs' holds from")
	}
	// rows marks what every run row, WHATEVER ITS STATUS, holds: a parked
	// run holds what its turn named for as long as it has a record.
	rows := func(when string) error {
		runs, err := r.runs.ListRuns(ctx)
		if err != nil {
			return fmt.Errorf("engine: the run store could not be listed %s for the parked runs' holds: %w",
				when, err)
		}
		for _, run := range runs {
			for _, key := range run.CodeHolds {
				if asked[key] {
					held[key] = true
				}
			}
		}
		return nil
	}
	// (a) THE ROWS, before the running turns are asked.
	if err := rows("before the running turns were asked"); err != nil {
		return nil, err
	}
	// (b) EVERY RUNNING TURN, asked of every process holding a seat lease.
	running, err := r.running(ctx, keys)
	if err != nil {
		return nil, err
	}
	for _, key := range running {
		if asked[key] {
			held[key] = true
		}
	}
	// (c) THE ROWS AGAIN, after: a first launch's holds, moved onto a new row
	// while (b) was on its way.
	if err := rows("after the running turns were asked"); err != nil {
		return nil, err
	}
	return held, nil
}

// running is the keys among keys some running turn of the fleet holds: every
// seat holder the leases name asked, and every one of them required to answer
// inside the budget.
//
// THE ASK ENDS ON WHO HAS ANSWERED, NOT ON HOW MANY: it lasts until every
// holder the leases name has, or the budget ends. Every engine answers the
// question, a node holding no seat lease among them (the file's doc), and a
// scatter's replies carry no sender, so an ask told to stop at as many
// replies as there are holders stopped at the first that many answers to
// arrive — where a node holding no seat answered first, a holder's answer was
// never read, and the read failed naming a holder that had answered at once.
func (r holdsReader) running(ctx context.Context, keys []string) ([]string, error) {
	if r.askEach == nil {
		// A NODE WITH NO BROKER IS THE FLEET: its own turns are every turn.
		return r.self.held(keys), nil
	}
	if r.leases == nil {
		return nil, errors.New("engine: this node has no coordination store to read the seat holders from")
	}
	leases, err := r.leases.ListLive(ctx, coord.ClassSeat)
	if err != nil {
		return nil, fmt.Errorf("engine: the seat leases could not be read for the running turns' holds: %w", err)
	}
	// answered is every holder the leases name, and whether it has; unheard
	// counts the ones that have not.
	answered := map[string]bool{}
	for _, l := range leases {
		answered[l.Owner] = false
	}
	unheard := len(answered)
	if unheard == 0 {
		// NO SEAT IS HELD, so no turn runs anywhere.
		return nil, nil
	}
	question, err := json.Marshal(codeHoldsQuestion{Version: codeHoldsProtocol, Keys: keys})
	if err != nil {
		return nil, fmt.Errorf("engine: encode a code holds question: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()
	var held []string
	err = r.askEach(askCtx, topics.CodeHolds, question, func(raw []byte) bool {
		var a codeHoldsAnswer
		if err := json.Unmarshal(raw, &a); err != nil || a.Version != codeHoldsProtocol || a.Owner == "" {
			// AN ANSWER THIS BUILD CANNOT READ names nobody it could count
			// as answered; its holder, if the leases name it, stays unheard.
			return true
		}
		// A HOLDER THE LISTING DID NOT NAME — one that took a seat since, or
		// a node holding none whose turns are stopping — is believed too:
		// what it holds can only keep a repository. It ends nothing.
		held = append(held, a.Held...)
		if heard, named := answered[a.Owner]; named && !heard {
			answered[a.Owner] = true
			unheard--
		}
		return unheard > 0
	})
	if err != nil {
		return nil, fmt.Errorf("engine: ask the seat holders for their running turns' holds: %w", err)
	}
	if unheard > 0 {
		var silent []string
		for owner, heard := range answered {
			if !heard {
				silent = append(silent, owner)
			}
		}
		slices.Sort(silent)
		return nil, unheardHolders(ctx, askCtx, silent, r.budget)
	}
	return held, nil
}

// unheardHolders is the error of an ask that ended before the seat holders
// silent answered, naming only them and saying how it ended: the pass's own
// context done, the budget run out — the only end that is a silence of the
// budget's length — or the transport delivering nothing more before either,
// for which it claims no wait the read did not make.
func unheardHolders(ctx, askCtx context.Context, silent []string, budget time.Duration) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("engine: the read of the running turns' holds ended before the seat holders %s "+
			"answered: %w", words.And(silent), ctx.Err())
	case askCtx.Err() != nil:
		return fmt.Errorf("engine: the seat holders %s did not answer inside %s for their running turns' "+
			"holds", words.And(silent), budget)
	}
	return fmt.Errorf("engine: the seat holders %s did not answer for their running turns' holds: the ask "+
		"ended before %s with no answer from them", words.And(silent), budget)
}
