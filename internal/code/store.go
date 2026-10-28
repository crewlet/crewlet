package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LIFECYCLE STORE: the writes a repository's life is made of, other than
// its pushes — created, settled and restored by the configuration's reconcile,
// deleted when it leaves code.repos, purged by the code-retention pass — plus
// a node's eviction from this log.
//
// Every write is ONE record through the domain's publisher, decided inside the
// publisher's snapshot from rows read there, under a FRESH operation id per
// call: a reconcile decides from rows, so the next call finds its own value
// already there and writes nothing. Every decide resets its out-variables at
// the top of every round, because a round that decided to write and lost the
// broker's arbitration is followed by one that finds the winner's value, and
// only the last round says what the call did.

// Publisher is the domain's write authority as this package calls it;
// *statelog.Publisher satisfies it.
type Publisher interface {
	Publish(ctx context.Context, req statelog.Request) (statelog.Result, error)
}

// StoreOptions configure a store.
type StoreOptions struct {
	// Publisher is the code log's write authority.
	Publisher Publisher
	// DB is the replicated estate a decision is read from (inside the
	// publisher's snapshot) and a write's outcome read back from.
	DB store.ReplicatedReader
	// NodeID is the deciding node: the actor a configuration or system
	// record names.
	NodeID string
	// Now is the clock authored instants (a record's CreatedAt) and the
	// retention judgement read; nil takes the wall clock in UTC.
	Now func() time.Time

	// THE THREE A TOOL'S LIFECYCLE REQUEST IS DECIDED BY ([Store.Lifecycle]),
	// none of them required: the code log's eviction gate builds a store that
	// decides only evictions, and a store missing SeatRules or Policy answers
	// every lifecycle request [ErrNotServing] — "this node's code store
	// decides no lifecycle request", so the router asks the next data node —
	// and never judges one by a nil chart.

	// SeatRules is the DECIDING node's epoch's chart (the committer's own,
	// [CommitterOptions.SeatRules]), read once a request: who may ask what
	// is judged by it — a seat's grant, a create's owner — and false (no
	// epoch installed yet) answers every request [ErrNotServing].
	SeatRules func() (SeatRules, bool)
	// Current is whether this node serves the company's configuration
	// (Reconciler.Admits): false — confirmed behind it, shed or stuck —
	// answers EVERY request [ErrNotServing], since every kind's "who may"
	// reads the chart, a person's create included. Nil is current.
	Current func() bool
	// Policy is the epoch's code.scratch and the repository bound
	// ([ScratchOf]), read once a create.
	Policy func() LifecyclePolicy
}

// Store is the code host's lifecycle write path.
type Store struct {
	publisher Publisher
	db        store.ReplicatedReader
	nodeID    string
	now       func() time.Time

	seatRules func() (SeatRules, bool)
	current   func() bool
	policy    func() LifecyclePolicy
}

// NewStore builds the lifecycle store, refusing what it cannot work without.
func NewStore(o StoreOptions) (*Store, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("code: a store needs the code log's write authority — " +
			"every write here is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("code: a store needs the replicated estate a write " +
			"decides from")
	case strings.TrimSpace(o.NodeID) == "":
		return nil, errors.New("code: a store needs the deciding node's id, which a " +
			"configuration or system record names as its actor")
	}
	s := &Store{publisher: o.Publisher, db: o.DB, nodeID: o.NodeID, now: o.Now, seatRules: o.SeatRules,
		current: o.Current, policy: o.Policy}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// Activation is one configuration activation, as this node has fenced the
// code log with it: what [Store.EnsureRepo] and [Store.DeleteRepo] take, so
// no reconcile can decide a repository without having raised the fence first.
// Only [Store.Activate] makes one.
type Activation struct {
	// epoch is the activation (configplane.ActivationStamp).
	epoch int64

	// at is the position of the fence record this node's own append landed
	// at, or zero when the fence was already at this activation in the
	// snapshot Activate decided from. Every later decide waits for this node
	// to apply it first (the request's session floor), which is what puts
	// every decision of this activation after its fence.
	at statelog.Position
}

// Epoch is the activation's stamp.
func (a Activation) Epoch() int64 { return a.epoch }

// Activate raises the code log's ACTIVATION FENCE to the configuration
// activated at activatedAt — the first write of every reconcile of code.repos,
// before any repository is decided.
//
// ONE RECORD AN ACTIVATION, fleet-wide: every data node's reconcile asks, the
// company subject arbitrates them, and a node whose snapshot already holds the
// fence at this activation writes nothing. An activation OLDER than the fence
// is [ErrSuperseded]: a later configuration owns the declared repositories,
// and every decision this one made would be refused on every node.
func (s *Store) Activate(ctx context.Context, activatedAt time.Time) (Activation, error) {
	if activatedAt.IsZero() {
		return Activation{}, ErrNoActivation
	}
	epoch := configplane.ActivationStamp(activatedAt)
	at := s.now()
	opID := statelog.NewOpID(time.Now(), "code.activate")
	subject := CompanySubject()
	scope := []string{ActivationPath()}
	var superseded bool
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			superseded = false
			fence, err := fenceIn(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			switch {
			case fence > epoch:
				superseded = true
				return statelog.Decision{}, nil
			case fence == epoch:
				// ALREADY FENCED AT THIS ACTIVATION, by this node's
				// earlier call or a peer's: the snapshot holds it, so
				// every later decide here is after it.
				return statelog.Decision{}, nil
			}
			return recordFor(stamp, subject, OpActivate, scope, opID,
				Actor{ID: s.nodeID, Kind: ActorConfig}, Activated{ConfigEpoch: epoch}, at)
		},
	})
	if err != nil {
		return Activation{}, fmt.Errorf("code: raise the activation fence to %d: %w", epoch, err)
	}
	if superseded {
		return Activation{}, fmt.Errorf("%w (activation %d)", ErrSuperseded, epoch)
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return Activation{}, unknownOutcome(ctx, "code: whether the activation fence reached %d "+
			"is unknown (operation %s); the next attempt decides it again", epoch, opID)
	}
	return Activation{epoch: epoch, at: result.Position}, nil
}

// Declared is one repository code.repos declares.
type Declared struct {
	Name string
	// Settings are what the declaration says. ManagedBy must be
	// ManagedConfig; ConfigEpoch is ignored — EnsureRepo sets the
	// activation's.
	Settings RepoSettings
}

// Ensured is what [Store.EnsureRepo] did.
type Ensured struct {
	RepoID string
	// Changed reports a record THIS CALL wrote: a create, a settings change
	// or a restore.
	Changed bool
	// Status is the status the repository has once that record applies, or
	// the row's when nothing was written.
	Status RepoStatus
}

// EnsureRepo makes the repository a declaration names exist with the declared
// settings, at the activation a: it creates it, settles a settings change, or
// restores a deleted one.
//
// # One record when something changed, none when nothing did
//
// An unchanged declaration writes NOTHING, whatever activation last stamped
// the row: the activation fence ([Store.Activate]) is what keeps an older
// activation from walking a newer one back, so the row needs no re-stamp to be
// protected, and an activation over ten thousand unchanged repositories costs
// its one fence record rather than ten thousand settings records. A reconcile
// of a whole declaration set takes [Store.EnsureRepos], which also reads the
// unchanged ones once rather than publishing twice for each.
//
// A deleted repository is restored only by an activation LATER than the one
// that deleted it, so a node applying the deleting activation's predecessor
// late cannot bring it back. One being purged answers [ErrPurging]: its name
// returns when the purge completes. A repository its configuration does not
// manage answers [ErrNotConfigManaged].
//
// A record a LATER activation's fence refused is [ErrSuperseded] — decided
// here from a snapshot where a later fence already stood, or read back after
// an applied record that left the repository below this activation.
func (s *Store) EnsureRepo(ctx context.Context, a Activation, d Declared) (Ensured, error) {
	if a.epoch == 0 {
		return Ensured{}, ErrNoActivation
	}
	if err := d.Validate(); err != nil {
		return Ensured{}, err
	}
	settings := d.settingsAt(a)

	created, found, err := s.claim(ctx, a, d.Name, settings)
	if err != nil {
		return Ensured{}, err
	}
	if created != "" {
		return Ensured{RepoID: created, Changed: true, Status: settings.status()}, nil
	}
	return s.settle(ctx, a, found, settings)
}

// Validate refuses a declaration no activation can ensure: a name that is no
// PROJECT's — a declared repository is never a seat's scratch repository,
// whose dotted name only that seat's tool creates — and settings that are no
// configuration's, judged as the reconcile records them (settingsAt): the
// declaration states no kind and no epoch, which the domain stamps, and one
// stating a kind other than a project's is refused.
func (d Declared) Validate() error {
	switch {
	case !gitref.ValidProjectName(d.Name):
		return fmt.Errorf("%w: name: %q is not a project repository's name", ErrInvalid, d.Name)
	case d.Settings.ManagedBy != ManagedConfig:
		return fmt.Errorf("%w: settings.managed_by: a declared repository is "+
			"managed by %s, not %q", ErrInvalid, ManagedConfig, d.Settings.ManagedBy)
	case d.Settings.Kind != "" && d.Settings.Kind != RepoProject:
		return fmt.Errorf("%w: settings.kind: a declared repository is a %s, not %q", ErrInvalid,
			RepoProject, d.Settings.Kind)
	}
	return d.settingsAt(Activation{}).Validate()
}

// settingsAt is what the declaration says, stamped with the activation a — THE
// ONE PLACE every settings a reconcile records is formed, so every one is a
// PROJECT's: a configuration declares nothing else.
func (d Declared) settingsAt(a Activation) RepoSettings {
	settings := d.Settings
	settings.Kind = RepoProject
	settings.ConfigEpoch = a.epoch
	return settings
}

// EnsureResult is one declaration's answer from [Store.EnsureRepos]: what
// [Store.EnsureRepo] answers for it, error included.
type EnsureResult struct {
	Ensured
	Err error
}

// EnsureRepos is [Store.EnsureRepo] over every repository a declaration set
// names, at the activation a — the reconcile's form — and answers, in the
// order given, what EnsureRepo answers for each.
//
// # One read for what has not changed
//
// An activation of an unchanged configuration changes nothing, and EnsureRepo
// spends two publishes a repository finding that out — each one its own
// eviction read, session wait and snapshot of the deferred scope, the anchor,
// the ledger and the rows — measured at 1.7–2.0 ms a repository: seventeen to
// twenty seconds of reads on every data node that runs the reconcile, at the
// ten thousand repositories a configuration may declare, for an activation
// whose log cost is one fence record. Read once, the ten thousand take
// 0.47–0.59 s (doc.go's measured section). So the set is read ONCE
// ([Store.unchanged]): one publish on the fence's own subject, scoped to the
// whole domain, whose decide reads every declared name's claim, its
// repository's row and its rules in ONE transaction and decides nothing. A
// declaration whose repository is live and declared as it is, or deleted at
// this activation or a later one, is answered from that read; every other — a
// name nobody holds, settings that differ, a restore, a purge, a repository
// the configuration does not manage or a later activation wrote — goes
// through EnsureRepo and is decided on its own subjects as before.
//
// THE SAME VERDICT AN EMPTY DECISION GIVES, because it is one: the batch
// publish passes the fences, the session wait and the deferred-scope probe
// every per-repository publish passes, and the probe over the whole domain is
// at least as strict as one over a repository's paths — a record this node
// cannot decode anywhere on the code log refuses the batch, and every
// declaration then goes through EnsureRepo, where each is refused or decided
// on its own scope.
//
// # What changed is decided side by side
//
// The declarations EnsureRepo decides are decided [reconcileInFlight] at a
// time ([decideEach]), because each is its own subjects — a name's claim and
// its repository's — so no two contend, and because one at a time a changed
// activation was bound by the round trip of one write: a create is a publish,
// an append and a wait for this node to apply it, 6.7 ms each, so the last of
// the ten thousand repositories a document may declare existed a minute and
// more after the first activation that declared them. In flight together,
// their records reach the applier together, which commits them as one batch
// rather than one transaction a record: 16–19 s (doc.go's measured section).
//
// # One answer for what every declaration shares
//
// The deferred refusal is the only one a narrower scope can answer
// differently. Any other the batch meets — the node's own fences (its
// eviction, the log's identity), the activation's session floor not reached
// in the budget, a later activation's fence, the estate or the context
// failing — every per-repository publish meets too, each after a wait of its
// own: on a node whose applier stalled below the fence, ten thousand
// declarations would each wait out the write budget
// ([statelog.DefaultResolveBudget], five seconds) — fourteen hours — for the
// refusal the first publish already had. And a failure of that kind met while
// the changed declarations are decided ([sharedFailure]: a later fence, this
// node's own refusal, the context) is one every declaration after it would
// meet: the decisions in flight are ended and no more are begun.
//
// So such a failure is the CALL's error, and it is also the answer of every
// declaration it left undecided or ENDED — one in flight that failed once the
// run had ended, whatever its own error says, since an append its context
// ended mid-flight is answered unknown and says nothing of the context
// ([decideEach]) — the same error value, so a caller that reports the call's
// failure once can pass over each of them. Each declaration decided before it
// keeps its own answer, a declaration that is not one ([ErrInvalid]) its own
// refusal; and the call never returns with a decision still in flight. An
// activation that is none ([ErrNoActivation]) answers nothing.
func (s *Store) EnsureRepos(ctx context.Context, a Activation, declared []Declared) ([]EnsureResult, error) {
	if a.epoch == 0 {
		return nil, ErrNoActivation
	}
	out := make([]EnsureResult, len(declared))
	valid := make([]Declared, 0, len(declared))
	for i, d := range declared {
		if out[i].Err = d.Validate(); out[i].Err == nil {
			valid = append(valid, d)
		}
	}
	unchanged, err := s.unchanged(ctx, a, valid)
	var refused *statelog.Unavailable
	switch {
	case err == nil:
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonDeferred:
		// THE ONE REFUSAL A NARROWER SCOPE ANSWERS DIFFERENTLY: the
		// batch's probe covers the whole domain and a repository's only
		// its own paths, so every declaration is decided on its own and
		// is refused there only where the record covers it.
		unchanged = nil
	default:
		// EVERY OTHER FAILURE IS EVERY DECLARATION'S ANSWER, because each
		// per-repository publish would meet it too. The node's fences and
		// the session floor are the activation's; the only record the
		// batch's own subject can be behind is a LATER activation's fence
		// (one fence an activation, rising), which every decide of this
		// one must see before it decides; an applied later fence
		// supersedes every one of them; an estate that cannot be read
		// cannot be read for any of them; and a context that ended ends
		// them all. Sent through EnsureRepo, each would wait out the same
		// budget for the same answer — hours, at ten thousand
		// declarations on a node whose applier stalled.
		for i := range out {
			if out[i].Err == nil {
				out[i].Err = err
			}
		}
		return out, err
	}
	var changed []int
	for i, d := range declared {
		if out[i].Err != nil {
			continue
		}
		if e, ok := unchanged[d.Name]; ok {
			out[i].Ensured = e
			continue
		}
		changed = append(changed, i)
	}
	ended, err := decideEach(ctx, len(changed), func(ctx context.Context, k int) error {
		i := changed[k]
		out[i].Ensured, out[i].Err = s.EnsureRepo(ctx, a, declared[i])
		return out[i].Err
	})
	for k, i := range changed {
		if ended[k] {
			out[i] = EnsureResult{Err: err}
		}
	}
	return out, err
}

// DeleteResult is one repository's answer from [Store.DeleteRepos]: what
// [Store.DeleteRepo] answers for it.
type DeleteResult struct {
	// Deleted reports a record THIS CALL wrote.
	Deleted bool
	Err     error
}

// DeleteRepos is [Store.DeleteRepo] over every repository ids names, at the
// activation a — the reconcile's form, for the configuration-managed
// repositories a document no longer declares — and answers, in the order
// given, what DeleteRepo answers for each.
//
// SIDE BY SIDE, [reconcileInFlight] at a time, for [Store.EnsureRepos]'
// reason: each repository is its own subject, so no two contend, and one at
// a time the removal of ten thousand repositories spent 7 ms a repository, a
// minute and more before the last was deleted.
//
// A failure every repository after it would meet ([sharedFailure]) ends the
// call as it ends EnsureRepos': it is the call's error and the answer of
// every repository it left undecided or ended, the same value, and nothing is
// in flight when the call returns.
func (s *Store) DeleteRepos(ctx context.Context, a Activation, ids []string) ([]DeleteResult, error) {
	if a.epoch == 0 {
		return nil, ErrNoActivation
	}
	out := make([]DeleteResult, len(ids))
	ended, err := decideEach(ctx, len(ids), func(ctx context.Context, k int) error {
		out[k].Deleted, out[k].Err = s.DeleteRepo(ctx, a, ids[k])
		return out[k].Err
	})
	for k := range out {
		if ended[k] {
			out[k] = DeleteResult{Err: err}
		}
	}
	return out, err
}

// reconcileInFlight is how many repositories a reconcile decides at once
// ([Store.EnsureRepos], [Store.DeleteRepos]).
//
// ENOUGH TO HIDE A DECISION'S ROUND TRIP, by Little's law: in flight = the
// rate the node can apply them × how long each takes from its publish to its
// apply. On one node (doc.go's measured section) the rate is about 550 a
// second whatever the width — this node's applier and the snapshots of the
// decisions themselves are the floor — and a decision's round trip alone is
// 6.7 ms, so under four in flight reach it: 8 and 128 decide ten thousand in
// the same 16–21 s, where 4 takes 23–25 s, 2 takes 34–35 s, 1 takes 67–70 s.
// In a fleet each append also waits for its stream's quorum, so the round
// trip grows by the members' round trips while the rate does not; THIRTY-TWO
// keeps the floor's rate up to a round trip of about 58 ms.
//
// AND NO WIDER, because the decisions in flight append together and the
// applier commits them as one batch: the widest record a reconcile writes — a
// create of a repository with [MaxProtectionsPerRepo] rules, its claim, its
// row and 64 rule rows — is 66 rows, so thirty-two of them are 2 112 rows,
// inside the applier's row budget (statelog.ApplyTxRowBudget, 4 000), where
// sixty-four would cross it and split the batch.
const reconcileInFlight = 32

// decideEach runs decide for every k in [0, n), up to [reconcileInFlight] at
// once, as a CLOSED LOOP — a decision begins only when one in flight has
// ended — and answers the run's ENDING: the failure that ended it, and for
// each k whether that failure is decision k's answer.
//
// A run ends at the first [sharedFailure] a decision meets, or when ctx ends
// — and then the failure is ctx's own error, whatever the decision that met
// it said, so a reconcile ended on purpose answers as its context rather than
// as one repository's message. The decisions in flight are cancelled, none is
// begun after it, and decideEach returns only once every decision it began has
// returned — so its caller never leaves one in flight. A run that ends with
// nothing undecided and nothing failed has no ending: nil.
//
// The ending is the answer of every decision it ENDED: each one never begun,
// each one that met it, and each one that FAILED once the run had ended,
// whatever its error says. A failure met on a dead context is the context's
// doing, and it does not always say so: statelog answers an append whose
// context ended mid-flight OutcomeUnknown with NO error, because the probe
// that would classify the cancelled append runs on that same context — so
// read as the decision's own, every decision in flight when a newer
// activation ended a reconcile was reported as a repository it could not
// apply. A decision that SUCCEEDED once the run had ended keeps its answer —
// it decided its repository — and so does a failure met before the end, which
// is its own and ends nothing.
func decideEach(ctx context.Context, n int, decide func(ctx context.Context, k int) error) (ended []bool, err error) {
	ended = make([]bool, n)
	if n == 0 {
		return ended, nil
	}
	run, stop := context.WithCancel(ctx)
	defer stop()
	var (
		mu     sync.Mutex
		next   int
		shared error
		wg     sync.WaitGroup
	)
	// endWith records err as the run's ending, unless one already is — as
	// ctx's own error where ctx has ended — and ends the decisions in flight.
	// Called under mu.
	endWith := func(err error) {
		if shared != nil {
			return
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		shared = err
		stop()
	}
	take := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		if shared != nil || next == n {
			return 0, false
		}
		if err := ctx.Err(); err != nil {
			endWith(err)
			return 0, false
		}
		k := next
		next++
		return k, true
	}
	// settle files decision k's failure err: the ending's where it is one
	// every decision shares, or where the run had already ended when the
	// decision returned (afterEnd) — by an ending recorded, or by ctx before
	// any decision noticed.
	settle := func(k int, err error, afterEnd bool) {
		if err == nil || (!afterEnd && !sharedFailure(err)) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		endWith(err)
		ended[k] = true
	}
	for range min(reconcileInFlight, n) {
		wg.Go(func() {
			for {
				k, ok := take()
				if !ok {
					return
				}
				err := decide(run, k)
				settle(k, err, run.Err() != nil)
			}
		})
	}
	wg.Wait()
	if shared == nil {
		return ended, nil
	}
	for k := next; k < n; k++ {
		ended[k] = true
	}
	return ended, shared
}

// sharedFailure reports whether err, met deciding one repository of a
// reconcile, is one every other repository of it meets too — which ends the
// reconcile ([decideEach]) rather than being reported once a repository:
//
//   - the context ended;
//   - a LATER activation's fence ([ErrSuperseded]), since a fence only rises
//     and every decide reads it first;
//   - a refusal of THIS NODE by the code log — its eviction, a floor it is
//     below or cannot read, a log that is full, skewed, truncated or not the
//     one its rows came from — since every publish this node makes meets it.
//
// Every other failure is the repository's own: a purge it waits for, a
// manager that is not the configuration, a record this node cannot decode
// covering its paths (a narrower scope than the next repository's), a marker
// or a voided record on its subject, a lost race, an outcome the broker did
// not answer for that one append while its context was live — one whose
// context had ended says so ([unknownOutcome]), and is the first case.
func sharedFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrSuperseded) {
		return true
	}
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) {
		return false
	}
	switch refused.Reason {
	case statelog.ReasonEvicted:
		// THIS NODE'S, unless the record the refusal names is another
		// node's copy of the operation, which says nothing about this
		// node (statelog.Unavailable.CopyWriter).
		return refused.CopyWriter == ""
	case statelog.ReasonBelowFloor, statelog.ReasonFloorUnknown, statelog.ReasonLogFull,
		statelog.ReasonSkew, statelog.ReasonLogTruncated, statelog.ReasonWrongStream:
		return true
	}
	return false
}

// unknownOutcome is the error of a write whose outcome this node could not
// establish — the next attempt decides it again — and it WRAPS ctx's end
// where ctx has ended.
//
// statelog answers an append whose context ended while it was in flight
// OutcomeUnknown with NO error: the cancelled append is classified by a probe
// on that same context, which cannot answer either. The unknown is then the
// ending's doing, and a caller that ended the write on purpose — a reconcile a
// newer activation superseded or the engine stopped, a retention pass that
// stopped — must read it as that ending ([sharedFailure], errors.Is) rather
// than as a write of its own that failed.
func unknownOutcome(ctx context.Context, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	if ended := ctx.Err(); ended != nil {
		return fmt.Errorf("%w: %w", err, ended)
	}
	return err
}

// unchanged reads, in ONE publish whose decide writes nothing, which of the
// declarations need no record at the activation a, and answers what
// EnsureRepo would answer for each of them — see [Store.EnsureRepos].
//
// ON THE FENCE'S SUBJECT, after the fence's session floor, because the
// question is one about the activation: the snapshot is the one every
// decision of a reads from, and a fence a later activation raised since is
// [ErrSuperseded] for every declaration at once — every per-repository
// decide reads that same fence before anything of its own, and a fence only
// rises.
func (s *Store) unchanged(ctx context.Context, a Activation, declared []Declared) (map[string]Ensured, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	var (
		found      map[string]Ensured
		superseded bool
	)
	_, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: CompanySubject().statelog(),
		Scope:   statelog.ScopeSet{Paths: []string{DomainPath()}},
		OpID:    statelog.NewOpID(time.Now(), "code.declared"),
		Pattern: statelog.PatternArbitrated, Session: a.at,
		Decide: func(tx *sql.Tx, _ statelog.Stamp) (statelog.Decision, error) {
			found = nil
			var err error
			if superseded, err = activationSuperseded(ctx, tx, a); err != nil || superseded {
				return statelog.Decision{}, err
			}
			found, err = unchangedIn(ctx, tx, a, declared)
			return statelog.Decision{}, err
		},
	})
	switch {
	case err != nil:
		return nil, fmt.Errorf("code: read the declared repositories at activation %d: %w",
			a.epoch, err)
	case superseded:
		return nil, fmt.Errorf("%w (reading the declared repositories at activation %d)",
			ErrSuperseded, a.epoch)
	}
	return found, nil
}

// unchangedIn is [Store.unchanged]'s read, in tx: every declared name's claim,
// the rows of the repositories they name and those repositories' rules — three
// statements, each driven from its list and seeking a primary key once an
// element (planner.go) — and the declarations EnsureRepo's two decides would
// both write nothing for, with what EnsureRepo answers for each.
func unchangedIn(ctx context.Context, tx *sql.Tx, a Activation, declared []Declared) (map[string]Ensured, error) {
	tokens := make([]string, len(declared))
	for i, d := range declared {
		tokens[i] = NameToken(d.Name)
	}
	claims, err := keyPairs(ctx, tx, claimsSQL, tokens)
	if err != nil {
		return nil, fmt.Errorf("code: read the declared names' claims: %w", err)
	}
	ids := make([]string, 0, len(claims))
	for _, id := range claims {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	repos, err := reposIn(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]Ensured{}
	for i, d := range declared {
		id, claimed := claims[tokens[i]]
		row, present := repos[id]
		if !claimed || !present || row.ManagedBy != ManagedConfig || row.ConfigEpoch > a.epoch {
			continue
		}
		switch row.Status {
		case RepoStatusActive, RepoStatusArchived:
			if !row.settings().equal(d.settingsAt(a)) {
				continue
			}
		case RepoStatusDeleted:
			if a.epoch > row.ConfigEpoch {
				continue
			}
		default:
			continue
		}
		out[d.Name] = Ensured{RepoID: id, Status: row.Status}
	}
	return out, nil
}

// keyPairs is every (key, value) a two-column statement over one JSON list of
// keys answers, by key.
func keyPairs(ctx context.Context, tx *sql.Tx, query string, keys []string) (map[string]string, error) {
	list, err := jsonList(keys)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, query, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string, len(keys))
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// reposIn reads the rows and rules of every repository ids names, in two
// statements — the batch form of [repoIn].
func reposIn(ctx context.Context, tx *sql.Tx, ids []string) (map[string]repoRow, error) {
	out := make(map[string]repoRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, reposRowsSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the declared repositories: %w", err)
	}
	for rows.Next() {
		var id string
		var row repoRow
		var status, managed, kind, owners, visibility string
		if err = rows.Scan(&id, &row.Name, &status, &managed, &kind, &owners, &row.Owner.Ref, &visibility,
			&row.DefaultBranch, &row.Description, &row.ConfigEpoch, &row.PurgeParts,
			&row.DeletedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("code: read the declared repositories: %w", err)
		}
		row.Status, row.ManagedBy, row.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
		row.Kind, row.Visibility = RepoKind(kind), Visibility(visibility)
		out[id] = row
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("code: read the declared repositories: %w", err)
	}
	_ = rows.Close()
	// THE RULES, sorted by pattern within each repository — the order a
	// declaration's settings carry them in, which the comparison relies on.
	rules, err := tx.QueryContext(ctx, protectionsOfSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the declared repositories' protections: %w", err)
	}
	defer func() { _ = rules.Close() }()
	for rules.Next() {
		var id, direct string
		var p Protection
		if err := rules.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct); err != nil {
			return nil, fmt.Errorf("code: read the declared repositories' protections: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		if row, ok := out[id]; ok {
			row.Protections = append(row.Protections, p)
			out[id] = row
		}
	}
	if err := rules.Err(); err != nil {
		return nil, fmt.Errorf("code: read the declared repositories' protections: %w", err)
	}
	for id, row := range out {
		slices.SortFunc(row.Protections, func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
		out[id] = row
	}
	return out, nil
}

// claim is EnsureRepo's first step, on the name's subject: a create when
// nobody holds the name, and the holder's id when somebody does. newID is
// minted ONCE per call, so every round of one call creates the same
// repository.
func (s *Store) claim(ctx context.Context, a Activation, name string,
	settings RepoSettings) (created, found string, err error) {

	token := NameToken(name)
	subject := NameSubject(name)
	newID := uuid.Must(uuid.NewV7()).String()
	scope := []string{NamePath(token), RepoPath(newID)}
	opID := statelog.NewOpID(time.Now(), "code.create")
	at := s.now()
	var superseded, wrote bool
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated, Session: a.at,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			superseded, wrote, found = false, false, ""
			var fenceErr error
			if superseded, fenceErr = activationSuperseded(ctx, tx, a); fenceErr != nil || superseded {
				return statelog.Decision{}, fenceErr
			}
			readErr := tx.QueryRowContext(ctx, claimSQL, token).Scan(&found)
			switch {
			case errors.Is(readErr, sql.ErrNoRows):
			case readErr != nil:
				return statelog.Decision{}, fmt.Errorf("code: read the claim on %q: %w", name, readErr)
			default:
				return statelog.Decision{}, nil
			}
			wrote = true
			return recordFor(stamp, subject, OpCreate, scope, opID,
				Actor{ID: s.nodeID, Kind: ActorConfig},
				CreateRepo{RepoID: newID, Name: name, Settings: settings}, at)
		},
	})
	switch {
	case err != nil:
		return "", "", fmt.Errorf("code: create %q: %w", name, err)
	case superseded:
		return "", "", fmt.Errorf("%w (creating %q at activation %d)", ErrSuperseded, name, a.epoch)
	case !wrote:
		return "", found, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return "", "", unknownOutcome(ctx, "code: whether %q was created is unknown "+
			"(operation %s); the next attempt decides it again", name, opID)
	case result.Outcome == statelog.OutcomeApplied:
		// APPLIED HERE, so the rows say whether it made the repository: a
		// fence a later activation raised between this decide and this
		// append leaves the name unclaimed by this id.
		var holder string
		if err := s.db.Read(ctx, func(tx *sql.Tx) error {
			err := tx.QueryRowContext(ctx, claimSQL, token).Scan(&holder)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}); err != nil {
			return "", "", fmt.Errorf("code: read back the claim on %q: %w", name, err)
		}
		if holder != newID {
			return "", "", fmt.Errorf("%w (creating %q at activation %d)", ErrSuperseded,
				name, a.epoch)
		}
	}
	return newID, "", nil
}

// settle is EnsureRepo's second step, on the repository's own subject: a
// settings change for a live repository whose settings differ, a restore for a
// deleted one an activation later than its deletion declares again, and
// nothing otherwise.
func (s *Store) settle(ctx context.Context, a Activation, id string,
	settings RepoSettings) (Ensured, error) {

	subject := RepoSubject(id)
	scope := []string{RepoPath(id)}
	opID := statelog.NewOpID(time.Now(), "code.settings")
	at := s.now()
	var (
		out        Ensured
		superseded bool
		purging    bool
	)
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated, Session: a.at,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			out, superseded, purging = Ensured{RepoID: id}, false, false
			var err error
			if superseded, err = activationSuperseded(ctx, tx, a); err != nil || superseded {
				return statelog.Decision{}, err
			}
			row, found, err := repoIn(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !found:
				return statelog.Decision{}, fmt.Errorf("code: the name of repository %s is "+
					"claimed and it has no row — a create writes both, so this node's "+
					"rows are not what the log built", id)
			case row.ManagedBy != ManagedConfig:
				return statelog.Decision{}, fmt.Errorf("%w: %s is managed by %q", ErrNotConfigManaged,
					row.Name, row.ManagedBy)
			case row.ConfigEpoch > a.epoch:
				// THE ROW ITSELF PROVES A LATER ACTIVATION WROTE IT.
				superseded = true
				return statelog.Decision{}, nil
			}
			out.Status = row.Status
			switch row.Status {
			case RepoStatusActive, RepoStatusArchived:
				if row.settings().equal(settings) {
					return statelog.Decision{}, nil
				}
				out.Changed, out.Status = true, settings.status()
				return recordFor(stamp, subject, OpSettings, scope, opID,
					Actor{ID: s.nodeID, Kind: ActorConfig},
					SettingsRecord{RepoID: id, Settings: settings}, at)
			case RepoStatusDeleted:
				if a.epoch <= row.ConfigEpoch {
					return statelog.Decision{}, nil
				}
				out.Changed, out.Status = true, settings.status()
				restored := settings
				return recordFor(stamp, subject, OpRestore, scope, opID,
					Actor{ID: s.nodeID, Kind: ActorConfig},
					LifecycleRecord{RepoID: id, ConfigEpoch: a.epoch, Settings: &restored}, at)
			case RepoStatusPurging:
				purging = true
				return statelog.Decision{}, nil
			}
			return statelog.Decision{}, fmt.Errorf("code: repository %s has status %q, "+
				"which this build does not decide", id, row.Status)
		},
	})
	switch {
	case err != nil:
		return Ensured{}, fmt.Errorf("code: settle repository %s: %w", id, err)
	case superseded:
		return Ensured{}, fmt.Errorf("%w (settling repository %s at activation %d)",
			ErrSuperseded, id, a.epoch)
	case purging:
		return out, fmt.Errorf("%w: repository %s", ErrPurging, id)
	case !out.Changed:
		return out, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return Ensured{}, unknownOutcome(ctx, "code: whether repository %s's settings landed "+
			"is unknown (operation %s); the next attempt decides them again", id, opID)
	case result.Outcome == statelog.OutcomeApplied:
		if err := s.readBack(ctx, id, a); err != nil {
			return Ensured{}, err
		}
	}
	return out, nil
}

// DeleteRepo marks a declared repository deleted — it left code.repos — at the
// activation a, and reports whether this call wrote the record. Nothing is
// written for a repository already deleted or purging, one the configuration
// does not manage, or one a later activation last wrote.
func (s *Store) DeleteRepo(ctx context.Context, a Activation, repoID string) (bool, error) {
	if a.epoch == 0 {
		return false, ErrNoActivation
	}
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	opID := statelog.NewOpID(time.Now(), "code.delete")
	at := s.now()
	var wrote, superseded bool
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated, Session: a.at,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			wrote, superseded = false, false
			var err error
			if superseded, err = activationSuperseded(ctx, tx, a); err != nil || superseded {
				return statelog.Decision{}, err
			}
			row, found, err := repoIn(ctx, tx, repoID)
			if err != nil || !found {
				return statelog.Decision{}, err
			}
			if (row.Status != RepoStatusActive && row.Status != RepoStatusArchived) ||
				row.ManagedBy != ManagedConfig || row.ConfigEpoch > a.epoch {
				return statelog.Decision{}, nil
			}
			wrote = true
			return recordFor(stamp, subject, OpDelete, scope, opID,
				Actor{ID: s.nodeID, Kind: ActorConfig},
				LifecycleRecord{RepoID: repoID, ConfigEpoch: a.epoch, Reason: "removed from code.repos"}, at)
		},
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("code: delete repository %s: %w", repoID, err)
	case superseded:
		return false, fmt.Errorf("%w (deleting repository %s at activation %d)",
			ErrSuperseded, repoID, a.epoch)
	case !wrote:
		return false, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return false, unknownOutcome(ctx, "code: whether repository %s was deleted is unknown "+
			"(operation %s); the next attempt decides it again", repoID, opID)
	case result.Outcome == statelog.OutcomeApplied:
		if err := s.readBack(ctx, repoID, a); err != nil {
			return false, err
		}
	}
	return true, nil
}

// readBack reads, after an applied record of activation a, whether the
// repository's row reached a: a record a later activation's fence refused
// leaves it below, which the caller is told as [ErrSuperseded].
func (s *Store) readBack(ctx context.Context, id string, a Activation) error {
	var epoch int64
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, readBackSQL, id).Scan(&epoch)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w (repository %s is gone)", ErrSuperseded, id)
	case err != nil:
		return fmt.Errorf("code: read back repository %s: %w", id, err)
	case epoch < a.epoch:
		return fmt.Errorf("%w (repository %s stayed at activation %d)", ErrSuperseded, id, epoch)
	}
	return nil
}

// activationSuperseded reports a snapshot whose fence a LATER activation has
// raised, and refuses one holding no fence at a at all — which no Activation
// made by [Store.Activate] can meet, since every decide waits for its fence.
func activationSuperseded(ctx context.Context, tx *sql.Tx, a Activation) (bool, error) {
	fence, err := fenceIn(ctx, tx)
	switch {
	case err != nil:
		return false, err
	case fence < a.epoch:
		return false, fmt.Errorf("code: this snapshot's activation fence is at %d, below "+
			"activation %d — a decide of an activation waits for its own fence, so this "+
			"Activation was not made by Activate", fence, a.epoch)
	}
	return fence > a.epoch, nil
}

// fenceIn is the activation fence in tx, zero where none has been raised.
func fenceIn(ctx context.Context, tx *sql.Tx) (int64, error) {
	var fence int64
	err := tx.QueryRowContext(ctx, fenceSQL, activationID).Scan(&fence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("code: read the activation fence: %w", err)
	}
	return fence, nil
}

// repoRow is a repository's row as the lifecycle decides read it.
type repoRow struct {
	Name          string
	Status        RepoStatus
	ManagedBy     ManagedBy
	Kind          RepoKind
	Owner         Owner
	Visibility    Visibility
	DefaultBranch string
	Description   string
	ConfigEpoch   int64
	PurgeParts    int
	DeletedAt     sql.NullInt64
	Protections   []Protection
	// LastActivityAt is the repository's idle clock, as the store encodes an
	// instant, and Version the composed position of the last record on its
	// subject: what a scratch expiry's decide holds to its pass
	// ([Store.ExpireScratch]).
	LastActivityAt int64
	Version        int64
}

// settings is the row's settings, for comparison with a declaration.
func (r repoRow) settings() RepoSettings {
	return RepoSettings{
		ManagedBy: r.ManagedBy, Kind: r.Kind, Owner: r.Owner, Visibility: r.Visibility,
		DefaultBranch: r.DefaultBranch, Description: r.Description, Archived: r.Status == RepoStatusArchived,
		Protections: r.Protections, ConfigEpoch: r.ConfigEpoch,
	}
}

// repoIn reads a repository's row and its rules in tx.
func repoIn(ctx context.Context, tx *sql.Tx, id string) (repoRow, bool, error) {
	var (
		row                                       repoRow
		status, managed, kind, owners, visibility string
	)
	err := tx.QueryRowContext(ctx, repoRowSQL, id).Scan(&row.Name, &status, &managed, &kind, &owners,
		&row.Owner.Ref, &visibility, &row.DefaultBranch, &row.Description, &row.ConfigEpoch,
		&row.PurgeParts, &row.DeletedAt, &row.LastActivityAt, &row.Version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoRow{}, false, nil
	case err != nil:
		return repoRow{}, false, fmt.Errorf("code: read repository %s: %w", id, err)
	}
	row.Status, row.ManagedBy, row.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
	row.Kind, row.Visibility = RepoKind(kind), Visibility(visibility)
	if row.Protections, err = protectionsIn(ctx, tx, id); err != nil {
		return repoRow{}, false, err
	}
	return row, true, nil
}

// protectionsIn reads a repository's rules, sorted by pattern — the order a
// declaration's settings carry them in.
func protectionsIn(ctx context.Context, tx *sql.Tx, id string) ([]Protection, error) {
	rows, err := tx.QueryContext(ctx, protectionsSQL, id)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's protections: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Protection
	for rows.Next() {
		var p Protection
		var direct string
		if err := rows.Scan(&p.Pattern, &p.AllowForce, &p.AllowDelete, &direct); err != nil {
			return nil, fmt.Errorf("code: read %s's protections: %w", id, err)
		}
		p.DirectPush = DirectPush(direct)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read %s's protections: %w", id, err)
	}
	// THE STATEMENT ORDERS BY PATTERN, and this states it again as a
	// property the comparison relies on rather than a property of a plan.
	slices.SortFunc(out, func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
	return out, nil
}

// The lifecycle decides' statements. claimsSQL and reposRowsSQL are
// [Store.unchanged]'s batch forms of claimSQL and repoRowSQL, each driven from
// its JSON list (planner.go); the batch's rules are the listing's own
// statement, protectionsOfSQL.
const (
	claimSQL  = `SELECT repo_id FROM code_names WHERE token = ?`
	claimsSQL = `SELECT token, repo_id FROM code_names
		WHERE token IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	reposRowsSQL = `SELECT id, name, status, managed_by, kind, owner_kind, owner, visibility, default_branch,
		description, config_epoch, purge_parts, deleted_at FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	repoRowSQL = `SELECT name, status, managed_by, kind, owner_kind, owner, visibility, default_branch,
		description, config_epoch, purge_parts, deleted_at, last_activity_at, version FROM code_repos
		WHERE id = ?`
	protectionsSQL = `SELECT pattern, allow_force, allow_delete, direct_push FROM code_protections
		WHERE repo_id = ? ORDER BY pattern`
	readBackSQL = `SELECT config_epoch FROM code_repos WHERE id = ?`
)
