package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// PushOutcome answers what became of one push, at session floored at the
// record's own position: "not found" is final only at or past it, so a node
// that cannot reach it refuses `behind` rather than answering early. Asked
// ([OutcomeQuery.Change]), it names the change the push opened from its
// options as well, by the id the push derives, in the same snapshot.
func (r *Reader) PushOutcome(ctx context.Context, q OutcomeQuery) (PushOutcome, error) {
	switch {
	case q.At.IsZero():
		return PushOutcome{}, refusedf("%w: at: a push's outcome is read at its record's position", ErrInvalid)
	case q.RepoID == "" || q.PushID == "":
		return PushOutcome{}, fmt.Errorf("%w: a push's outcome names its repository and its id", ErrInvalid)
	}
	query := statelog.FloorAt(statelog.ReadSession, q.At).Query(
		statelog.ScopeSet{Paths: []string{RepoPath(q.RepoID)}}, false)
	var out PushOutcome
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = PushOutcome{Position: cp.At}
		row, found, err := pushIn(ctx, tx, q.PushID)
		if err != nil || !found {
			return err
		}
		if row.RepoID != q.RepoID {
			return fmt.Errorf("%w: push %s is %s's, not %s's", ErrInvalid, q.PushID, row.RepoID, q.RepoID)
		}
		out.Found, out.Push = true, row
		if !q.Change {
			return nil
		}
		// THE CHANGE IT OPENED, by the id its push derives, in the
		// snapshot its row was read in.
		change, opened, err := changeRowIn(ctx, tx, PushChangeID(q.PushID))
		if err == nil && opened && change.repoID == q.RepoID {
			out.Opened = change.number
		}
		return err
	})
	if err != nil {
		return PushOutcome{}, err
	}
	return out, nil
}

// pushIn reads one push row.
func pushIn(ctx context.Context, tx *sql.Tx, pushID string) (PushRow, bool, error) {
	row, _, err := scanPush(tx.QueryRowContext(ctx, pushSQL, pushID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PushRow{}, false, nil
	case err != nil:
		return PushRow{}, false, fmt.Errorf("gitlet: read push %s: %w", pushID, err)
	}
	return row, true, nil
}

// scanner is a row or a row of rows.
type scanner interface{ Scan(dest ...any) error }

// scanPush reads a push row in pushColumns' order, and answers the length of
// its stored commands, so a page measures it without encoding it again. A row
// a NEWER build applied may store a field this build does not decode; its
// length is then the longer, and a page ends early rather than late.
//
// A ROW NAMING NO WAY THIS BUILD KNOWS IS REFUSED ([ErrInvalid]): the column's
// default is the empty string (replicated 0091), so an apply that forgot it
// fails here rather than reading as a client's push.
func scanPush(s scanner) (PushRow, int, error) {
	var (
		row                       PushRow
		position, at, received    int64
		via, commands, introduced string
	)
	if err := s.Scan(&row.PushID, &row.RepoID, &position, &at, &row.Actor, &row.ActorKind,
		&row.ActorSeat, &via, &row.Run.TurnID, &row.Run.WorkBackend, &row.Run.WorkKey, &row.Run.Model,
		&row.Run.RunID, &row.Run.Harness, &row.Node, &row.Atomic, &commands, &row.PackID, &row.PackBytes,
		&introduced, &row.IntroducedCount, &row.Offered, &received); err != nil {
		return PushRow{}, 0, err
	}
	if row.Via = PushVia(via); !row.Via.Valid() {
		return PushRow{}, 0, fmt.Errorf("%w: push %s's row names its way as %q, which is not one of %s",
			ErrInvalid, row.PushID, via, words.Or(PushVias()))
	}
	row.Position, row.At, row.ReceivedAt = unpacked(position), store.DecodeTime(at), store.DecodeTime(received)
	if err := json.Unmarshal([]byte(commands), &row.Commands); err != nil {
		return PushRow{}, 0, fmt.Errorf("gitlet: push %s's commands: %w", row.PushID, err)
	}
	if introduced != "" {
		row.Introduced = strings.Split(introduced, " ")
	}
	return row, len(commands), nil
}

// scanIntroduced reads a push row as the index of which push introduced each
// commit ([introducedOf]), and answers what serving it costs a page: the
// larger of the row it SENDS and the commands it READ.
//
// A page of condensed pushes is held to what it reads: a row stores every
// command its client sent, refused ones too ([RecordedRef]), while the
// condensed row sends only the landed refs, so a push of one branch beside 999
// refused long refs sends 656 bytes but reads 403 KB. Charged by what it sent,
// a page decoded gigabytes, past any estate read's attempt, on every retry.
// Nothing else in the row is wider than what it sends.
func scanIntroduced(rows *sql.Rows) (IntroducedRow, int, error) {
	pushed, commands, err := scanPush(rows)
	if err != nil {
		return IntroducedRow{}, 0, err
	}
	in := introducedOf(pushed)
	return in, max(in.encodedBytes(), commands), nil
}

// mirrorPush serves a `mirror_push` read ([ReadMirrorPush]): the repository's
// push mirror (none where it has none or is gone) and every green row, from
// one snapshot, each a primary-key seek: a home asks it once an attempt.
func (r *Reader) mirrorPush(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	id := q.MirrorPush.RepoID
	remotes, err := remotesIn(ctx, tx, id)
	if err != nil || remotes.to == nil {
		return err
	}
	c := MirrorCandidate{ID: id, To: remotes.mirrorTo()}
	if err = tx.QueryRowContext(ctx, repoNameSQL, id).Scan(&c.Name); err != nil {
		return fmt.Errorf("gitlet: read repository %s's name: %w", id, err)
	}
	green, err := greenRowsIn(ctx, tx, id)
	if err != nil {
		return err
	}
	out.Mirrors, out.Green = []MirrorCandidate{c}, green
	return nil
}

// branchChanges serves a `branch_changes` read ([ReadBranchChanges]): the live
// change each named branch heads, in the query's order, one seek of
// code_changes_open_idx a branch (liveBySourceSQL), in one transaction. NOT
// HELD TO A GRANT: the engine names its own run's refs.
func (r *Reader) branchChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	for _, repo := range q.BranchChanges.Repos {
		var heads []BranchChange
		if err := inChunks(repo.Refs, func(part []string) error {
			encoded, err := json.Marshal(part)
			if err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, liveBySourceSQL, repo.RepoID, string(encoded))
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				row, err := scanChange(rows.Scan)
				if err != nil {
					return err
				}
				heads = append(heads, BranchChange{RepoID: repo.RepoID, Ref: row.source, ChangeID: row.id,
					Number: row.number})
			}
			return rows.Err()
		}); err != nil {
			return fmt.Errorf("gitlet: read the live changes of repository %s's branches: %w", repo.RepoID, err)
		}
		order := make(map[string]int, len(repo.Refs))
		for i, ref := range repo.Refs {
			order[ref] = i
		}
		slices.SortFunc(heads, func(a, b BranchChange) int { return order[a.Ref] - order[b.Ref] })
		out.BranchChanges = append(out.BranchChanges, heads...)
	}
	return nil
}

// repos serves a listing of repositories, by name or by last push
// ([reposListed]), of those the asker and its ceiling may read ([JudgeRead]),
// each carrying the asker's access.
//
// THE PAGE IS WHAT WAS SCANNED, NOT WHAT WAS KEPT: the cursor is the last row
// scanned, so a page may be short or empty with a Next and a caller pages
// until no Next. Cut at kept rows, a page of unreadable repositories would
// end the listing early, or filling a page would scan without bound.
func (r *Reader) repos(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {
	statuses := q.Status
	if len(statuses) == 0 {
		// THE REPOSITORIES IN USE, importing ones included (fetched, filling as
		// the import lands); deleted and purging ones are asked for.
		statuses = []RepoStatus{RepoStatusImporting, RepoStatusActive, RepoStatusArchived}
	}
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = string(s)
	}
	list, err := jsonList(names)
	if err != nil {
		return err
	}
	limit := min(limitOr(q.Limit, defaultRepoLimit), r.pageRows)
	if len(q.DayBounds) > 0 {
		// The sparks' read is measured at this page at most, whatever the default.
		limit = min(limit, MaxSparkReposPerPage)
	}
	listed, err := reposListed(ctx, tx, q, list, limit)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(listed))
	cursors := make([]string, 0, len(listed))
	for _, l := range listed {
		g, refused := JudgeRead(q.Asker, q.CeilingSeat, l.v.ref, chart)
		if refused != nil {
			continue
		}
		l.v.ref.Position = cp
		s := l.v.summary(l.head)
		s.Access = g.Access.String()
		out.Repos = append(out.Repos, s)
		ids = append(ids, s.ID)
		cursors = append(cursors, l.cursor)
	}
	if err = protectionsOf(ctx, tx, ids, out.Repos); err != nil {
		return err
	}
	// ITS FACTS WHERE ASKED, read before the page is cut so it is measured with them, and
	// on a first page of a listing narrowed by its kind and statuses alone, its whole.
	if q.RepoFacts {
		facts := make([]*RepoSummary, len(out.Repos))
		for i := range out.Repos {
			facts[i] = &out.Repos[i]
		}
		if err = withFacts(ctx, tx, facts); err != nil {
			return err
		}
		if q.Cursor == "" && q.Prefix == "" && !q.Protected {
			if out.ReposTotal, err = r.reposTotal(ctx, tx, q, chart, statuses, list); err != nil {
				return err
			}
		}
	}
	// ITS PUSHES ON THE ASKER'S DAYS where DayBounds asks, before the cut so the page is measured with them.
	if len(q.DayBounds) > 0 {
		days, sparkErr := pushesOnDays(ctx, tx, ids, q.DayBounds)
		if sparkErr != nil {
			return sparkErr
		}
		for i := range out.Repos {
			out.Repos[i].Spark = days[out.Repos[i].ID]
		}
	}
	// MEASURED WITH THEIR RULES, most of a row's bytes when a repository has
	// many. The page is cut at the row that fills it, its cursor that row's;
	// otherwise full when the statement filled its limit, its cursor the last
	// row scanned. EACH ROW'S CARD is read as the page fills, its push rows
	// charged to the page, so a row past the cut reads none.
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for i := range out.Repos {
		if q.RepoFacts {
			if err = r.carded(ctx, tx, &out.Repos[i], &pg); err != nil {
				return err
			}
		}
		if pg.add(out.Repos[i].encodedBytes()) {
			out.Next = cursors[i]
			out.Repos = out.Repos[:i+1]
			return nil
		}
	}
	if len(listed) == limit {
		out.Next = listed[len(listed)-1].cursor
	}
	return nil
}

// protectionsOf fills a page of repositories' rules in ONE statement, each as
// a push is judged by it ([PushRule]): a listing answers no merge gate
// ([ReadQuery.Gates]), so its rows weigh what it sends.
func protectionsOf(ctx context.Context, tx *sql.Tx, ids []string, repos []RepoSummary) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, pushRulesOfSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the listing's protections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	at := make(map[string]int, len(repos))
	for i, s := range repos {
		at[s.ID] = i
	}
	for rows.Next() {
		var id, direct string
		var p PushRule
		if err := rows.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct); err != nil {
			return fmt.Errorf("gitlet: read the listing's protections: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		i := at[id]
		repos[i].Protections = append(repos[i].Protections, p)
	}
	return rows.Err()
}

// repo serves q's repository in detail, and the record that last moved it, to
// an asker and ceiling that may read it ([JudgeRead]): else [ErrNotPermitted]
// or [ErrAboveCeiling], judged after the row is read. A missing one is
// [ErrRepoNotFound]: a `visibility: unit` repository's existence is no secret
// (the company's document names it), only its contents are.
func (r *Reader) repo(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {
	name := q.Repo
	v, found, err := namedIn(ctx, tx, name, cp)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	id := v.ref.ID
	g, err := JudgeRead(q.Asker, q.CeilingSeat, v.ref, chart)
	if err != nil {
		return err
	}
	head, _, err := liveRefIn(ctx, tx, id, branchPrefix+v.ref.DefaultBranch)
	if err != nil {
		return fmt.Errorf("gitlet: read %s's head: %w", name, err)
	}
	// THE COUNTS ARE THE ROW'S, kept by the applier, never a walk of every ref
	// on every detail read.
	summary := v.summary(head.OID)
	summary.Access = g.Access.String()
	out.Repo = &RepoDetail{RepoSummary: summary,
		Branches: v.branches, Tags: v.tags, Tombstones: v.tombstones,
		CreatedAt: store.DecodeTime(v.createdAt), Limits: r.limits()}
	// THE PATH OWNERS ONLY TO A READ THAT ASKS ([ReadQuery.PathOwners]): at
	// their bound a quarter MiB that every resolution would carry and drop.
	if q.PathOwners {
		if out.Repo.PathOwners, err = pathOwnersIn(ctx, tx, id); err != nil {
			return err
		}
	}
	// AND THE GATES ONLY TO A READ THAT ASKS ([ReadQuery.Gates]), with who
	// set each rule and when, in one seek of the rules ([ruleDetailsIn]) in
	// the transaction the row's rules came from, so every gate names one.
	if q.Gates {
		if out.Repo.Gates, out.Repo.RuleSources, err = ruleDetailsIn(ctx, tx, id); err != nil {
			return err
		}
	}
	// ITS PUSHES ON THE ASKER'S DAYS AND WHO PUSHED, where DayBounds asks beside RepoFacts.
	if len(q.DayBounds) > 0 {
		var days map[string][]int64
		if days, err = pushesOnDays(ctx, tx, []string{id}, q.DayBounds); err != nil {
			return err
		}
		out.Repo.Spark = days[id]
		if out.Repo.Pushers, err = pushersIn(ctx, tx, id, q.DayBounds, MaxPushersAnswered); err != nil {
			return err
		}
	}
	// AND ITS FACTS ONLY TO A READ THAT ASKS ([ReadQuery.RepoFacts]).
	if q.RepoFacts {
		if err = withFacts(ctx, tx, []*RepoSummary{&out.Repo.RepoSummary}); err != nil {
			return err
		}
		if err = r.carded(ctx, tx, &out.Repo.RepoSummary, nil); err != nil {
			return err
		}
	}
	// AND ITS EXPUNGES' AND OTHER HOSTS' PROGRESS (a purge, an import, a
	// mirror's last sync), in one seek of its row ([detailRowIn]).
	out.Repo.Expunge, out.Repo.Remotes, out.Repo.Version, err = detailRowIn(ctx, tx, id)
	return err
}

// detailRowIn reads what a repository's detail serves of its row beyond its
// summary (its expunges [expungeState.view], its other hosts [RemoteState]
// and the record that last moved it) in ONE primary-key statement, since
// every resolution of a repository reads this detail. The applier's and the
// decide's narrower reads ([expungeStateSQL], [remotesSQL]) stay their own.
func detailRowIn(ctx context.Context, tx *sql.Tx, id string) (ExpungeState, RemoteState, statelog.Position,
	error) {
	var (
		state                             expungeState
		remotes                           RemoteState
		from, to, source, failed          string
		chunks, doneAt, syncedAt, version int64
	)
	dests := append(state.dests(), &from, &to, &source, &chunks, &doneAt, &failed, &syncedAt, &version)
	if err := tx.QueryRowContext(ctx, detailRowSQL, id).Scan(dests...); err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's detail row: %w", id, err)
	}
	var err error
	if remotes.MirrorFrom, err = remoteOf[MirrorSource](from); err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's mirror_from: %w", id, err)
	}
	if remotes.MirrorTo, err = remoteOf[MirrorTarget](to); err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's mirror_to: %w", id, err)
	}
	imported, err := remoteOf[ImportSource](source)
	if err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's import_source: %w", id, err)
	}
	if imported != nil {
		remotes.Import = &ImportState{Source: *imported, Chunks: chunks, Error: failed}
		if doneAt != 0 {
			remotes.Import.DoneAt = store.DecodeTime(doneAt)
		}
	}
	if syncedAt != 0 {
		remotes.MirrorSyncedAt = store.DecodeTime(syncedAt)
	}
	return state.view(), remotes, unpacked(version), nil
}

// detailRowSQL reads what [detailRowIn] serves, by the primary key: the
// expunge columns first, in [expungeStateSQL]'s order ([expungeState.dests]),
// then the other hosts', then the row's version — registered in plan_test.go.
const detailRowSQL = `SELECT drop_epoch, expunge_id, expunge_request, expunge_request_position,
	expunged_at, expunge_dropped_objects, expunge_dropped_bytes, mirror_from, mirror_to, import_source,
	import_seq, imported_at, import_error, mirror_synced_at, version FROM code_repos WHERE id = ?`

// pushes serves one repository's push history, newest first IN LOG ORDER,
// keyset-paged by (position, push_id): group commit puts up to 64 pushes in
// one record sharing its position, and the broker's instant steps back across
// a leader change (replicated 0084). The history is the repository's contents,
// refused as [Reader.repo] refuses. Each row names the change its record
// merged ([PushRow.Change]); a narrowed history is [Reader.pushesNarrowed]'s.
func (r *Reader) pushes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	id := repo.ref.ID
	if q.Push != "" {
		// ONE PUSH, WHOLE, by its key, and only one this history holds;
		// another repository's is an answer with none.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		row, found, err := pushIn(ctx, tx, q.Push)
		if err != nil {
			return fmt.Errorf("gitlet: read %s's push %s: %w", q.Repo, q.Push, err)
		}
		if found && row.RepoID == id {
			rows := []PushRow{row}
			var named mergedNames
			named.add(0, row)
			if _, err = named.name(ctx, tx, repo.ref, rows); err != nil {
				return err
			}
			out.Pushes = rows
		}
		return nil
	}
	// THE REPOSITORY'S PUSHES ON THE ASKER'S DAYS where DayBounds asks, whatever the page holds.
	if len(q.DayBounds) > 0 {
		var days map[string][]int64
		if days, err = pushesOnDays(ctx, tx, []string{id}, q.DayBounds); err != nil {
			return err
		}
		out.PushDays = days[id]
	}
	if q.PushFilter != nil {
		return r.pushesNarrowed(ctx, tx, q, repo.ref, out)
	}
	limit := limitOr(q.Limit, defaultPushLimit)
	var rows *sql.Rows
	if q.Cursor == "" {
		rows, err = tx.QueryContext(ctx, pushesFirstSQL, id, limit)
	} else {
		position, pushID, ok := cutHistoryCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, pushesAfterSQL, id, position, position, pushID, limit)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	var (
		named   mergedNames
		charges []int
	)
	for rows.Next() {
		var (
			row      PushRow
			commands int
		)
		if row, commands, err = scanPush(rows); err != nil {
			return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
		}
		named.add(len(out.Pushes), row)
		// A ROW IN BRIEF IS CHARGED THE ROW IT READ, or what it sends if
		// larger: charged by what it sends alone, a page of maximal pushes
		// would read without bound ([scanIntroduced]).
		charge := row.encodedBytes(commands)
		if q.Brief {
			row = row.brief()
			charge = max(charge, row.encodedBytes(row.commandsBytes()))
		}
		out.Pushes = append(out.Pushes, row)
		charges = append(charges, charge)
		if pg.add(charge) {
			out.Next = logCursor(row.Position.Packed(), row.PushID)
			break
		}
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
	}
	grew, err := named.name(ctx, tx, repo.ref, out.Pushes)
	if err != nil {
		return err
	}
	if kept := recut(charges, grew, limit, r.pageBytes); kept < len(out.Pushes) {
		out.Pushes = out.Pushes[:kept]
		last := out.Pushes[kept-1]
		out.Next = logCursor(last.Position.Packed(), last.PushID)
	}
	return nil
}

// refsListing serves one page of a repository's LIVE refs under a prefix, by
// name or as a [RefListingQuery] orders them, keyset after the last ref
// served, to an asker and ceiling that may read it ([JudgeRead]); the first
// page carries the repository's detail too, in the same transaction, so a
// seat's first question is one read. The detail rides in the half of the
// estate reply MaxPageBytes leaves free.
//
// OVER code_refs_live_idx (replicated 0082, its predicate repeated verbatim),
// never code_refs' key, whose range also holds up to MaxTombstonesPerRepo
// tombstones to step over. A directory's names lie between dir + "/" and
// dir + "0", the push decide's range.
func (r *Reader) refsListing(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {

	prefix := cmp.Or(q.Prefix, branchPrefix)
	after, err := q.refsAfter(prefix)
	if err != nil {
		return err
	}
	var id string
	if q.Cursor == "" {
		if err = r.repo(ctx, tx, q, chart, cp, out); err != nil {
			return err
		}
		id = out.Repo.ID
	} else {
		var found bool
		if id, found, err = claimIn(ctx, tx, q.Repo); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		if err = mayReadIn(ctx, tx, id, q.Asker, q.CeilingSeat, chart); err != nil {
			return err
		}
	}
	// THE PAGE IS refslisting.go's, by name where nothing orders it otherwise.
	if q.RefListing == nil {
		q.RefListing = &RefListingQuery{}
	}
	return r.refsListed(ctx, tx, q, chart, cp, id, after, out)
}

// introduced serves one page of a repository's pushes in LOG ORDER for a Gitlet
// node's index of which push introduced each commit: (After, Through]
// ascending (Through zero: this read's checkpoint) or below Before descending;
// keyset `<packed position>/<push id>`, since group commit puts up to 64 pushes
// at one position. A page serves at least one push and ends at its row limit
// or MaxPageBytes, charged as [scanIntroduced] says. A purged repository is
// [ErrRepoNotFound], which drops the index that asked.
//
// ITS ROW LIMIT IS MaxSnapshotItems, never MaxPushesPerPage (a person's
// screen): its bytes bound its cost, and the narrowest pushes (345 bytes) fill
// about the same 4 MiB (internal/gitlet/cache's measured section), where 200 a
// page would read a monorepo's 181 days in 90 000 pages rather than 2 400.
func (r *Reader) introduced(ctx context.Context, tx *sql.Tx, q ReadQuery, cp statelog.Position,
	out *ReadAnswer) error {

	in := q.Introduced
	var name string
	switch err := tx.QueryRowContext(ctx, repoNameSQL, in.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, in.RepoID)
	case err != nil:
		return fmt.Errorf("gitlet: read repository %s: %w", in.RepoID, err)
	}
	through := in.Through.Packed()
	if in.Through.IsZero() {
		through = cp.Packed()
	}
	limit := min(limitOr(q.Limit, r.pageRows), r.pageRows)
	var rows *sql.Rows
	var err error
	switch position, pushID, keyed := cutLogCursor(q.Cursor); {
	case q.Cursor != "" && (!keyed || (in.Descending && position >= in.Before.Packed()) ||
		(!in.Descending && position <= in.After.Packed())):
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case in.Descending && q.Cursor == "":
		rows, err = tx.QueryContext(ctx, pushesBelowSQL, in.RepoID, in.Before.Packed(), limit)
	case in.Descending:
		rows, err = tx.QueryContext(ctx, pushesAfterSQL, in.RepoID, position, position, pushID, limit)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, pushesAboveSQL, in.RepoID, in.After.Packed(), through, limit)
	default:
		rows, err = tx.QueryContext(ctx, pushesAboveAfterSQL, in.RepoID, position, through, position, pushID,
			limit)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read %s's pushes in log order: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		row, cost, err := scanIntroduced(rows)
		if err != nil {
			return fmt.Errorf("gitlet: read %s's pushes in log order: %w", name, err)
		}
		out.Introduced = append(out.Introduced, row)
		if pg.add(cost) {
			out.Next = logCursor(row.Position.Packed(), row.PushID)
			break
		}
	}
	return rows.Err()
}

// runPushes serves one page of an executor's pushes ([ReadRunPushes]): each
// named repository in order, its pushes above the launch position in LOG
// ORDER, answering those naming the run; keyset after the last push READ,
// `<repository index>/<packed position>/<push id>`, the run's or not. NOT HELD
// TO A GRANT: the run was given only repositories its seat could read.
//
// A PAGE IS HELD TO WHAT IT READS: code_pushes_repo_idx carries no run
// (replicated 0098), so every push beside the run's is read to test it. A
// page reads at most MaxSnapshotItems pushes, stopping once their row_bytes
// WEIGH MaxPageBytes ([pushesWithin]'s rule), weighed by covering seeks
// ([runSpanOf]), never in Go; it may end with none kept and a cursor.
// BenchmarkRunPushes holds every page under a tenth of statelog.ReadBudget.
func (r *Reader) runPushes(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	in := q.RunPushes
	after := in.After.Packed()
	// A PAGE READS AFTER A PLACE: its cursor's push, or on a repository's
	// first page (after+1, ""), before every push past the launch, since no
	// push's id is empty.
	start, position, pushID := 0, after+1, ""
	if q.Cursor != "" {
		var keyed bool
		if start, position, pushID, keyed = cutRunPushCursor(q.Cursor); !keyed || start >= len(in.Repos) ||
			position <= after {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
	}
	reads := page{maxRows: min(limitOr(q.Limit, r.pageRows), r.pageRows), maxB: r.pageBytes}
	sent := 0
	for i := start; i < len(in.Repos); i++ {
		full, err := r.runPushesOf(ctx, tx, i, in, position, pushID, &reads, &sent, out)
		if err != nil || full {
			return err
		}
		position, pushID = after+1, ""
	}
	return nil
}

// runPushesOf reads the pushes of the run's i-th repository after the place
// (position, pushID), as many as reads still takes, adds those naming the run
// to out — what they send counted in sent — and reports whether the page is
// full, setting out.Next to the last push it READ: the one that filled its
// reads, or the run's push that filled what it sends.
func (r *Reader) runPushesOf(ctx context.Context, tx *sql.Tx, i int, in *RunPushesQuery, position int64,
	pushID string, reads *page, sent *int, out *ReadAnswer) (bool, error) {

	repo := in.Repos[i]
	span, err := runSpanOf(ctx, tx, repo, position, pushID, *reads)
	if err != nil {
		return false, fmt.Errorf("gitlet: weigh the pushes to %s a page of run %s's may read: %w", repo, in.Run,
			err)
	}
	if span.pushes == 0 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, runPushesInSQL, repo, in.Run, position, pushID, span.lastPosition,
		span.lastID)
	if err != nil {
		return false, fmt.Errorf("gitlet: read run %s's pushes to %s: %w", in.Run, repo, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, commands, err := scanPush(rows)
		if err != nil {
			return false, fmt.Errorf("gitlet: read run %s's pushes to %s: %w", in.Run, repo, err)
		}
		out.Pushes = append(out.Pushes, row)
		if *sent += row.encodedBytes(commands); *sent >= reads.maxB {
			out.Next = runPushCursor(i, row.Position.Packed(), row.PushID)
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read run %s's pushes to %s: %w", in.Run, repo, err)
	}
	reads.rows += span.pushes
	reads.bytes += span.weight
	if reads.full() {
		out.Next = runPushCursor(i, span.lastPosition, span.lastID)
		return true, nil
	}
	return false, nil
}

// runSpan is how far a page reads one repository's pushes: how many, what
// their rows weigh, and the last one's place in log order.
type runSpan struct {
	pushes, weight int
	lastPosition   int64
	lastID         string
}

// runSpanOf is the span of a repository's pushes after the place (position,
// pushID) that a page may still read, what it has read so far counted in
// reads, from code_pushes_repo_idx alone: as many as it has rows left, and
// none past the first whose weight fills its bytes (reads is the page's copy,
// so weighing moves nothing the page counts).
func runSpanOf(ctx context.Context, tx *sql.Tx, repo string, position int64, pushID string,
	reads page) (runSpan, error) {

	var span runSpan
	if err := tx.QueryRowContext(ctx, runWeighSQL, repo, position, pushID, reads.left()).Scan(&span.pushes,
		&span.weight); err != nil {
		return runSpan{}, err
	}
	if span.pushes == 0 {
		return span, nil
	}
	if reads.bytes+span.weight >= reads.maxB {
		// THEY WEIGH MORE THAN THE PAGE'S BYTES LEFT: their weights, in log
		// order, to the one that fills it.
		var err error
		if span.pushes, span.weight, err = runWeights(ctx, tx, repo, position, pushID, reads); err != nil {
			return runSpan{}, err
		}
	}
	err := tx.QueryRowContext(ctx, runLastSQL, repo, position, pushID, span.pushes-1).Scan(&span.lastPosition,
		&span.lastID)
	return span, err
}

// runWeights walks the weights of a repository's pushes after the place
// (position, pushID) in log order, counting each into reads — the page's copy
// — and answers how many it walked and what they weigh: up to the push that
// fills the page, or as many as it has rows left.
func runWeights(ctx context.Context, tx *sql.Tx, repo string, position int64, pushID string,
	reads page) (pushes, weight int, err error) {

	rows, err := tx.QueryContext(ctx, runWeightsSQL, repo, position, pushID, reads.left())
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var w int
		if err := rows.Scan(&w); err != nil {
			return 0, 0, err
		}
		pushes, weight = pushes+1, weight+w
		if reads.add(w) {
			break
		}
	}
	return pushes, weight, rows.Err()
}

// runPushCursor is "<repository index>/<packed position>/<push id>": a push's
// place among the pushes a run's read pages through — the repository of the
// run's query it was made to, then its place in that repository's log order,
// its [logCursor].
func runPushCursor(repo int, position int64, pushID string) string {
	return strconv.Itoa(repo) + "/" + logCursor(position, pushID)
}

// cutRunPushCursor reads a [runPushCursor].
func cutRunPushCursor(s string) (repo int, position int64, pushID string, ok bool) {
	index, rest, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, "", false
	}
	repo, err := strconv.Atoi(index)
	if err != nil || repo < 0 || strconv.Itoa(repo) != index {
		return 0, 0, "", false
	}
	position, pushID, ok = cutLogCursor(rest)
	return repo, position, pushID, ok
}

// ResolveRef answers the refs of one repository a query names, at its level,
// to an asker and ceiling that may read it ([JudgeRead]), in ONE transaction:
// a deleted or purging repository is [ErrRepoNotFound] in [RepoGone]'s
// sentence. Each ref resolves whole, a short name as a branch then a tag, and
// [HeadRef] as the default branch this transaction's row names, with the
// position that last moved it ([ResolvedRef.Moved]). A name that resolves to
// nothing is answered [ResolvedRef.Missing] beside the others, never failing
// them: whether a call goes on is its caller's to say.
func (r *Reader) ResolveRef(ctx context.Context, q RefQuery) (RefAnswer, error) {
	if err := checkFreshness(q.Fresh); err != nil {
		return RefAnswer{}, err
	}
	if !ValidName(q.Repo) {
		return RefAnswer{}, refusedf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if why := q.Asker.invalid(); why != "" {
		return RefAnswer{}, refusedf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return RefAnswer{}, fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	// EVERY NAME IS CHECKED BEFORE ANY READ: a query naming one no ref can
	// have is the asker's mistake, refused whole.
	switch n := len(q.Refs); {
	case n == 0:
		return RefAnswer{}, refusedf("%w: refs: a resolve names at least one ref", ErrInvalid)
	case n > MaxRefsPerQuery:
		return RefAnswer{}, refusedf("%w: refs: %d refs, past the %d one resolve reads", ErrInvalid, n,
			MaxRefsPerQuery)
	}
	tried := make([][]string, len(q.Refs))
	for i, ref := range q.Refs {
		if ref == HeadRef {
			continue // the default branch, which the row read below names
		}
		names, nameErr := refNames(ref)
		if nameErr != nil {
			return RefAnswer{}, nameErr
		}
		tried[i] = names
	}
	chart, chartErr := r.chartFor(q.Asker, false)
	if chartErr != nil {
		return RefAnswer{}, chartErr
	}
	var out RefAnswer
	query := q.Fresh.Resolved(resolver(q.Repo), false)
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = RefAnswer{}
		id, found, err := claimIn(ctx, tx, q.Repo)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		v, found, err := repoViewIn(ctx, tx, id, cp.At)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		if v.ref.Status == RepoStatusDeleted || v.ref.Status == RepoStatusPurging {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, RepoGone{ID: id, Name: v.ref.Name,
				Status: v.ref.Status, DeletedAt: v.ref.DeletedAt, ManagedBy: v.ref.ManagedBy}.Detail())
		}
		if _, err := JudgeRead(q.Asker, q.CeilingSeat, v.ref, chart); err != nil {
			return err
		}
		out = RefAnswer{Repo: v.ref, Refs: make([]ResolvedRef, len(q.Refs)), Position: cp.At}
		for i, names := range tried {
			said := ""
			if q.Refs[i] == HeadRef {
				names, said = []string{branchPrefix + v.ref.DefaultBranch}, ", its default branch"
			}
			resolved, err := firstLiveIn(ctx, tx, id, names)
			if err != nil {
				return fmt.Errorf("gitlet: read %s of %s: %w", q.Refs[i], q.Repo, err)
			}
			resolved.Name = q.Refs[i]
			if resolved.Ref == "" {
				resolved.Missing = fmt.Sprintf("repository %s holds no live %s%s", q.Repo, words.Or(names), said)
			}
			out.Refs[i] = resolved
		}
		return nil
	})
	if err != nil {
		return RefAnswer{}, err
	}
	return out, nil
}

// firstLiveIn is the first of names repository id holds as a live ref, read in
// tx — its whole name, the object it names and where it last moved — or a
// zero ResolvedRef where it holds none of them.
func firstLiveIn(ctx context.Context, tx *sql.Tx, id string, names []string) (ResolvedRef, error) {
	for _, name := range names {
		resolved, found, err := liveRefIn(ctx, tx, id, name)
		switch {
		case err != nil:
			return ResolvedRef{}, err
		case found:
			return resolved, nil
		}
	}
	return ResolvedRef{}, nil
}

// refNames is every ref one name of a [RefQuery] may be, in the order they
// are tried: a whole ref as itself; a short name as a branch, then as a tag —
// each only where git could hold it. A name that can be none is
// [ErrInvalid], refused before any read.
func refNames(ref string) ([]string, error) {
	if strings.HasPrefix(ref, "refs/") {
		if !ValidRef(ref) {
			return nil, refusedf("%w: ref: %q is not a ref", ErrInvalid, ref)
		}
		return []string{ref}, nil
	}
	var names []string
	if gitref.ValidBranch(ref) {
		names = append(names, branchPrefix+ref)
	}
	if ValidRef(tagPrefix + ref) {
		names = append(names, tagPrefix+ref)
	}
	if len(names) == 0 {
		return nil, refusedf("%w: ref: %q is no ref, branch or tag name", ErrInvalid, ref)
	}
	return names, nil
}

// liveRefIn is repository id's live ref by its whole name, read in tx: the
// object, the position and broker instant of the record that last moved it
// (apply_push.go writes them), and the seat that set it in its own namespace
// ([seatOf]), in one key lookup; or false where no live ref has the name.
func liveRefIn(ctx context.Context, tx *sql.Tx, id, ref string) (ResolvedRef, bool, error) {
	var (
		out       = ResolvedRef{Ref: ref}
		moved, at int64
	)
	switch err := tx.QueryRowContext(ctx, liveRefSQL, id, ref).Scan(&out.OID, &moved, &at, &out.Seat); {
	case errors.Is(err, sql.ErrNoRows):
		return ResolvedRef{}, false, nil
	case err != nil:
		return ResolvedRef{}, false, err
	}
	out.Moved, out.At = unpacked(moved), store.DecodeTime(at)
	return out, true, nil
}

// mayReadIn refuses an asker that may not read repository id — or the seat it
// reads for, ceilingSeat, that may not ([JudgeRead]) — judged from what a
// grant reads of its row in tx ([grantRefIn]).
func mayReadIn(ctx context.Context, tx *sql.Tx, id string, asker Principal, ceilingSeat string,
	chart Roster) error {
	ref, err := grantRefIn(ctx, tx, id)
	if err != nil {
		return err
	}
	_, err = JudgeRead(asker, ceilingSeat, ref, chart)
	return err
}

// grantRefIn is what a grant reads of repository id's row in tx — its name, its
// owner and its visibility — and its status beside them, by one primary-key
// read: [ErrRepoNotFound] where no row is there.
func grantRefIn(ctx context.Context, tx *sql.Tx, id string) (RepoRef, error) {
	var (
		ref                        = RepoRef{ID: id}
		status, owners, visibility string
	)
	err := tx.QueryRowContext(ctx, repoGrantSQL, id).Scan(&ref.Name, &status, &owners, &ref.Owner.Ref,
		&visibility)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RepoRef{}, fmt.Errorf("%w: %s", ErrRepoNotFound, id)
	case err != nil:
		return RepoRef{}, fmt.Errorf("gitlet: read repository %s's grant: %w", id, err)
	}
	ref.Status, ref.Owner.Kind, ref.Visibility = RepoStatus(status), OwnerKind(owners), Visibility(visibility)
	return ref, nil
}

// logCursor is "<packed position>/<id>": a push's or a change's place in log
// order, by its record's position and its id, since one record holds up to 64
// pushes, each opening a change of its own.
func logCursor(packed int64, id string) string {
	return strconv.FormatInt(packed, 10) + "/" + id
}

// cutLogCursor reads a [logCursor].
func cutLogCursor(s string) (int64, string, bool) {
	at, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return 0, "", false
	}
	position, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return position, id, true
}
