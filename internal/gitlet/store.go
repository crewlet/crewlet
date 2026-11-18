package gitlet

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

// THE LIFECYCLE STORE: a repository's writes other than its pushes — created,
// settled and restored by the configuration's reconcile, deleted when it leaves
// code.repos, purged by the code-retention pass — plus a node's eviction.
//
// Every write is ONE record through the domain's publisher, decided inside its
// snapshot from rows read there, under a FRESH operation id per call: the next
// call finds its own value already there and writes nothing. Every decide
// resets its out-variables each round: a round that lost the broker's
// arbitration is followed by one that finds the winner's value, and only the
// last round says what the call did.

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

	// The fields below decide a tool's lifecycle request ([Store.Lifecycle])
	// and none is required: the eviction gate's store decides only evictions.
	// A store missing SeatRules or Policy answers every lifecycle request
	// [ErrNotServing], so the router asks the next data node, rather than
	// judging one by a nil chart.

	// SeatRules is the DECIDING node's epoch's chart
	// ([CommitterOptions.SeatRules]), read once a request: a seat's grant and
	// a create's owner are judged by it, and false (no epoch installed yet)
	// answers every request [ErrNotServing].
	SeatRules func() (SeatRules, bool)
	// Current is whether this node serves the company's configuration
	// (Reconciler.Admits): false answers EVERY request [ErrNotServing], a
	// person's create included, since every kind's "who may" reads the chart.
	// Nil is current.
	Current func() bool
	// Policy is the epoch's code.scratch and the repository bound
	// ([ScratchOf]), read once a create.
	Policy func() LifecyclePolicy
	// People answers who holds a human seat and with which apps: what a
	// create or settings change setting a push mirror that pushes as a
	// person's seat is judged by, since the mirror publishes only what that
	// seat reads and a person reads only through Gitlet (ADR-0110). Without it
	// such a request answers [ErrNotServing].
	People People
}

// Store is Gitlet's lifecycle write path.
type Store struct {
	publisher Publisher
	db        store.ReplicatedReader
	nodeID    string
	now       func() time.Time

	seatRules func() (SeatRules, bool)
	current   func() bool
	policy    func() LifecyclePolicy
	people    People
}

// NewStore builds the lifecycle store, refusing what it cannot work without.
func NewStore(o StoreOptions) (*Store, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("gitlet: a store needs the code log's write authority — " +
			"every write here is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("gitlet: a store needs the replicated estate a write " +
			"decides from")
	case strings.TrimSpace(o.NodeID) == "":
		return nil, errors.New("gitlet: a store needs the deciding node's id, which a " +
			"configuration or system record names as its actor")
	}
	s := &Store{publisher: o.Publisher, db: o.DB, nodeID: o.NodeID, now: o.Now, seatRules: o.SeatRules,
		current: o.Current, policy: o.Policy, people: o.People}
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

	// at is the position of this node's own fence record, or zero when
	// Activate's snapshot already held the fence. Every later decide waits for
	// this node to apply it (the request's session floor): that puts every
	// decision of this activation after its fence.
	at statelog.Position
}

// Epoch is the activation's stamp.
func (a Activation) Epoch() int64 { return a.epoch }

// Activate raises the code log's ACTIVATION FENCE to the configuration
// activated at activatedAt: the first write of every reconcile of code.repos
// (doc.go, "The activation fence").
//
// ONE RECORD AN ACTIVATION, fleet-wide: the company subject arbitrates every
// data node's call, and a snapshot already fenced at this activation writes
// nothing. An activation OLDER than the fence is [ErrSuperseded]: a later
// configuration owns the declared repositories.
func (s *Store) Activate(ctx context.Context, activatedAt time.Time) (Activation, error) {
	if activatedAt.IsZero() {
		return Activation{}, ErrNoActivation
	}
	epoch := configplane.ActivationStamp(activatedAt)
	at := s.now()
	opID := statelog.NewOpID(time.Now(), "code_activate")
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
				// Already fenced here, by this node or a peer: every
				// later decide is after it.
				return statelog.Decision{}, nil
			}
			return recordFor(stamp, subject, OpActivate, scope, opID,
				Actor{ID: s.nodeID, Kind: ActorConfig}, Activated{ConfigEpoch: epoch}, at)
		},
	})
	if err != nil {
		return Activation{}, fmt.Errorf("gitlet: raise the activation fence to %d: %w", epoch, err)
	}
	if superseded {
		return Activation{}, fmt.Errorf("%w (activation %d)", ErrSuperseded, epoch)
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return Activation{}, unknownOutcome(ctx, "gitlet: whether the activation fence reached %d "+
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
	// Import is the declaration's import ([CreateRepo.Import]), read by a
	// create alone: for an existing repository EnsureRepo settles the
	// settings and passes the import by (the engine's reconcile reports
	// code_import_ignored).
	Import *ImportSource
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
// settings at the activation a: it creates it, settles a settings change, or
// restores a deleted one. A whole declaration set takes [Store.EnsureRepos].
//
// An unchanged declaration writes NOTHING, whatever activation last stamped
// the row: the activation fence ([Store.Activate]) keeps an older activation
// from walking a newer one back, so the row needs no re-stamp.
//
// A deleted repository is restored only by an activation LATER than its
// deletion's, so an older one applied late cannot bring it back. A purging one answers [ErrPurging], one the configuration does
// not manage [ErrNotConfigManaged], and a record a later fence refused
// (decided or read back) [ErrSuperseded].
func (s *Store) EnsureRepo(ctx context.Context, a Activation, d Declared) (Ensured, error) {
	if a.epoch == 0 {
		return Ensured{}, ErrNoActivation
	}
	if err := d.Validate(); err != nil {
		return Ensured{}, err
	}
	settings := d.settingsAt(a)

	created, found, err := s.claim(ctx, a, d.Name, settings, d.Import)
	if err != nil {
		return Ensured{}, err
	}
	if created != "" {
		create := CreateRepo{Settings: settings, Import: d.Import}
		return Ensured{RepoID: created, Changed: true, Status: create.status()}, nil
	}
	return s.settle(ctx, a, found, settings)
}

// Validate refuses a declaration no activation can ensure: a name that is no
// PROJECT's (a seat's dotted scratch name only that seat's tool creates), and
// settings that are no configuration's as the reconcile records them
// (settingsAt), including a kind other than a project's.
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
	settings := d.settingsAt(Activation{})
	if err := settings.Validate(); err != nil {
		return err
	}
	if why := (CreateRepo{Name: d.Name, Settings: settings, Import: d.Import}).importInvalid(); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	return nil
}

// settingsAt is the declaration's settings stamped with the activation a: THE
// ONE PLACE a reconcile's settings are formed, so every one is a PROJECT's.
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

// EnsureRepos is [Store.EnsureRepo] over a declaration set at the activation
// a (the reconcile's form), answering in the order given what EnsureRepo would.
//
// The set is read ONCE ([Store.unchanged]), not two publishes a repository
// (doc.go, "Measured"), passing every gate a per-repository publish does: a
// live repository declared as it is, or one deleted at this activation or
// later, is answered from that read. The rest go through EnsureRepo,
// [reconcileInFlight] at a time ([decideEach]), each on its own subjects so
// none contend. A failure all would meet ([sharedFailure]) is the call's error
// and the same value answers each declaration left undecided or ended; those
// decided before, or invalid ([ErrInvalid]), keep their own, and nothing is in
// flight on return.
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
		// THE ONE REFUSAL A NARROWER SCOPE ANSWERS DIFFERENTLY: the batch
		// probes the whole domain, a repository only its own paths, so each
		// is decided on its own.
		unchanged = nil
	default:
		// EVERY OTHER FAILURE IS EVERY DECLARATION'S ANSWER: the node's
		// fences, the session floor, a later fence (the one record this
		// subject can be behind, which every decide sees first), the estate,
		// the context. Through EnsureRepo each would wait out the same budget
		// for it: hours, at ten thousand declarations on a stalled applier.
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

// DeleteRepos is [Store.DeleteRepo] over every repository ids names at the
// activation a (the reconcile's form, for configuration-managed repositories a
// document no longer declares), answering in the order given.
//
// Decided [reconcileInFlight] at a time, for [Store.EnsureRepos]' reason, and
// a [sharedFailure] ends the call as it ends EnsureRepos': the call's error is
// the answer of every repository left undecided or ended, and nothing is in
// flight on return.
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
// ([Store.EnsureRepos], [Store.DeleteRepos]): as many of the widest record a
// reconcile writes as one apply transaction holds, fifteen.
//
// Enough to hide a decision's round trip (Little's law): one node applies
// ~550 a second and a round trip is 6.7 ms, so four reach that floor (doc.go,
// "Measured"); fifteen keep it up to a ~27 ms round trip, room for a quorum
// within a region. No wider: past statelog.ApplyTxRowBudget (4 000; fifteen of
// [reconcileRecordRows] are 3 915) the applier splits the batch, and a push
// appended behind the reconcile waits on each part
// (TestAReconcileInFlightFitsOneApplyTransaction holds the product).
const reconcileInFlight = statelog.ApplyTxRowBudget / reconcileRecordRows

// reconcileRecordRows is the most rows one reconcile record touches as the
// applier counts them: a settings change or restore deletes and rewrites a
// repository's rules and path owners (at most [MaxProtectionsPerRepo] and
// [MaxPathOwners] each), moves its row (which carries both digests,
// [rowDigests]), the two change tally rows an owner or visibility change moves
// counts between (apply_counts.go) and its two repository tally rows
// (apply_repotallies.go). A create writes at most 132, a deletion three.
const reconcileRecordRows = 2*(MaxProtectionsPerRepo+MaxPathOwners) + 1 + tallyMoveRows + repoTallyMoveRows

// decideEach runs decide for every k in [0, n), up to [reconcileInFlight] at
// once as a CLOSED LOOP (one begins only when one ends), and answers the run's
// ending (nil if none): the failure that ended it, and for each k whether it
// is k's answer.
//
// A run ends at the first [sharedFailure] or when ctx ends, and then the
// failure is ctx's own error, so a reconcile ended on purpose answers as its
// context. Decisions in flight are cancelled, none begins after, and
// decideEach returns only once every one it began has. The ending answers each
// decision never begun, each that met it, and each that FAILED after the end
// whatever its error says ([unknownOutcome] has why); a success after the end,
// or a failure before it, keeps its own answer.
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
	// settle files decision k's failure err as the ending where it is shared,
	// or where the run had already ended when it returned (afterEnd).
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
// reconcile, is one every other repository meets too, which ends the
// reconcile ([decideEach]): the context ended; a LATER activation's fence
// ([ErrSuperseded]), which only rises and every decide reads first; or the
// code log refusing THIS NODE (its eviction or an unreadable eviction state, a
// floor it is below or cannot read, unreadable usage, a full, skewed,
// truncated or foreign log). Every other failure is the repository's own: a
// purge, a manager other than the configuration, an undecodable record on its
// paths, a marker or voided record on its subject, a lost race, an unknown
// outcome while its context was live.
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
		// THIS NODE'S, unless the refusal names another node's copy of the
		// operation (statelog.Unavailable.CopyWriter).
		return refused.CopyWriter == ""
	case statelog.ReasonEvictionUnknown, statelog.ReasonBelowFloor, statelog.ReasonFloorUnknown,
		statelog.ReasonUsageUnknown, statelog.ReasonLogFull, statelog.ReasonSkew,
		statelog.ReasonLogTruncated, statelog.ReasonWrongStream:
		return true
	}
	return false
}

// unknownOutcome is the error of a write whose outcome this node could not
// establish (the next attempt decides it again), WRAPPING ctx's end where ctx
// has ended: statelog answers an append whose context ended mid-flight
// OutcomeUnknown with NO error, since the probe that would classify it runs on
// that same context. A caller that ended the write on purpose must read it as
// that ending ([sharedFailure], errors.Is), not as a failed write of its own.
func unknownOutcome(ctx context.Context, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	if ended := ctx.Err(); ended != nil {
		return fmt.Errorf("%w: %w", err, ended)
	}
	return err
}

// unchanged reads, in ONE publish that writes nothing, which declarations
// need no record at the activation a, answering what EnsureRepo would for
// each ([Store.EnsureRepos]). It runs on the fence's subject after the fence's
// session floor, so its snapshot is the one every decision of a reads from,
// and a later fence is [ErrSuperseded] for every declaration at once.
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
		OpID:    statelog.NewOpID(time.Now(), "code_declared"),
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
		return nil, fmt.Errorf("gitlet: read the declared repositories at activation %d: %w",
			a.epoch, err)
	case superseded:
		return nil, fmt.Errorf("%w (reading the declared repositories at activation %d)",
			ErrSuperseded, a.epoch)
	}
	return found, nil
}

// unchangedIn is [Store.unchanged]'s read in tx: the declared names' claims
// and their repositories' rows, rules and path owners (each statement driven
// from its list, seeking a primary key an element, planner.go), answering the
// declarations both of EnsureRepo's decides would write nothing for.
func unchangedIn(ctx context.Context, tx *sql.Tx, a Activation, declared []Declared) (map[string]Ensured, error) {
	tokens := make([]string, len(declared))
	for i, d := range declared {
		tokens[i] = NameToken(d.Name)
	}
	claims, err := keyPairs(ctx, tx, claimsSQL, tokens)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the declared names' claims: %w", err)
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
		case RepoStatusImporting, RepoStatusActive, RepoStatusArchived:
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

// reposIn reads the rows, rules and path owners of every repository ids names,
// in three statements — the batch form of [repoIn].
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
		return nil, fmt.Errorf("gitlet: read the declared repositories: %w", err)
	}
	for rows.Next() {
		var id string
		var row repoRow
		var status, managed, kind, owners, visibility, search, from, to string
		var pending bool
		if err = rows.Scan(&id, &row.Name, &status, &managed, &kind, &owners, &row.Owner.Ref, &visibility,
			&search, &row.DefaultBranch, &row.Description, &row.ConfigEpoch, &row.PurgeParts,
			&row.DeletedAt, &from, &to, &pending); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("gitlet: read the declared repositories: %w", err)
		}
		row.Status, row.ManagedBy, row.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
		row.Kind, row.Visibility, row.Search = RepoKind(kind), Visibility(visibility), SearchMode(search)
		if err = row.remotesOf(from, to, pending); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[id] = row
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("gitlet: read the declared repositories: %w", err)
	}
	_ = rows.Close()
	// THE RULES, sorted by pattern within each repository — the order a
	// declaration's settings carry them in, which the comparison relies on.
	rules, err := tx.QueryContext(ctx, protectionsOfSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the declared repositories' protections: %w", err)
	}
	defer func() { _ = rules.Close() }()
	for rules.Next() {
		var id, direct, gate string
		var p Protection
		if err = rules.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct, &gate); err != nil {
			return nil, fmt.Errorf("gitlet: read the declared repositories' protections: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		if p.Gate, err = gateFrom(gate); err != nil {
			return nil, fmt.Errorf("gitlet: read repository %s's rule %s: %w", id, p.Pattern, err)
		}
		if row, ok := out[id]; ok {
			row.Protections = append(row.Protections, p)
			out[id] = row
		}
	}
	if err := rules.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the declared repositories' protections: %w", err)
	}
	// The path owners in one statement too, sorted below into a declaration's
	// order.
	if err := pathOwnersOf(ctx, tx, list, func(id string, po PathOwner) {
		if row, ok := out[id]; ok {
			row.PathOwners = append(row.PathOwners, po)
			out[id] = row
		}
	}); err != nil {
		return nil, fmt.Errorf("gitlet: read the declared repositories' path owners: %w", err)
	}
	for id, row := range out {
		slices.SortFunc(row.Protections, func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
		slices.SortFunc(row.PathOwners, comparePathOwners)
		out[id] = row
	}
	return out, nil
}

// pathOwnersOf reads the path owners of every repository list — a JSON list of
// ids — in ONE statement, handing each to keep with its repository's id, in no
// order a caller may rely on.
func pathOwnersOf(ctx context.Context, tx *sql.Tx, list string, keep func(id string, po PathOwner)) error {
	rows, err := tx.QueryContext(ctx, pathOwnersOfSQL, list)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, kind, packed string
		var po PathOwner
		if err := rows.Scan(&id, &kind, &po.Owner.Ref, &packed); err != nil {
			return err
		}
		po.Owner.Kind, po.Paths = OwnerKind(kind), unpackPaths(packed)
		keep(id, po)
	}
	return rows.Err()
}

// claim is EnsureRepo's first step, on the name's subject: a create when
// nobody holds the name, and the holder's id when somebody does. newID is
// minted ONCE per call, so every round of one call creates the same
// repository.
func (s *Store) claim(ctx context.Context, a Activation, name string, settings RepoSettings,
	imports *ImportSource) (created, found string, err error) {

	token := NameToken(name)
	subject := NameSubject(name)
	newID := uuid.Must(uuid.NewV7()).String()
	scope := []string{NamePath(token), RepoPath(newID)}
	opID := statelog.NewOpID(time.Now(), "code_create")
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
				return statelog.Decision{}, fmt.Errorf("gitlet: read the claim on %q: %w", name, readErr)
			default:
				return statelog.Decision{}, nil
			}
			wrote = true
			return recordFor(stamp, subject, OpCreate, scope, opID,
				Actor{ID: s.nodeID, Kind: ActorConfig},
				CreateRepo{RepoID: newID, Name: name, Settings: settings, Import: imports}, at)
		},
	})
	switch {
	case err != nil:
		return "", "", fmt.Errorf("gitlet: create %q: %w", name, err)
	case superseded:
		return "", "", fmt.Errorf("%w (creating %q at activation %d)", ErrSuperseded, name, a.epoch)
	case !wrote:
		return "", found, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return "", "", unknownOutcome(ctx, "gitlet: whether %q was created is unknown "+
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
			return "", "", fmt.Errorf("gitlet: read back the claim on %q: %w", name, err)
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
	opID := statelog.NewOpID(time.Now(), "code_settings")
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
				return statelog.Decision{}, fmt.Errorf("gitlet: the name of repository %s is "+
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
			// AN IMPORT STILL TO BE DONE IS NEVER ARCHIVED: its importing
			// status is the one an archive would take, and an archived
			// repository takes nothing an import lands.
			pending := row.Status == RepoStatusImporting ||
				(row.Status == RepoStatusDeleted && row.Remotes.importPending)
			if pending && settings.Archived && !row.settings().equal(settings) {
				return statelog.Decision{}, fmt.Errorf("%w: %s is being imported from another host, and "+
					"is archived once its import is done: an archived repository takes nothing an import "+
					"lands", ErrInvalid, row.Name)
			}
			switch row.Status {
			case RepoStatusImporting, RepoStatusActive, RepoStatusArchived:
				if row.settings().equal(settings) {
					return statelog.Decision{}, nil
				}
				out.Changed = true
				switch {
				case row.Status == RepoStatusImporting:
				case row.Remotes.startsImport(settings) != nil:
					// MADE A PULL MIRROR OF A SOURCE IT DOES NOT FOLLOW
					// YET: imported from it first, as its apply makes it.
					out.Status = RepoStatusImporting
				default:
					out.Status = settings.status()
				}
				return recordFor(stamp, subject, OpSettings, scope, opID,
					Actor{ID: s.nodeID, Kind: ActorConfig},
					SettingsRecord{RepoID: id, Settings: settings}, at)
			case RepoStatusDeleted:
				if a.epoch <= row.ConfigEpoch {
					return statelog.Decision{}, nil
				}
				out.Changed, out.Status = true, row.Remotes.restoredStatus(settings)
				restored := settings
				return recordFor(stamp, subject, OpRestore, scope, opID,
					Actor{ID: s.nodeID, Kind: ActorConfig},
					LifecycleRecord{RepoID: id, ConfigEpoch: a.epoch, Settings: &restored}, at)
			case RepoStatusPurging:
				purging = true
				return statelog.Decision{}, nil
			}
			return statelog.Decision{}, fmt.Errorf("gitlet: repository %s has status %q, "+
				"which this build does not decide", id, row.Status)
		},
	})
	switch {
	case err != nil:
		return Ensured{}, fmt.Errorf("gitlet: settle repository %s: %w", id, err)
	case superseded:
		return Ensured{}, fmt.Errorf("%w (settling repository %s at activation %d)",
			ErrSuperseded, id, a.epoch)
	case purging:
		return out, fmt.Errorf("%w: repository %s", ErrPurging, id)
	case !out.Changed:
		return out, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return Ensured{}, unknownOutcome(ctx, "gitlet: whether repository %s's settings landed "+
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
	opID := statelog.NewOpID(time.Now(), "code_delete")
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
			if !row.Status.live() || row.ManagedBy != ManagedConfig || row.ConfigEpoch > a.epoch {
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
		return false, fmt.Errorf("gitlet: delete repository %s: %w", repoID, err)
	case superseded:
		return false, fmt.Errorf("%w (deleting repository %s at activation %d)",
			ErrSuperseded, repoID, a.epoch)
	case !wrote:
		return false, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return false, unknownOutcome(ctx, "gitlet: whether repository %s was deleted is unknown "+
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
		return fmt.Errorf("gitlet: read back repository %s: %w", id, err)
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
		return false, fmt.Errorf("gitlet: this snapshot's activation fence is at %d, below "+
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
		return 0, fmt.Errorf("gitlet: read the activation fence: %w", err)
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
	Search        SearchMode
	DefaultBranch string
	Description   string
	ConfigEpoch   int64
	PurgeParts    int
	DeletedAt     sql.NullInt64
	Protections   []Protection
	PathOwners    []PathOwner
	// Remotes are its mirrors and whether its import is still to be done.
	Remotes repoRemotes
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
		ManagedBy: r.ManagedBy, Kind: r.Kind, Owner: r.Owner, Visibility: r.Visibility, Search: r.Search,
		DefaultBranch: r.DefaultBranch, Description: r.Description, Archived: r.Status == RepoStatusArchived,
		Protections: r.Protections, PathOwners: r.PathOwners, MirrorFrom: r.Remotes.mirrorFrom(),
		MirrorTo: r.Remotes.mirrorTo(), ConfigEpoch: r.ConfigEpoch,
	}
}

// remotesOf reads a row's two mirror columns and its pending import, as
// repoRowSQL and reposRowsSQL select them, into r.
func (r *repoRow) remotesOf(from, to string, pending bool) error {
	var err error
	if r.Remotes.from, err = remoteOf[MirrorSource](from); err != nil {
		return fmt.Errorf("gitlet: read %s's mirror_from: %w", r.Name, err)
	}
	if r.Remotes.to, err = remoteOf[MirrorTarget](to); err != nil {
		return fmt.Errorf("gitlet: read %s's mirror_to: %w", r.Name, err)
	}
	r.Remotes.importPending = pending
	return nil
}

// repoIn reads a repository's row, its rules and its path owners in tx.
func repoIn(ctx context.Context, tx *sql.Tx, id string) (repoRow, bool, error) {
	var (
		row                                               repoRow
		status, managed, kind, owners, visibility, search string
		from, to                                          string
		pending                                           bool
	)
	err := tx.QueryRowContext(ctx, repoRowSQL, id).Scan(&row.Name, &status, &managed, &kind, &owners,
		&row.Owner.Ref, &visibility, &search, &row.DefaultBranch, &row.Description, &row.ConfigEpoch,
		&row.PurgeParts, &row.DeletedAt, &row.LastActivityAt, &row.Version, &from, &to, &pending)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoRow{}, false, nil
	case err != nil:
		return repoRow{}, false, fmt.Errorf("gitlet: read repository %s: %w", id, err)
	}
	row.Status, row.ManagedBy, row.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
	row.Kind, row.Visibility, row.Search = RepoKind(kind), Visibility(visibility), SearchMode(search)
	if err = row.remotesOf(from, to, pending); err != nil {
		return repoRow{}, false, err
	}
	if row.Protections, err = protectionsIn(ctx, tx, id); err != nil {
		return repoRow{}, false, err
	}
	if row.PathOwners, err = pathOwnersIn(ctx, tx, id); err != nil {
		return repoRow{}, false, err
	}
	return row, true, nil
}

// pathOwnersIn reads a repository's path owners in tx, in the order its
// settings carry them in ([comparePathOwners]) — nil where it names none, as
// settings that name none carry them.
func pathOwnersIn(ctx context.Context, tx *sql.Tx, id string) ([]PathOwner, error) {
	rows, err := tx.QueryContext(ctx, pathOwnersSQL, id)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's path owners: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []PathOwner
	for rows.Next() {
		var kind, packed string
		var po PathOwner
		if err := rows.Scan(&kind, &po.Owner.Ref, &packed); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's path owners: %w", id, err)
		}
		po.Owner.Kind, po.Paths = OwnerKind(kind), unpackPaths(packed)
		out = append(out, po)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's path owners: %w", id, err)
	}
	// Sorted again: the comparison relies on the order, not on a plan.
	slices.SortFunc(out, comparePathOwners)
	return out, nil
}

// protectionsIn reads a repository's rules WHOLE, each with its merge gate,
// sorted by pattern — the order a declaration's settings carry them in: what
// a decide that rewrites the settings, or judges a change to a rule, reads
// ([repoIn], the lifecycle decides). No read of the repository's row does
// ([pushRulesIn]).
func protectionsIn(ctx context.Context, tx *sql.Tx, id string) ([]Protection, error) {
	rows, err := tx.QueryContext(ctx, protectionsSQL, id)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's protections: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Protection
	for rows.Next() {
		var p Protection
		var direct, gate string
		if err = rows.Scan(&p.Pattern, &p.AllowForce, &p.AllowDelete, &direct, &gate); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's protections: %w", id, err)
		}
		p.DirectPush = DirectPush(direct)
		if p.Gate, err = gateFrom(gate); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's rule %s: %w", id, p.Pattern, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's protections: %w", id, err)
	}
	// Sorted again: the comparison relies on the order, not on a plan.
	slices.SortFunc(out, func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
	return out, nil
}

// ruleIn reads pattern's rule of the repository id WHOLE, its merge gate
// included — what a protect request's answer names ([LifecycleResult.Rule]) —
// and nil where the repository holds no rule for it: one row by the table's
// key, never the repository's every rule with its gate.
func ruleIn(ctx context.Context, tx *sql.Tx, id, pattern string) (*Protection, error) {
	var (
		p            Protection
		direct, gate string
	)
	err := tx.QueryRowContext(ctx, ruleSQL, id, pattern).Scan(&p.Pattern, &p.AllowForce, &p.AllowDelete,
		&direct, &gate)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("gitlet: read %s's rule %s: %w", id, pattern, err)
	}
	p.DirectPush = DirectPush(direct)
	if p.Gate, err = gateFrom(gate); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's rule %s: %w", id, pattern, err)
	}
	return &p, nil
}

// pushRulesIn reads a repository's rules as a PUSH is judged by them
// ([PushRule]), WITHOUT the gate column: a push is held to its rule's flags and
// direct push, and a gate holds only a change's merges. Every read of a
// repository's row ([repoViewIn]) and every round of the push decide reads
// these in a snapshot holding a reader connection, and a repository's gates
// are up to [MaxProtectionsPerRepo] of [MaxGateBytes] of JSON each, decoded
// for nothing while other replicated reads wait ([verdicts.formBefore]).
func pushRulesIn(ctx context.Context, tx *sql.Tx, id string) ([]PushRule, error) {
	rows, err := tx.QueryContext(ctx, pushRulesSQL, id)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's protections: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []PushRule
	for rows.Next() {
		var p PushRule
		var direct string
		if err := rows.Scan(&p.Pattern, &p.AllowForce, &p.AllowDelete, &direct); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's protections: %w", id, err)
		}
		p.DirectPush = DirectPush(direct)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's protections: %w", id, err)
	}
	// Sorted again: the comparison relies on the order, not on a plan.
	slices.SortFunc(out, func(a, b PushRule) int { return strings.Compare(a.Pattern, b.Pattern) })
	return out, nil
}

// gatesIn reads the merge gates of a repository's rules that carry one, each
// named by its rule's pattern, in pattern order — what the changes falling due
// read beside their facts — and nil where no rule carries one. A
// repository's detail reads the same gates with who set each rule, in one
// statement ([ruleDetailsIn]).
func gatesIn(ctx context.Context, tx *sql.Tx, id string) ([]RuleGate, error) {
	rows, err := tx.QueryContext(ctx, gatesSQL, id)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's merge gates: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []RuleGate
	for rows.Next() {
		var r RuleGate
		var column string
		if err = rows.Scan(&r.Pattern, &column); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's merge gates: %w", id, err)
		}
		// NEVER AN EMPTY COLUMN, which the statement leaves out: one here is
		// refused as the gate it does not decode to, never read as none.
		if r.Gate, err = decodeGate(column); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's rule %s: %w", id, r.Pattern, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's merge gates: %w", id, err)
	}
	// Sorted again: a gate is found beside its rule by walking both in order.
	slices.SortFunc(out, func(a, b RuleGate) int { return strings.Compare(a.Pattern, b.Pattern) })
	return out, nil
}

// ruleDetailsIn reads a repository's rules as its detail answers a read
// asking for its gates ([ReadQuery.Gates]): each gated rule's [RuleGate] and
// who set every rule and when ([RuleSource]), in pattern order, nil where none.
//
// ONE SEEK of the rules for both, since they are the same rows in the same
// transaction (as [detailRowIn] reads its row once); [gatesIn] stays separate
// for the changes falling due, which read nothing of who set the rules.
func ruleDetailsIn(ctx context.Context, tx *sql.Tx, id string) ([]RuleGate, []RuleSource, error) {
	rows, err := tx.QueryContext(ctx, ruleDetailsSQL, id)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlet: read %s's rules: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		gates   []RuleGate
		sources []RuleSource
	)
	for rows.Next() {
		var (
			r      RuleSource
			column string
			kind   string
			at     int64
		)
		if err = rows.Scan(&r.Pattern, &column, &r.SetBy, &kind, &at); err != nil {
			return nil, nil, fmt.Errorf("gitlet: read %s's rules: %w", id, err)
		}
		r.SetByKind, r.SetAt = ActorKind(kind), store.DecodeTime(at)
		sources = append(sources, r)
		// AN EMPTY COLUMN IS A RULE WITH NO GATE, the rows [gatesIn]'s
		// statement leaves out; any other is decoded, and one that does not
		// decode is refused rather than read as none.
		if column == "" {
			continue
		}
		g := RuleGate{Pattern: r.Pattern}
		if g.Gate, err = decodeGate(column); err != nil {
			return nil, nil, fmt.Errorf("gitlet: read %s's rule %s: %w", id, r.Pattern, err)
		}
		gates = append(gates, g)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("gitlet: read %s's rules: %w", id, err)
	}
	// Sorted again: a gate and a source are found beside their rule by
	// walking them in order.
	slices.SortFunc(gates, func(a, b RuleGate) int { return strings.Compare(a.Pattern, b.Pattern) })
	slices.SortFunc(sources, func(a, b RuleSource) int { return strings.Compare(a.Pattern, b.Pattern) })
	return gates, sources, nil
}

// The lifecycle decides' statements. claimsSQL and reposRowsSQL are
// [Store.unchanged]'s batch forms of claimSQL and repoRowSQL, each driven from
// its JSON list (planner.go); the batch's rules are protectionsOfSQL, the
// batch form of protectionsSQL — the listing's pushRulesOfSQL with each rule's
// gate, which the comparison with a declaration reads — and its path owners
// pathOwnersOfSQL, the batch form of pathOwnersSQL.
const (
	claimSQL  = `SELECT repo_id FROM code_names WHERE token = ?`
	claimsSQL = `SELECT token, repo_id FROM code_names
		WHERE token IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	reposRowsSQL = `SELECT id, name, status, managed_by, kind, owner_kind, owner, visibility, search,
		default_branch, description, config_epoch, purge_parts, deleted_at, mirror_from, mirror_to,
		import_source != '' AND imported_at = 0 FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	repoRowSQL = `SELECT name, status, managed_by, kind, owner_kind, owner, visibility, search, default_branch,
		description, config_epoch, purge_parts, deleted_at, last_activity_at, version, mirror_from, mirror_to,
		import_source != '' AND imported_at = 0 FROM code_repos
		WHERE id = ?`
	protectionsSQL = `SELECT pattern, allow_force, allow_delete, direct_push, gate FROM code_protections
		WHERE repo_id = ? ORDER BY pattern`
	// pushRulesSQL is protectionsSQL without the gate: the rules of a
	// repository's row and of the push decide ([pushRulesIn]).
	pushRulesSQL = `SELECT pattern, allow_force, allow_delete, direct_push FROM code_protections
		WHERE repo_id = ? ORDER BY pattern`
	// ruleSQL is protectionsSQL for one pattern, by the table's key
	// ([ruleIn]): a protect request's answer.
	ruleSQL = `SELECT pattern, allow_force, allow_delete, direct_push, gate FROM code_protections
		WHERE repo_id = ? AND pattern = ?`
	// gatesSQL is the gates of a repository's rules that carry one, by
	// pattern ([gatesIn]): the changes falling due read them.
	gatesSQL = `SELECT pattern, gate FROM code_protections WHERE repo_id = ? AND gate != '' ORDER BY pattern`
	// ruleDetailsSQL is every rule of a repository with its gate and who set
	// it and when, by pattern ([ruleDetailsIn]): one repository's detail
	// asked for its gates.
	ruleDetailsSQL = `SELECT pattern, gate, set_by, set_by_kind, set_at FROM code_protections
		WHERE repo_id = ? ORDER BY pattern`
	// protectionsOfSQL is protectionsSQL for every repository a JSON list
	// names, in one statement ([reposIn]).
	protectionsOfSQL = `SELECT repo_id, pattern, allow_force, allow_delete, direct_push, gate
		FROM code_protections WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		ORDER BY repo_id, pattern`
	pathOwnersSQL = `SELECT owner_kind, owner, paths FROM code_path_owners
		WHERE repo_id = ? ORDER BY owner_kind, owner`
	pathOwnersOfSQL = `SELECT repo_id, owner_kind, owner, paths FROM code_path_owners
		WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	readBackSQL = `SELECT config_epoch FROM code_repos WHERE id = ?`
)
