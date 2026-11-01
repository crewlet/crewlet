package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE REF LIFECYCLE (M3-decisions M3-O62, M3-O64): the code-retention pass's
// LAST step deletes every branch a seat set in its own namespace and nobody
// has set for [RefExpiry] — through a push on the engine's own account
// ([PrincipalSystem], [ViaExpiry]) to the repository's committer, never a
// write of its own, so a deletion is decided against every push and hold on
// the repository's one subject and lands in its push history like any other:
// the row keeps each branch's last commit (its command's Old), from which
// code_create_branch restores it while the repository still holds that
// commit. The row is kept for the push history's 181 days, but the commit is
// held only for the deletion's [PruneGrace] at least: the row names where
// the branch was, never a promise that its commit is still there.
//
// Last, after the scratch expiry, on the pass's pacer and within its budget:
// a purge and a release free names, an expiry only room, which
// code_refs_near_limit says when it runs short.
//
// # The order is the safety argument
//
// The scratch expiry's: the CUTOFF is taken once, before anything is read;
// each PAGE is read with the code log's position inside its own transaction;
// and only THEN are the live turns' holds read, for that page's refs. A hold
// the pass could not see was therefore taken after its page was fixed, and
// either the branch was set within [RefHoldRecordAfter] when it was taken —
// later than the cutoff, which the decide's window refuses — or the turn
// RECORDED its hold on the repository's subject past the page's position,
// which the decide's held term refuses ([ExpiryRule]). The decide judges
// every command again in its own snapshot, so a branch set, held, made the
// default or put under review since its page is kept.
//
// # A retry is safe by state, never by id
//
// A branch an earlier proposal already deleted reads gone, so its command is
// a noop (rule 3); one pushed meanwhile is stale (rule 4); one held since its
// page is stale (4b) — so every proposal takes a fresh push id, and no ledger
// of expiry ids is kept.

// expire deletes, a page at a time, oldest first, every seat branch idle past
// [RefExpiry] that no live turn holds — never on an unknown — until none is
// left, the pass's budget runs out or a busy committer is met (stop), or the
// context ends (the error).
//
// A HOLD THAT CANNOT BE READ ends the step for this pass with nothing more
// expired: [RetentionReport.ExpiryUnread] says why, and a WARN line names the
// page. A page whose every ref is in an archived or deleted repository asks
// no hold at all: nothing of it is proposed.
func (r *Retention) expire(ctx context.Context, now time.Time, pace *pacer, report *RetentionReport,
	log *slog.Logger) (stop bool, err error) {

	q := idleRefsQuery{cutoff: now.Add(-RefExpiry), limit: MaxCommandsPerPush}
	for {
		page, err := r.Reader.idleSeatRefs(ctx, q)
		if err != nil {
			return false, fmt.Errorf("code: list the seat branches idle since %s: %w",
				q.cutoff.UTC().Format(time.RFC3339), err)
		}
		if len(page.refs) == 0 {
			return false, nil
		}
		// THE HOLDS, AFTER THE PAGE — of an ACTIVE repository's refs alone,
		// since nothing else of the page is proposed.
		var keys []string
		for _, ref := range page.refs {
			if page.repos[ref.repoID].status == RepoStatusActive {
				keys = append(keys, RefHoldKey(ref.repoID, ref.ref))
			}
		}
		held := map[string]bool{}
		if len(keys) > 0 {
			if held, err = r.Holds(ctx, keys); err != nil {
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				report.ExpiryUnread = fmt.Sprintf("which branches live turns hold could not be read (%v); no "+
					"branch was expired this pass", err)
				log.WarnContext(ctx, "code_ref_expiry_skipped", "error", err.Error(), "repo",
					page.repos[page.refs[0].repoID].name, "first", page.refs[0].ref, "page", len(page.refs),
					"detail", "a branch whose holds are unknown is never expired; the next pass asks again")
				return false, nil
			}
		}
		for _, id := range page.order {
			if stop, err := r.expireIn(ctx, now, id, page, held, pace, report, log); err != nil || stop {
				return stop, err
			}
		}
		if len(page.refs) < q.limit {
			return false, nil
		}
		q.after = &page.refs[len(page.refs)-1]
	}
}

// expireIn proposes the deletion of repository id's refs of the page — those
// no live turn holds (counted [RetentionReport.RefsHeld]) but its default
// branch (kept uncounted; [ExpiryRule] keeps it too, should the default move
// to it after the page) — as one push or as several, each cut by the
// domain's own bounds ([expiryCut]), and counts what became of each. An
// archived repository is read-only and keeps its branches; a deleted or
// purging one is the purge's: neither is proposed anything.
func (r *Retention) expireIn(ctx context.Context, now time.Time, id string, page idleRefsPage,
	held map[string]bool, pace *pacer, report *RetentionReport, log *slog.Logger) (stop bool, err error) {

	repo := page.repos[id]
	if repo.status != RepoStatusActive {
		return false, nil
	}
	var cmds []Command
	for _, ref := range page.refs {
		switch {
		case ref.repoID != id:
		case held[RefHoldKey(id, ref.ref)]:
			report.RefsHeld++
		case ref.ref == branchPrefix+repo.defaultBranch:
		default:
			cmds = append(cmds, Command{Ref: ref.ref, Old: ref.target, New: ZeroOID})
		}
	}
	for len(cmds) > 0 {
		p := Proposal{PushID: uuid.Must(uuid.NewV7()).String(), RepoID: id,
			Principal: Principal{Kind: PrincipalSystem}, Via: ViaExpiry, Node: r.Node, ReceivedAt: now,
			Seen: page.through, DropEpoch: repo.dropEpoch}
		n, err := expiryCut(p, cmds)
		if err != nil {
			report.failExpiry(repo.name, err.Error())
			return false, nil
		}
		p.Commands, cmds = cmds[:n], cmds[n:]
		switch turn, err := pace.wait(ctx); {
		case err != nil:
			return false, err
		case !turn:
			return true, nil
		}
		began := pace.clock()
		res, err := r.Propose(ctx, p)
		pace.took(began)
		switch {
		case err != nil && ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil:
			report.failExpiry(repo.name, err.Error())
			return false, nil
		}
		var ok, ng int
		for _, c := range res.Answer.Commands {
			switch c.Result {
			case ResultOK:
				ok++
			case ResultNG:
				ng++
			}
		}
		switch {
		case res.Outcome == statelog.OutcomeApplied:
			report.RefsExpired += ok
			report.RefsKept += ng
			log.InfoContext(ctx, "code_refs_expired", "repo", repo.name, "push_id", p.PushID, "expired", ok,
				"kept", ng, "position", res.Position.String())
		case res.Outcome != "":
			report.failExpiry(repo.name, fmt.Sprintf("outcome %s (op %s): what is still there is listed "+
				"again by the next pass", res.Outcome, res.OpID))
		case res.Answer.Refused == "":
			// NO RECORD, AND NO REFUSAL OF THE WHOLE PUSH: every branch of
			// it kept or already gone — pushed, held or reviewed since the
			// page — which is the decide doing its work, not a failure.
			report.RefsKept += ng
			log.DebugContext(ctx, "code_refs_expiry_kept", "repo", repo.name, "push_id", p.PushID, "kept", ng)
		case res.Answer.Refused == RefusalBusy:
			// THE COMMITTER HAS PERSONS' AND SEATS' PUSHES QUEUED, and an
			// expiry yields to them: the next pass resumes from the rows.
			return true, nil
		case res.Answer.Refused == RefusalRepoArchived, res.Answer.Refused == RefusalRepoDeleted:
			// THE LISTING RACED A LIFECYCLE RECORD: an archived repository
			// keeps its branches, and a deleted one is the purge's.
			log.DebugContext(ctx, "code_refs_expiry_skipped", "repo", repo.name, "refused",
				string(res.Answer.Refused))
			return false, nil
		default:
			report.failExpiry(repo.name, fmt.Sprintf("%s: %s", res.Answer.Refused, res.Answer.Detail))
			return false, nil
		}
	}
	return false, nil
}

// expiryCut is how many of cmds, in order, the expiry push p carries: at most
// [MaxCommandsPerPush] — a page never lists more — and as many as its entry
// holds in its record ([EntryRoom], [Proposal.Validate]'s [TooWideError]). A
// thousand plain refs take under half the room, but a ref whose characters
// the log spells in six bytes each (`&`, `<`, `>`) costs up to six times its
// length, so some six hundred such refs fill it — and an expiry refused
// TooWideError every pass would never expire its page. So it is cut as
// `crewlet code import` cuts its batches: by the domain's own rule, the whole
// list asked first, which is the answer for every page of plain refs. One
// command of the longest ref takes under two kibibytes, so a push always
// carries one; any other refusal is the step's own bug, answered as an error.
func expiryCut(p Proposal, cmds []Command) (int, error) {
	fits := func(n int) (bool, error) {
		p.Commands = cmds[:n]
		err := p.Validate()
		var wide *TooWideError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &wide):
			return false, nil
		}
		return false, err
	}
	if ok, err := fits(len(cmds)); ok || err != nil {
		return len(cmds), err
	}
	var failed error
	// THE FIRST LENGTH THAT DOES NOT FIT, less one: an entry only grows with
	// its commands.
	n := sort.Search(len(cmds), func(i int) bool {
		ok, err := fits(i + 1)
		if err != nil && failed == nil {
			failed = err
		}
		return !ok
	})
	switch {
	case failed != nil:
		return 0, failed
	case n == 0:
		return 0, fmt.Errorf("code: one deletion of %s alone is wider than one push's record may be",
			cmds[0].Ref)
	}
	return n, nil
}

// failExpiry records one repository's expiry failure in the report — beside
// the one it already holds for that name, as [RetentionReport.fail] does.
func (report *RetentionReport) failExpiry(name, sentence string) {
	if report.ExpiryFailed == nil {
		report.ExpiryFailed = map[string]string{}
	}
	if held, ok := report.ExpiryFailed[name]; ok {
		sentence = held + "; " + sentence
	}
	report.ExpiryFailed[name] = sentence
}

// idleRefsQuery is one page of the ref expiry's listing.
type idleRefsQuery struct {
	// cutoff is the pass's now less RefExpiry, fixed once.
	cutoff time.Time
	// after is the last ref of the page before; nil for the first.
	after *idleRef
	limit int
}

// idleRef is one seat branch the expiry may delete.
type idleRef struct {
	repoID, ref, target string
	at                  int64
}

// expiryRepo is what the step reads of a page's repository: its name, which
// the report names it by, the status that decides whether anything of it is
// proposed, its default branch, which is never, and its drop epoch, which its
// proposal carries as every push's does ([Proposal.DropEpoch]).
type expiryRepo struct {
	name, defaultBranch string
	status              RepoStatus
	dropEpoch           int64
}

// idleRefsPage is one page of the ref expiry's listing: its refs, oldest
// first; each repository they name, by id, and in the order the page first
// names it; and the position it was read at — the code log's checkpoint, read
// INSIDE the page's transaction — which every proposal formed from it carries
// as its Seen.
type idleRefsPage struct {
	refs    []idleRef
	repos   map[string]expiryRepo
	order   []string
	through statelog.Position
}

// idleSeatRefs lists one page of the refs a seat set in its own namespace
// whose `at` is at or before q.cutoff, oldest first and keyset after q.after,
// over code_refs_expiry_idx (replicated 0114) — with the page's repositories
// and the position it read them at, in the same transaction. A STALE READ:
// every deletion is decided again in its committer's own snapshot.
func (r *Reader) idleSeatRefs(ctx context.Context, q idleRefsQuery) (idleRefsPage, error) {
	if q.limit <= 0 {
		return idleRefsPage{}, fmt.Errorf("%w: a page of the idle seat branches lists at least one", ErrInvalid)
	}
	after := idleRef{at: math.MinInt64}
	if q.after != nil {
		after = *q.after
	}
	var page idleRefsPage
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		page = idleRefsPage{repos: map[string]expiryRepo{}}
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
		page.through = cp.At
		if err := page.readRefs(ctx, tx, q, after); err != nil {
			return err
		}
		return page.readRepos(ctx, tx)
	})
	if err != nil {
		return idleRefsPage{}, err
	}
	return page, nil
}

// readRefs reads the page's refs, and the order its repositories first appear
// in.
func (page *idleRefsPage) readRefs(ctx context.Context, tx *sql.Tx, q idleRefsQuery, after idleRef) error {
	rows, err := tx.QueryContext(ctx, expiryListingSQL, store.EncodeTime(q.cutoff), after.at, after.repoID,
		after.ref, q.limit)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var ref idleRef
		if err := rows.Scan(&ref.repoID, &ref.ref, &ref.target, &ref.at); err != nil {
			return err
		}
		page.refs = append(page.refs, ref)
		if !seen[ref.repoID] {
			seen[ref.repoID] = true
			page.order = append(page.order, ref.repoID)
		}
	}
	return rows.Err()
}

// readRepos reads the page's repositories, in one statement over the key,
// driven from the list.
func (page *idleRefsPage) readRepos(ctx context.Context, tx *sql.Tx) error {
	if len(page.order) == 0 {
		return nil
	}
	list, err := jsonList(page.order)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, expiryReposSQL, list)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, status string
		var repo expiryRepo
		if err := rows.Scan(&id, &repo.name, &status, &repo.defaultBranch, &repo.dropEpoch); err != nil {
			return err
		}
		repo.status = RepoStatus(status)
		page.repos[id] = repo
	}
	return rows.Err()
}

// The ref expiry's two statements, each registered in plan_test.go: the
// keyset page over its partial index, the predicate repeated verbatim and the
// >= beside the OR keeping the range seek (the reader's own rule), and the
// page's repositories by their key.
const (
	expiryListingSQL = `SELECT repo_id, ref, target, at FROM code_refs INDEXED BY code_refs_expiry_idx
		WHERE seat != '' AND at <= ?1 AND at >= ?2 AND (at > ?2 OR repo_id > ?3 OR (repo_id = ?3 AND ref > ?4))
		ORDER BY at, repo_id, ref LIMIT ?5`
	expiryReposSQL = `SELECT p.id, p.name, p.status, p.default_branch, p.drop_epoch
		FROM json_each(?) AS j CROSS JOIN code_repos AS p WHERE p.id = CAST(j.value AS TEXT)`
)
