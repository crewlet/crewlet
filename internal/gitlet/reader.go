package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about Gitlet from this node's own applied rows.
//
// EVERY READ GOES THROUGH [statelog.Reader], so a level is a property of the
// answer: the refusal ladder, the coverage probe over what the read is ABOUT,
// the freshness target, then the rows in ONE transaction.
//
// AND THE POSITION IS THE ROWS' OWN: every read is stamped with the checkpoint
// [statelog.Reader.ReadThrough] hands its transaction — the checkpoint row
// carried over the barriers reached at release. The release position alone can
// be OLDER than the rows; the row alone LAGS decoded, uncommitted barriers, so a
// linearizable read could stamp below another node's earlier answer, which a
// cache takes for its log gone back.
type Reader struct {
	log    *statelog.Reader
	limits func() Limits
	roster func() Roster
	now    func() time.Time

	// pageRows and pageBytes bound one page: MaxSnapshotItems and
	// MaxPageBytes, and a test's smaller values (export_test.go).
	pageRows  int
	pageBytes int
	// tipWindow bounds the landings one target's tip work reads above its
	// green: MaxTipLandings, and a test's smaller value (export_test.go).
	tipWindow int
	// reposSized and lastPushWalk bound a listing's total and its cards: MaxReposSized and
	// MaxLastPushWalk, or a test's.
	reposSized, lastPushWalk int
	// refsScanned and activeCounted bound a refs listing's scan and its active
	// count: MaxRefsScanned and MaxActiveBranchesCounted, or a test's.
	refsScanned, activeCounted int
	// pushesScanned bounds a narrowed push history's page: MaxPushesScanned, or a test's.
	pushesScanned int
	// spokenScanned bounds a live state's conversation side: MaxSpokenScanned, or a test's.
	spokenScanned int
	// standingCounted bounds a standing count's places a state: MaxStandingCounted, or a test's.
	standingCounted int
	// queueCounted bounds a queue read's and a counts read's walks: MaxQueueCounted, or a test's.
	queueCounted int
	// words is this node's index of changes' words, nil where it keeps none.
	words ChangeWords
	// textCandidates and textTail bound a words listing: MaxChangeTextCandidates and MaxChangeTextTail,
	// or a test's.
	textCandidates, textTail int
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	// Log is the code log's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader

	// Limits are the epoch's code.limits, for RepoDetail alone; nil reads as
	// [DefaultLimits].
	Limits func() Limits

	// Roster is the SERVING node's current epoch's chart (the engine's
	// codeRoster), read once a read: an agent asker is judged by it
	// ([GrantOf]). Nil, or a roster answering nil, refuses every agent's read
	// [ErrNotServing] so the estate asks a node holding a chart
	// ([Reader.chartFor]). A person's read asks nothing of it.
	Roster func() Roster

	// Now is the serving node's clock, which a change's routing is dated by
	// ([RoutedPage.RetainedFrom]): its notices are kept by THIS node's sweeps.
	// Nil reads the wall clock.
	Now func() time.Time

	// ChangeWords is this node's index of changes' words; nil where it keeps none, and a listing the
	// words lead is then refused [ErrNotServing], so the estate asks a node that keeps one.
	ChangeWords ChangeWords
}

// NewReader builds Gitlet's read side.
func NewReader(o ReaderOptions) (*Reader, error) {
	if o.Log == nil {
		return nil, errors.New("gitlet: a reader needs the code log's read authority — " +
			"without it every read level is a label rather than a guarantee")
	}
	r := &Reader{log: o.Log, limits: o.Limits, roster: o.Roster, now: o.Now, pageRows: MaxSnapshotItems,
		pageBytes: MaxPageBytes, tipWindow: MaxTipLandings, refsScanned: MaxRefsScanned,
		activeCounted: MaxActiveBranchesCounted, spokenScanned: MaxSpokenScanned}
	r.pushesScanned = MaxPushesScanned
	if r.limits == nil {
		r.limits = DefaultLimits
	}
	r.standingCounted = MaxStandingCounted
	if r.now == nil {
		r.now = time.Now
	}
	r.reposSized, r.lastPushWalk = MaxReposSized, MaxLastPushWalk
	r.queueCounted = MaxQueueCounted
	r.words, r.textCandidates, r.textTail = o.ChangeWords, MaxChangeTextCandidates, MaxChangeTextTail
	return r, nil
}

// chartFor is the roster a read by asker is judged by: the serving node's
// current chart for a seat's read and for every read judging a change's
// readiness ([ReadQuery.judgesReadiness]), read ONCE a read so a listing is
// judged by one chart, and none for anything else.
//
// A READER HOLDING NO CHART (from boot until its first epoch installs) REFUSES
// such a read [ErrNotServing] before any row, never judges it: judged with no
// roster every seat stands nowhere and no approval counts, an ANSWER that ends
// the estate's walk with what every peer holding a chart would contradict.
// Refused, the node ran nothing (estate's ranNothing) and the walk asks the
// next data node. A chart that DENIES the seat is still the answer.
func (r *Reader) chartFor(asker Principal, readiness bool) (Roster, error) {
	if asker.Kind != PrincipalAgent && !readiness {
		return nil, nil
	}
	var chart Roster
	if r.roster != nil {
		chart = r.roster()
	}
	switch {
	case chart != nil:
		return chart, nil
	case asker.Kind == PrincipalAgent:
		return nil, fmt.Errorf("%w: this node holds no chart to judge a seat's read by", ErrNotServing)
	}
	return nil, fmt.Errorf("%w: this node holds no chart to judge a change's readiness by", ErrNotServing)
}

// judgesReadiness reports a read whose answer carries a change's readiness,
// which the chart judges: a listing of changes, and a change's first page of
// its revisions, which carries its detail.
func (q ReadQuery) judgesReadiness() bool {
	return q.Kind == ReadChanges || q.Kind == ReadChange && q.Cursor == "" && q.pagesDetail() && !q.Brief
}

// resolves reports a read that may resolve the repository it names
// ([ReadQuery.Resolve]): one that names it by name and judges the asker's
// grant on it — a `change` read, and a `changes` or a `checks` listing naming
// one repository.
func (q ReadQuery) resolves() bool {
	return q.Kind == ReadChange || (q.Kind == ReadChanges || q.Kind == ReadChecks) && q.Repo != ""
}

// inTx is one open transaction as a statelog.ReadEstate, so the checkpoint is
// read with the rows.
type inTx struct{ tx *sql.Tx }

func (t inTx) Read(_ context.Context, fn func(*sql.Tx) error) error { return fn(t.tx) }

// checkpointIn is the code log's checkpoint row in tx, for a WRITE's decision
// and a node's read-back of its own rows, which the read authority does not
// serve. A read through the authority uses its handed stamp instead: this row
// lags the barriers the read was released past ([Reader]).
func checkpointIn(ctx context.Context, tx *sql.Tx) (statelog.Checkpoint, error) {
	cp, _, err := statelog.CheckpointOf(ctx, inTx{tx}, topics.CodeLogStream)
	return cp, err
}

// unpacked is a packed position on the code log.
func unpacked(packed int64) statelog.Position {
	return statelog.Position{
		Stream:     topics.CodeLogStream,
		Generation: uint32(packed / statelog.GenerationStride),
		Seq:        uint64(packed % statelog.GenerationStride),
	}
}

// checkFreshness is EVERY code read's one rule for its freshness: any level
// but an EMPTY one (a cache's sync and a git request each need a specific one;
// [ReadQuery.freshness] fills the REST path's), and no session read naming no
// code-log position, which would wait for nothing. A floor on ANOTHER log is
// refused first ([statelog.ErrForeignPosition]), so a session read naming only
// such a floor is told which floor is misplaced.
func checkFreshness(f statelog.Freshness) error {
	switch {
	case f.Level == "":
		return fmt.Errorf("%w: a code read states its freshness", ErrBadLevel)
	case !f.Level.Valid():
		return fmt.Errorf("%w: %q is not a read level", ErrBadLevel, f.Level)
	}
	if err := f.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("gitlet: this read's floor: %w", err)
	}
	if f.Level == statelog.ReadSession && f.Floor(topics.CodeLogStream).IsZero() {
		return fmt.Errorf("%w: a session read names the position it must include", ErrBadLevel)
	}
	return nil
}

// resolver is the scope a read naming a repository by NAME is about, read in
// the read's own transaction: its name claim, and the repository the claim
// names when there is one.
func resolver(name string) statelog.Resolver {
	token := NameToken(name)
	return func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		var id string
		err := tx.QueryRowContext(ctx, claimSQL, token).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return statelog.ScopeSet{Paths: []string{NamePath(token)}}, nil
		case err != nil:
			return statelog.ScopeSet{}, fmt.Errorf("gitlet: resolve %q: %w", name, err)
		}
		return statelog.ScopeSet{Paths: []string{NamePath(token), RepoPath(id)}}, nil
	}
}

// claimIn is the repository a name names in tx.
func claimIn(ctx context.Context, tx *sql.Tx, name string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, claimSQL, NameToken(name)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("gitlet: read the claim on %q: %w", name, err)
	}
	return id, true, nil
}

// repoView is a repository's row as the reads serve it.
type repoView struct {
	ref               RepoRef
	branches, tags    int64
	tombstones        int64
	tombstonesThrough int64
	// retiredThrough is the highest position a retire_sweep removed the
	// repository's retired rows at or below: below it, the rows that told of
	// a retirement may be gone.
	retiredThrough int64
	// layersPosition is the position of the last record that changed the
	// repository's published chain: a delta from below it serves the chain.
	layersPosition int64
	pushes         int64
	lastPushAt     sql.NullInt64
	createdAt      int64
}

// summary is the view as a listing renders it.
func (v repoView) summary(head string) RepoSummary {
	s := RepoSummary{RepoRef: v.ref, Head: head, Pushes: v.pushes}
	if v.lastPushAt.Valid {
		s.LastPushAt = store.DecodeTime(v.lastPushAt.Int64)
	}
	return s
}

// activityAt is a repository's idle clock as a read serves it: the instant its
// row holds, or none where it holds none.
func activityAt(encoded int64) time.Time {
	if encoded == 0 {
		return time.Time{}
	}
	return store.DecodeTime(encoded)
}

// namedIn reads the repository a name names in tx, at the checkpoint cp — and
// where the name is an OLD one still redirecting to it after a rename, says so
// ([RepoRef.Redirect]): the repository's current name and when the redirect
// ends. False where nothing holds the name, or its row is gone.
func namedIn(ctx context.Context, tx *sql.Tx, name string, cp statelog.Position) (repoView, bool, error) {
	id, redirect, found, err := claimRowIn(ctx, tx, NameToken(name))
	if err != nil || !found {
		return repoView{}, false, err
	}
	v, found, err := repoViewIn(ctx, tx, id, cp)
	if err != nil || !found {
		return repoView{}, false, err
	}
	if redirect.Valid && v.ref.Name != name {
		v.ref.Redirect, v.ref.RedirectUntil = v.ref.Name, store.DecodeTime(redirect.Int64)
	}
	return v, true, nil
}

// repoViewIn reads one repository and its rules as a push is judged by them
// ([pushRulesIn]: no gate) in tx, at the checkpoint cp. A decide that rewrites
// the rules reads them whole beside it ([protectionsIn]).
func repoViewIn(ctx context.Context, tx *sql.Tx, id string, cp statelog.Position) (repoView, bool, error) {
	var (
		v                                                 repoView
		status, managed, kind, owners, visibility, search string
		deleted                                           sql.NullInt64
		active                                            int64
	)
	err := tx.QueryRowContext(ctx, repoViewSQL, id).Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &kind,
		&owners, &v.ref.Owner.Ref, &visibility, &search, &v.ref.Mirrored, &v.ref.DefaultBranch,
		&v.ref.Description, &deleted, &active, &v.ref.Refs, &v.branches, &v.tags, &v.tombstones,
		&v.ref.LivePacks, &v.ref.LiveBytes,
		&v.ref.LiveObjects, &v.ref.DropEpoch, &v.tombstonesThrough, &v.retiredThrough, &v.layersPosition, &v.pushes,
		&v.lastPushAt, &v.createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoView{}, false, nil
	case err != nil:
		return repoView{}, false, fmt.Errorf("gitlet: read repository %s: %w", id, err)
	}
	v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
	v.ref.Kind, v.ref.Visibility, v.ref.Search = RepoKind(kind), Visibility(visibility), SearchMode(search)
	v.ref.LastActivityAt = activityAt(active)
	if deleted.Valid {
		v.ref.DeletedAt = store.DecodeTime(deleted.Int64)
	}
	if v.ref.Protections, err = pushRulesIn(ctx, tx, id); err != nil {
		return repoView{}, false, err
	}
	v.ref.Position = cp
	return v, true, nil
}

// Resolve answers the repository a name names, deleted and purging ones
// included: the caller answers such a one with [RepoGone]'s sentence (when its
// purge MAY BEGIN, or that nothing brings it back), never a "restorable until"
// deadline nothing holds the purge to.
//
// AN OLD NAME a rename left redirecting answers the repository it redirects
// to, with [RepoRef.Redirect] naming its current name (a clone or fetch is
// served, a push refused naming the new name), until the old name's row is
// released.
func (r *Reader) Resolve(ctx context.Context, q ResolveQuery) (RepoRef, error) {
	if err := checkFreshness(q.Fresh); err != nil {
		return RepoRef{}, err
	}
	if !ValidName(q.Name) {
		return RepoRef{}, refusedf("%w: name: %q is not a repository name", ErrInvalid, q.Name)
	}
	var out RepoRef
	query := q.Fresh.Resolved(resolver(q.Name), false)
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		v, found, err := namedIn(ctx, tx, q.Name, cp.At)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Name)
		}
		out = v.ref
		return nil
	})
	if err != nil {
		return RepoRef{}, err
	}
	return out, nil
}

// Read answers a person's, a seat's or a surface's read — a listing of
// repositories, one repository in detail, one repository's push history or
// its refs — and the engine's own reads, at the level it is asked for
// ([ReadQuery.freshness]): `session` only beside the position on the code log
// it must include, and stale where a principal's read or a duty listing names
// none.
func (r *Reader) Read(ctx context.Context, q ReadQuery) (ReadAnswer, error) {
	if err := q.validate(); err != nil {
		return ReadAnswer{}, err
	}
	if err := q.daysKept(r.now()); err != nil {
		return ReadAnswer{}, err
	}
	fresh, levelErr := q.freshness()
	if levelErr != nil {
		return ReadAnswer{}, levelErr
	}
	// THE ONE CHART every row is judged by, read before the transaction (a
	// listing judged against a chart swapped mid-page answers a page no epoch
	// held) and before any row ([Reader.chartFor]).
	chart, chartErr := r.chartFor(q.Asker, q.judgesReadiness())
	if chartErr != nil {
		return ReadAnswer{}, chartErr
	}
	if q.Kind == ReadChanges && q.textLeads() && r.words == nil {
		return ReadAnswer{}, fmt.Errorf("%w: this node keeps no index of changes' words", ErrNotServing)
	}
	var (
		out   ReadAnswer
		query statelog.Query
		serve func(*sql.Tx, statelog.Position) error
	)
	switch q.Kind {
	case ReadRepos:
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.repos(ctx, tx, q, chart, cp, &out) }
	case ReadRepo:
		query = fresh.Resolved(resolver(q.Repo), false)
		serve = func(tx *sql.Tx, cp statelog.Position) error {
			return r.repo(ctx, tx, q, chart, cp, &out)
		}
	case ReadPushes:
		query = fresh.Resolved(resolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.pushes(ctx, tx, q, chart, &out) }
	case ReadRunPushes:
		// A POINT READ OF EVERY REPOSITORY THE RUN WAS GIVEN: past a deferred
		// record it would say the run delivered less than it did, so it
		// refuses and the engine asks another node.
		paths := make([]string, len(q.RunPushes.Repos))
		for i, id := range q.RunPushes.Repos {
			paths[i] = RepoPath(id)
		}
		query = fresh.Query(statelog.ScopeSet{Paths: paths}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.runPushes(ctx, tx, q, &out) }
	case ReadRefs:
		// A FIRST PAGE IS A POINT READ, as it carries the repository's detail;
		// a later page is the listing's, a set.
		query = fresh.Resolved(resolver(q.Repo), q.Cursor != "")
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.refsListing(ctx, tx, q, chart, cp, &out) }
	case ReadChanges:
		// A SET READ of the named repository or of the domain; one that
		// resolves its repository is a point read of it ([ReadQuery.Resolve]).
		if q.Repo != "" {
			query = fresh.Resolved(resolver(q.Repo), !q.Resolve)
		} else {
			query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		}
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.changes(ctx, tx, q, chart, cp, &out) }
	case ReadChange:
		// A FIRST PAGE IS A POINT READ of the repository, carrying the detail;
		// a later page or a conversation part is a set (its records scope the
		// repository's path too), unless it resolves the repository
		// ([ReadQuery.Resolve]).
		query = fresh.Resolved(resolver(q.Repo), !q.Resolve && (q.Cursor != "" || !q.pagesDetail()))
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.change(ctx, tx, q, chart, cp, &out) }
	case ReadChangeRouting:
		// A SET READ of the repository: every record about a change scopes its
		// path, so the repository's deferrals order its notices too.
		query = fresh.Resolved(resolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.changeRouting(ctx, tx, q, chart, &out) }
	case ReadReviews:
		// A SET READ OF THE DOMAIN, as a listing of changes across
		// repositories is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.reviewsGiven(ctx, tx, q, chart, &out) }
	case ReadCommit:
		// A POINT READ: past a deferred record a commit's page would name less
		// than it belongs to. Its gates' checks and approvals, on other paths,
		// are read as their rows stand.
		query = fresh.Resolved(resolver(q.Repo), false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.commit(ctx, tx, q, chart, cp, &out) }
	case ReadIntroduced:
		// A POINT READ OF THE REPOSITORY: an index built past a deferred record
		// would silently miss its pushes, so it refuses and the backfill asks
		// again.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.Introduced.RepoID)}}, false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.introduced(ctx, tx, q, cp, &out) }
	case ReadCompactionDue:
		// A SET READ OF THE DOMAIN: the compaction that follows is decided
		// again from the committer's own snapshot. So are the duty listings
		// below.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.compactionDue(ctx, tx, q, &out) }
	case ReadPublicationDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.publicationDue(ctx, tx, q, &out) }
	case ReadExpungeDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.expungeDue(ctx, tx, q, &out) }
	case ReadImportsDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.importsDue(ctx, tx, q, &out) }
	case ReadMirrorsDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.mirrorsDue(ctx, tx, q, &out) }
	case ReadChangesDue:
		// A SET READ, of the domain or one repository (changesdue.go).
		query = q.changesDueQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.changesDue(ctx, tx, q, &out) }
	case ReadIndexed:
		// A SET READ OF THE DOMAIN: the indexer reads each repository from its
		// own copy, and a deferred mode change is read at the next poll.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.indexed(ctx, tx, q, &out) }
	case ReadDefaultTips:
		// A SET READ OF THE DOMAIN: the scan rereads each tip from the
		// repository's own rows.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.defaultTips(ctx, tx, q, &out) }
	case ReadChecks:
		// A SET READ, of the repository it names or of the domain for the
		// window (checksread.go).
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.checks(ctx, tx, q, chart, cp, &out) }
	case ReadCheck:
		// A POINT READ of the repository's checks.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.check(ctx, tx, q, chart, &out) }
	case ReadChecksDue:
		// A SET READ, as changes_due's: the checks writer decides the dispatch
		// again.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.dueCheckWork(ctx, tx, q, &out) }
	case ReadCheckBox:
		// A POINT READ: a box is never admitted by an attempt read around a
		// deferred record.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.checkBox(ctx, tx, q, &out) }
	case ReadChecksQueued:
		// A SET READ OF THE DOMAIN: the checks writer decides each claim again,
		// refusing one another runner holds.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.queuedChecks(ctx, tx, q, &out) }
	case ReadChecksRunning:
		// A SET READ: the checks writer decides each ending again.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.runningChecks(ctx, tx, q, &out) }
	case ReadQueue, ReadLandings, ReadGreen:
		// A SET READ, of the repository it names or of the domain
		// (queueread.go).
		query = q.queueQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error {
			switch q.Kind {
			case ReadLandings:
				return r.landings(ctx, tx, q, chart, &out)
			case ReadGreen:
				return r.greens(ctx, tx, q, chart, &out)
			}
			return r.queueListing(ctx, tx, q, chart, &out)
		}
	case ReadInbox:
		// A SET READ OF THE DOMAIN: notices come from every change's subject
		// and marks from the person's own (inboxread.go).
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.inbox(ctx, tx, q, chart, &out) }
	case ReadNoticeCards:
		// A SET READ OF THE DOMAIN, as an `inbox` read is: the changes it
		// names lie on any repository's subjects (inboxread.go).
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.noticeCards(ctx, tx, q, chart, &out) }
	case ReadCommitMarks:
		// A SET READ, as a listing of its checks is: a deferred attempt or change is
		// read as its row stands, so a history page polled each minute is not refused.
		query = fresh.Resolved(checksResolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.commitMarks(ctx, tx, q, chart, &out) }
	case ReadCounts:
		// A SET READ, of the repository it names or of the domain, as a listing of changes is.
		if q.Repo != "" {
			query = fresh.Resolved(resolver(q.Repo), true)
		} else {
			query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		}
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.counts(ctx, tx, q, chart, &out) }
	case ReadExpungeTips:
		// A POINT READ OF THE REPOSITORY: past a deferred drop an expunge
		// would drop an object the drop's grace keeps, so it refuses.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.ExpungeTips.RepoID)}}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.expungeTips(ctx, tx, q, &out) }
	case ReadMirrorPush:
		// A POINT READ OF THE REPOSITORY: past a deferred record it could
		// publish a commit its checks never passed, or to a target no longer
		// named, so it refuses.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.MirrorPush.RepoID)}}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.mirrorPush(ctx, tx, q, &out) }
	case ReadBranchChanges:
		// A POINT READ OF EVERY REPOSITORY IT NAMES: past a deferred record it
		// could name a delivered ref by a change it no longer heads, so it
		// refuses.
		paths := make([]string, len(q.BranchChanges.Repos))
		for i, repo := range q.BranchChanges.Repos {
			paths[i] = RepoPath(repo.RepoID)
		}
		query = fresh.Query(statelog.ScopeSet{Paths: paths}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.branchChanges(ctx, tx, q, &out) }
	}
	answer, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = ReadAnswer{Position: cp.At}
		return serve(tx, cp.At)
	})
	if err != nil {
		return ReadAnswer{}, err
	}
	out.Level = answer.Level
	return out, nil
}
