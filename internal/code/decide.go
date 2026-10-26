package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// THE PUSH DECIDE: one round of a batch's decision, taken from the rows ONE
// snapshot holds — re-run whole on every compare-and-set round, so it reads
// nothing outside tx and keeps nothing between rounds but the protection
// verdicts ([verdicts]), each a pure function of a command and rules the
// round's own snapshot holds.
//
// # Every proposal, in order, against what the ones before it did
//
// A batch is decided in the order its proposals were queued, and each one is
// judged against the repository's rows WITH the effects of every push before
// it in the batch applied: two pushes that each move main from where they
// last saw it are one that lands and one that is stale, exactly as two records
// would be. Each command meets the first rule that fires, in order:
//
//  1. a command the receiver already refused stays refused;
//  2. the rules no state decides ([StatelessRefusal]);
//  3. the ref already at the new value is a noop;
//  4. the ref not at the old value is stale;
//  5. a protection ([ProtectionRefusal]), every matching rule consulted;
//  6. a create beside a live ref above it or with a live ref below it
//     (ref_conflict, [working.beside]);
//  7. a create past MaxRefsPerRepo live refs (repo_refs) — this and 6
//     judged after every other command of its push, so the push's own
//     deletions free their room and their names whatever order git sent
//     them in;
//  8. otherwise ok.
//
// Then over the push's ok commands, a pack named longer ago than a write may
// still name it is refused upload_expired, and one that takes the repository
// past code.limits.repo_max_bytes repo_size; and an --atomic push with any
// refusal is refused whole ([Atomic]). A push with an ok command left is an
// entry of the record; one with none is answered here and writes nothing.
//
// # A push that already landed is answered from its row
//
// A push id the repository's history already holds is a push that applied —
// a proposal forwarded twice, or one a failed-over node decided again — and
// it is answered from its row and contributes nothing. Read BEFORE the
// repository's status, because a push that landed has landed whatever became
// of the repository since: told repo_archived, its client would push again
// what is already there.
//
// # Two reads for the batch, never one a command
//
// The decide runs inside the publisher's snapshot — ONE read transaction, on a
// connection of the replicated estate's reader pool, on the repository's one
// committer goroutine — and is re-run whole on every round a peer's record
// forces, so a per-command cost is paid commands times rounds, all of it
// holding that connection and that goroutine: the push ids every proposal
// names are read in ONE statement, and the refs every command names in ONE
// more, each over its primary key (planner.go) — never a point read a
// command, which a full batch makes nearly four thousand of. (It holds no write
// lane; the APPLY does, and reads the same way for that reason,
// apply_push.go.) A batch that may CREATE a ref reads two statements more —
// the directories its creates would make, and the refs under each create —
// and one that creates none reads neither.
//
// # A ref is never beside a directory of its name
//
// Every client keeps a ref as a file — refs/remotes/origin/feature — so
// refs/heads/feature and refs/heads/feature/x can never both be live: a clone
// holding one cannot fetch the other ("cannot lock ref"). git's receive-pack
// refuses the pair in its ref transaction, and here it runs none — every
// command goes to the hook — so the decide refuses it instead, and it is the
// only place that can: the pair is a fact about two refs, which one snapshot
// holds and no receiver sees whole. A create is refused ref_conflict when a
// ref it would make a directory of is live (refs/heads/feature, under
// refs/heads/feature/x) or a live ref sits under its name. Both are judged
// against the working state, so the batch decides as its records would one by
// one: the push's own deletions free a name — git's own transaction takes a
// deletion of feature beside a create of feature/v2 — an earlier push's create
// takes one, and a deleted ref (a tombstone) is in nobody's way, since no
// client holds it.
//
// The cost follows what the batch's creates spell, never what the
// repository holds: one primary-key seek for every directory a create would
// make (a ref of n components below its namespace makes n−1, the deepest a
// ref may spell about 120) and one ranged seek under each create, past the
// refs the batch itself names, which the working state answers instead.
//
// # The protections are asked once a batch, before its publish
//
// Asking every rule about every command is the decide's one cost that grows
// with what a founder writes rather than with the rows: a rule of gitref's
// costliest shape costs a 255-byte ref 11–12 µs, so a record's worth of
// such refs (2 567) against [MaxProtectionsPerRepo] such rules is about two
// seconds of a core, where a team's rules cost about what the reads do
// (doc.go's measured section). A command's verdict is a pure function of the
// command and the rules, so it is formed ONCE a batch and kept across the
// rounds of its one publish ([verdicts]) — and formed BEFORE the publish, on
// the committer goroutine, from the rules one short read of this node's rows
// holds ([verdicts.formBefore]), because it is CPU alone, and inside the
// snapshot it would hold one of the reader pool's few connections for all of
// its length. Every round compares its own snapshot's rules, whole, with those
// the verdicts were formed under, and forms them again inside the snapshot
// only when they differ — a settings record changing THIS repository's
// protections, which only an activation writes, landing between the read
// and the round or between two rounds. Every other round, the first
// included, pays the reads alone.

// verdicts are one batch's protection verdicts, formed before its publish
// ([verdicts.formBefore]) and kept across the rounds of that one publish
// while every round's snapshot holds the rules they were formed under. The
// zero value holds none.
type verdicts struct {
	// set is whether stored and rules are a round's: the zero value has
	// read no rules, which is not the same as a repository with none.
	set bool
	// stored are the rules every verdict below was formed under, as the
	// snapshot held them, and rules the same parsed.
	stored []Protection
	rules  Protections
	// of is each command's verdict, by its proposal's place in the batch
	// and its own in the proposal: a batch is the same proposals in every
	// round of its publish.
	of map[[2]int]verdict
	// formed counts the verdicts formed, which is the work the rounds
	// share — before the publish, or inside a round whose rules differ.
	formed int
}

// verdict is one command's answer from the protections: the refusal and the
// sentence naming the rule that refuses it, or "" for none.
type verdict struct {
	refusal Refusal
	detail  string
}

// under makes stored the rules the verdicts answer from: the verdicts kept
// from the batch's earlier rounds when stored are the rules those read, field
// by field and in order, and none otherwise.
func (v *verdicts) under(stored []Protection) error {
	if v.set && slices.Equal(v.stored, stored) {
		return nil
	}
	rules, err := ParseProtections(stored)
	if err != nil {
		return err
	}
	v.set, v.stored, v.rules, v.of = true, stored, rules, map[[2]int]verdict{}
	return nil
}

// verdict is command (the place of c in the proposal at push) under the
// rules, formed the first time a round asks and kept for the rest.
func (v *verdicts) verdict(push, command int, c Command) (Refusal, string) {
	key := [2]int{push, command}
	if got, ok := v.of[key]; ok {
		return got.refusal, got.detail
	}
	var got verdict
	if r, rule := ProtectionRefusal(c, v.rules); r != "" {
		got = verdict{refusal: r, detail: ProtectionDetail(rule, r)}
	}
	v.of[key] = got
	v.formed++
	return got.refusal, got.detail
}

// formBefore forms, before the batch's publish, the verdict of every command
// of batch a round can ask about, under repoID's rules as this node's rows
// hold them now — read in a transaction of their own, one statement over the
// primary key.
//
// # Outside the snapshot, because forming them needs no connection
//
// Forming the verdicts is CPU alone — about two seconds of a core at the
// costliest rules a repository can carry — and the publisher's snapshot is a
// read transaction holding a connection of the replicated estate's reader
// pool, which has four by default. Formed inside it, a batch's first round
// held that connection for the whole formation and needed it for 13–22 ms of
// reads (doc.go's measured section), so four repositories at those rules
// taking maximal batches at once held every reader the node has: every
// replicated read on the node waited behind them — the dashboard's, a peer
// router's, every other repository's decide — and the longer round widened
// the window in which a peer's push stales its expectation, so more rounds
// followed. Formed here, every round on the common path holds its reader for
// the reads alone.
//
// # The snapshot still decides
//
// These rules are this node's rows a moment before the publish takes its
// snapshot, and only the snapshot's rules count: every round compares its own
// with these, whole ([verdicts.under]), and a round that holds others forms
// the verdicts again inside the snapshot. So a read or a parse that fails
// here forms nothing and is not reported: the round reads the same rows and
// answers for what it finds — a parse that fails there fails the batch —
// because forming the verdicts here moves when the work is done and never
// what is decided.
func (v *verdicts) formBefore(ctx context.Context, db store.ReplicatedReader, repoID string,
	batch []Proposal) {

	var stored []Protection
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		stored, err = protectionsIn(ctx, tx, repoID)
		return err
	}); err != nil {
		return
	}
	if err := v.under(stored); err != nil {
		return
	}
	// EVERY COMMAND A ROUND MAY ASK ABOUT: the first proposal of each push
	// id — a later one is answered as its first and never decided — and of
	// it each command the receiver did not refuse and no stateless rule
	// refuses, in the place a round keeps its verdict under. A round asks
	// about fewer, since a noop or a stale command never reaches the rules;
	// forming theirs costs what the round would have paid had they moved,
	// and only here, outside every snapshot.
	first := make(map[string]bool, len(batch))
	for push, p := range batch {
		if ctx.Err() != nil {
			return // the publish ends on the same context, and a round forms what it lacks
		}
		if first[p.PushID] {
			continue
		}
		first[p.PushID] = true
		for command, c := range p.Commands {
			if c.Result != ResultNG && StatelessRefusal(c) == "" {
				v.verdict(push, command, c)
			}
		}
	}
}

// decided is one proposal's answer from one round.
type decided struct {
	// commands is every command with its result, and Detail on each ng the
	// decide formed.
	commands []Command
	// inRecord is a proposal whose entry is in this round's record.
	inRecord bool
	// collapsed is the row an earlier record applied this push as.
	collapsed *PushRow
	// refused is a refusal of the whole push — repo_deleted, repo_archived —
	// and detail its sentence, which every command it covers carries too.
	refused Refusal
	detail  string
	// err is a proposal no rule decides: its push id names a push to
	// another repository.
	err error
	// dupOf is the index of an earlier proposal in the batch with this
	// proposal's push id, whose answer is this one's; -1 for none.
	dupOf int
}

// decideBatch is one round's decision over the rows tx holds, for the batch in
// order: the entries of the record, every proposal's answer, and the version
// the decision read. Pure over (the rows, the batch, limits, retention, now):
// retention is code.deleted_retention_days on the deciding node's epoch, from
// which a deleted repository's refusal names when its purge may begin; v is
// the protection verdicts the batch's earlier rounds formed, which this round
// reads only where its own snapshot holds the rules they were formed under.
func decideBatch(ctx context.Context, tx *sql.Tx, repoID string, batch []Proposal,
	limits Limits, retention time.Duration, now time.Time, v *verdicts) (entries []PushEntry,
	answers []decided, version int64, err error) {

	answers = make([]decided, len(batch))
	ids := make([]string, 0, len(batch))
	first := make(map[string]int, len(batch))
	for i, p := range batch {
		answers[i].dupOf = -1
		if j, ok := first[p.PushID]; ok {
			answers[i].dupOf = j
			continue
		}
		first[p.PushID] = i
		ids = append(ids, p.PushID)
	}
	landed, err := pushRowsIn(ctx, tx, ids)
	if err != nil {
		return nil, nil, 0, err
	}
	open := make([]int, 0, len(batch))
	for i, p := range batch {
		if answers[i].dupOf >= 0 {
			continue
		}
		row, ok := landed[p.PushID]
		switch {
		case !ok:
			open = append(open, i)
		case row.RepoID != repoID:
			answers[i].err = fmt.Errorf("%w: push_id: %s is a push to repository %s, not %s",
				ErrInvalidProposal, p.PushID, row.RepoID, repoID)
		default:
			answers[i].collapsed, answers[i].commands = &row, row.Commands
		}
	}

	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return nil, nil, 0, err
	}
	var refused Refusal
	var detail string
	switch {
	case !found, repo.status == RepoStatusDeleted, repo.status == RepoStatusPurging:
		// WHICH OF FOUR STATES, because each has its own remedy
		// ([RepoGone]): a deleted repository comes back when declared again,
		// one being purged or purged never does.
		if detail, err = goneDetail(ctx, tx, repoID, repo, found, retention); err != nil {
			return nil, nil, 0, err
		}
		refused = RefusalRepoDeleted
	case repo.status == RepoStatusArchived:
		refused = RefusalRepoArchived
		detail = ArchivedDetail(repo.name)
	}
	if refused != "" {
		for _, i := range open {
			answers[i].refused, answers[i].detail = refused, detail
			answers[i].commands = refuseEvery(batch[i].Commands, refused, detail)
		}
		return nil, answers, repo.version, nil
	}

	stored, err := protectionsIn(ctx, tx, repoID)
	if err != nil {
		return nil, nil, 0, err
	}
	// PARSED ONCE A BATCH, never once a command and rule (refusal.go), and
	// again only in a round whose rules are not the ones parsed.
	if err = v.under(stored); err != nil {
		return nil, nil, 0, fmt.Errorf("code: read %s's protections: %w", repoID, err)
	}
	w, err := newWorking(ctx, tx, repoID, repo, batch, open, now)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, i := range open {
		p := batch[i]
		cmds := w.decide(i, p, v, limits)
		answers[i].commands = cmds
		if !slices.ContainsFunc(cmds, isOK) {
			continue
		}
		w.apply(p, cmds)
		answers[i].inRecord = true
		entries = append(entries, p.entryWith(cmds))
	}
	return entries, answers, repo.version, nil
}

// isOK reports a command that moves its ref.
func isOK(c Command) bool { return c.Result == ResultOK }

// refuseEvery is cmds with every command not already refused refused under
// code, with detail.
func refuseEvery(cmds []Command, code Refusal, detail string) []Command {
	out := slices.Clone(cmds)
	for i := range out {
		if out[i].Result != ResultNG {
			out[i].Result, out[i].Reason, out[i].Detail = ResultNG, code, detail
		}
	}
	return out
}

// pushRepo is what a decide reads of a repository's row: a push batch's, and a
// compaction's ([decideCompaction]).
type pushRepo struct {
	name      string
	status    RepoStatus
	refs      int
	liveBytes int64
	version   int64
	// deletedAt is when a deleted or purging repository was deleted, and
	// zero for any other.
	deletedAt time.Time
}

// pushRepoIn reads a repository's row for a decide.
func pushRepoIn(ctx context.Context, tx *sql.Tx, id string) (pushRepo, bool, error) {
	var r pushRepo
	var status string
	var deleted sql.NullInt64
	err := tx.QueryRowContext(ctx, decideRepoSQL, id).Scan(&r.name, &status, &r.refs,
		&r.liveBytes, &r.version, &deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pushRepo{}, false, nil
	case err != nil:
		return pushRepo{}, false, fmt.Errorf("code: read repository %s for its pushes: %w", id, err)
	}
	r.status = RepoStatus(status)
	if deleted.Valid {
		r.deletedAt = store.DecodeTime(deleted.Int64)
	}
	return r, true, nil
}

// deletionIn reads the deletion marker of a repository the push decide found
// no row of: its name, and true where the marker exists — a repository whose
// purge completed, which the marker records for ever — and false for an id no
// repository on this log had. Read only on a refusal's path, one point read
// over the marker's primary key: that one, and the committer's answer to a
// batch the marker's gate refused ([Committer.purgedDetail]), which names the
// repository by the same row.
func deletionIn(ctx context.Context, tx *sql.Tx, id string) (string, bool, error) {
	var name string
	err := tx.QueryRowContext(ctx, decideDeletionSQL, id).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("code: read repository %s's deletion marker: %w", id, err)
	}
	return name, true, nil
}

// pushRowsIn reads the push rows ids name, by push id, in ONE statement over
// the primary key.
func pushRowsIn(ctx context.Context, tx *sql.Tx, ids []string) (map[string]PushRow, error) {
	out := make(map[string]PushRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, pushesNamedSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the pushes a batch names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, _, err := scanPush(rows)
		if err != nil {
			return nil, fmt.Errorf("code: read the pushes a batch names: %w", err)
		}
		out[row.PushID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read the pushes a batch names: %w", err)
	}
	return out, nil
}

// working is the repository as the batch's earlier pushes leave it: the refs
// the batch names, and the counts the bounds are judged on.
//
// # The live refs are the one count a create is judged on
//
// MaxRefsPerRepo bounds LIVE refs, and only they: a ref the repository has no
// row for and one it holds as a tombstone both read as ZeroOID ([current]),
// so a create of either is one more live ref and nothing else, and a deletion
// one fewer — in the push that makes it too ([working.decide]). The deleted
// refs a repository still keeps as tombstones are bounded apart, by the
// applier ([MaxTombstonesPerRepo]), which forgets the oldest — or writes a
// record's deletions as removals — rather than refuse anybody; judged here
// with the live refs, a repository that deleted its stale branches refused
// every new one until their deletions aged out two days later.
type working struct {
	name string
	now  time.Time

	// targets is every ref the batch names, every directory its creates would
	// make and each create's upper bound ([readAround]), that has a ROW: its
	// value, or "" for a tombstone. A ref absent here has no row at all.
	targets map[string]string
	// watched is every ref the working state can hold live, sorted — one a
	// command names, which the batch may make, keep or delete, and one with a
	// live row in the snapshot — so the ones under a create are found by a
	// search; the snapshot is asked only about the live refs between them.
	watched []string
	// dirs is, for each ref the batch may create, the directories it would
	// make that the working state can hold live — those with a row, and
	// those a command names — and occupied, a live ref under it that nothing
	// in the batch names: in its way whatever the batch does.
	dirs     map[string][]string
	occupied map[string]string
	// live is the repository's live refs.
	live int
	// bytes is the packs the repository holds.
	bytes int64
}

// newWorking reads the refs every open proposal's commands name, in ONE
// statement, and sets the counts the bounds are judged on — and, for a batch
// that may create a ref, what is around each create ([working.readAround]).
func newWorking(ctx context.Context, tx *sql.Tx, repoID string, repo pushRepo, batch []Proposal,
	open []int, now time.Time) (*working, error) {

	var names []string
	seen := map[string]bool{}
	deleted := map[string]bool{}
	for _, i := range open {
		for _, c := range batch[i].Commands {
			if c.Result == ResultNG {
				continue
			}
			if !seen[c.Ref] {
				seen[c.Ref] = true
				names = append(names, c.Ref)
			}
			if c.New == ZeroOID {
				deleted[c.Ref] = true
			}
		}
	}
	targets, err := refsNamed(ctx, tx, repoID, names)
	if err != nil {
		return nil, fmt.Errorf("code: read the refs a batch names: %w", err)
	}
	w := &working{name: repo.name, now: now, targets: targets, live: repo.refs, bytes: repo.liveBytes,
		occupied: map[string]string{}}
	if err := w.readAround(ctx, tx, repoID, batch, open, seen, deleted); err != nil {
		return nil, err
	}
	return w, nil
}

// readAround reads what is around every ref the batch may create, so
// [working.beside] can judge each create against it: the rows of the
// directories a create would make and of its bound, into targets, what the
// working state can hold live, into watched, and for each create the first
// live ref under it that the snapshot holds and the working state does not
// answer, into occupied. seen is every ref the batch names, which it extends
// by the directories and the bounds; deleted, every ref a command of it
// deletes.
//
// A ref the batch MAY create is one a command gives a value, that no rule
// without state refuses, over no live value in the snapshot — or over one a
// command of the batch deletes, after which a later command creates it again.
// Every other command is an update, a deletion or a refusal, and moves no
// name, so a batch of updates reads nothing here.
func (w *working) readAround(ctx context.Context, tx *sql.Tx, repoID string, batch []Proposal, open []int,
	seen, deleted map[string]bool) error {

	var creatable []string
	asked := map[string]bool{}
	for _, i := range open {
		for _, c := range batch[i].Commands {
			if c.Result == ResultNG || c.New == ZeroOID || asked[c.Ref] || StatelessRefusal(c) != "" ||
				(w.current(c.Ref) != ZeroOID && !deleted[c.Ref]) {
				continue
			}
			asked[c.Ref] = true
			creatable = append(creatable, c.Ref)
		}
	}
	if len(creatable) == 0 {
		return nil
	}
	named := maps.Clone(seen) // the refs the batch's commands name
	// THE ROWS AROUND EVERY CREATE: each directory it would make, and — for a
	// create under another create — its upper bound ref + "0", a name the
	// gaps below never include and one that lies under every create above it
	// (refs/heads/x/y0 is under refs/heads/x), so the working state answers
	// it like a directory. A create under none has its bound under none, so
	// nothing is asked for it there: a batch of creates beside each other
	// reads its directories and nothing more.
	var asks []string
	ask := func(name string) {
		if !seen[name] {
			seen[name] = true
			asks = append(asks, name)
		}
	}
	of := make([][]string, len(creatable))
	for i, ref := range creatable {
		of[i] = directoriesOf(ref)
		nested := false
		for _, d := range of[i] {
			ask(d)
			nested = nested || asked[d]
		}
		if nested {
			ask(ref + "0")
		}
	}
	if err := inChunks(asks, func(part []string) error {
		rows, err := refsNamed(ctx, tx, repoID, part)
		maps.Copy(w.targets, rows)
		return err
	}); err != nil {
		return fmt.Errorf("code: read the directories a batch's creates would make: %w", err)
	}
	// WHAT THE WORKING STATE CAN HOLD LIVE: a ref a command names, which the
	// batch may make, and a name with a live row. A name with neither — a
	// directory or a bound with no row, or a tombstone — stays no live ref
	// whatever the batch does, which is nearly every directory a create would
	// make; so [working.beside] never walks them.
	for n := range seen {
		if named[n] || w.targets[n] != "" {
			w.watched = append(w.watched, n)
		}
	}
	slices.Sort(w.watched)
	// A DIRECTORY THE WORKING STATE CAN HOLD LIVE, kept for each create: one
	// with a row, or one a command names. Any other has no row and nothing in
	// the batch makes it, so it is in nobody's way — which is nearly every
	// directory, and so nearly every create keeps none.
	w.dirs = make(map[string][]string, len(creatable))
	for i, ref := range creatable {
		for _, d := range of[i] {
			if _, row := w.targets[d]; row || named[d] {
				w.dirs[ref] = append(w.dirs[ref], d)
			}
		}
	}

	// THE GAPS: every create's range (ref + "/", ref + "0") cut at the
	// bounds of every create and at every name with a live row, each gap
	// asked ONCE for the first live ref in it — a live name being the working
	// state's to answer, since the batch may delete it, and anything else
	// live there in the way whatever the batch does. A create's first such
	// ref is the first answer among the gaps inside its range. Ranges of
	// refs nest or are apart, so a gap lies inside a range or outside it;
	// and one gap serves every create whose range holds it, so the gaps are
	// at most the creates' bounds and the live names — where asking each
	// create for the gaps between the names under it asked a nested chain of
	// creates over everything named below it once a create: 3.4–4.5 s a round
	// and some 600 MB of statements for a record of 118 nested creates over
	// 2 492 more under the deepest (BenchmarkDecideABatch, fanned).
	//
	// ONE SORTED SWEEP over the cuts makes the gaps and, for each create, the
	// run of them inside its range: its range opens at its lower bound and
	// closes at its upper, so the gaps made between the two are its own and
	// those of every create inside it. A lower bound, ending in '/', is no
	// ref and no other bound; an upper bound, ending in '0', may be a live
	// name too, and is one cut.
	type cut struct {
		at     string
		create int // the create whose bound it is; -1 for a live name
		opens  bool
	}
	cuts := make([]cut, 0, 2*len(creatable))
	for i, ref := range creatable {
		cuts = append(cuts, cut{ref + "/", i, true}, cut{ref + "0", i, false})
	}
	for n, target := range w.targets {
		if target != "" {
			cuts = append(cuts, cut{n, -1, false})
		}
	}
	slices.SortFunc(cuts, func(a, b cut) int { return strings.Compare(a.at, b.at) })
	var gaps [][2]string
	from, to := make([]int, len(creatable)), make([]int, len(creatable))
	inside := 0 // the ranges holding the gap after the cut at hand
	for i := 0; i < len(cuts); {
		at := cuts[i].at
		for ; i < len(cuts) && cuts[i].at == at; i++ {
			switch c := cuts[i]; {
			case c.create < 0:
			case c.opens:
				inside++
				from[c.create] = len(gaps)
			default:
				inside--
				to[c.create] = len(gaps)
			}
		}
		if inside > 0 && i < len(cuts) {
			gaps = append(gaps, [2]string{at, cuts[i].at})
		}
	}
	found, err := liveIn(ctx, tx, repoID, gaps)
	if err != nil {
		return fmt.Errorf("code: read the refs under a batch's creates: %w", err)
	}
	// next[g] is the first gap from g on that holds a live ref.
	next := make([]int, len(gaps)+1)
	next[len(gaps)] = len(gaps)
	for g := len(gaps) - 1; g >= 0; g-- {
		if next[g] = next[g+1]; found[g] != "" {
			next[g] = g
		}
	}
	for i, ref := range creatable {
		if g := next[from[i]]; g < to[i] {
			w.occupied[ref] = found[g]
		}
	}
	return nil
}

// namesPerStatement is the most names one statement of the decide's lists
// carries: every ref is at most MaxRefNameBytes, so a list is at most about a
// mebibyte however deep the refs a batch creates — the directories of a
// record's worth of the deepest refs are some three hundred thousand, and in
// one statement they cost several hundred megabytes of encoding (measured,
// BenchmarkDecideABatch's deepest arm) where in parts they cost the same time
// and a part's memory.
const namesPerStatement = 4096

// inChunks calls f with list in consecutive parts of at most
// [namesPerStatement], stopping at the first error.
func inChunks[T any](list []T, f func(part []T) error) error {
	for len(list) > 0 {
		n := min(len(list), namesPerStatement)
		if err := f(list[:n]); err != nil {
			return err
		}
		list = list[n:]
	}
	return nil
}

// current is ref's value, ZeroOID where it has none — no row, or a tombstone.
func (w *working) current(ref string) string {
	if target := w.targets[ref]; target != "" {
		return target
	}
	return ZeroOID
}

// watchedUnder is every watched ref that lies under ref — with ref + "/" as a
// prefix, which in byte order is every name between ref + "/" and ref + "0"
// ('0' is the byte after '/').
func (w *working) watchedUnder(ref string) []string {
	from, _ := slices.BinarySearch(w.watched, ref+"/")
	to, _ := slices.BinarySearch(w.watched, ref+"0")
	return w.watched[from:to]
}

// beside is the live ref in the way of creating ref, as the working state and
// the push being decided leave things — made its creates judged ok so far,
// gone its deletions judged ok — and whether there is one: a directory ref
// would make that is a live ref, a live ref under ref the batch names
// nowhere, or a watched one under it that is live by now.
func (w *working) beside(ref string, made, gone map[string]bool) (string, bool) {
	live := func(r string) bool {
		switch {
		case made[r]:
			return true
		case gone[r]:
			return false
		}
		return w.current(r) != ZeroOID
	}
	for _, d := range w.dirs[ref] {
		if live(d) {
			return d, true
		}
	}
	if other, ok := w.occupied[ref]; ok {
		return other, true
	}
	for _, n := range w.watchedUnder(ref) {
		if live(n) {
			return n, true
		}
	}
	return "", false
}

// directoriesOf is every directory ref would make on a client's disk that
// another ref could be: each of its prefixes that ends at a '/' past its
// namespace, shallowest first (refs/heads/a/b/c → refs/heads/a,
// refs/heads/a/b). refs/heads itself is no ref any rule admits, so it is
// never one.
func directoriesOf(ref string) []string {
	rest, ok := strings.CutPrefix(ref, "refs/")
	if !ok {
		return nil
	}
	ns := strings.IndexByte(rest, '/')
	if ns < 0 {
		return nil
	}
	start := len("refs/") + ns + 1
	var out []string
	for i := start; i < len(ref); i++ {
		if ref[i] == '/' {
			out = append(out, ref[:i])
		}
	}
	return out
}

// liveIn is, for each gap (lo, hi) — both exclusive — the first live ref of
// repoID between them in byte order, or "" for none.
//
// TWO STATEMENTS, the second only for a gap that holds one: whether a gap holds
// a live ref ([gapsHoldingSQL], one ranged seek of the live refs' index a gap,
// stopping at its first row), then which ref it is ([firstLiveInSQL]). Asked
// as one statement — the first live ref of every gap — it cost about twice as
// much (9.5 µs a gap against 5.4, three thousand gaps measured over the
// primary key), and the gap that holds a live ref is the create that is
// refused, which nearly none is.
func liveIn(ctx context.Context, tx *sql.Tx, repoID string, gaps [][2]string) ([]string, error) {
	out := make([]string, len(gaps))
	var held []int
	if err := inChunks(gapIndices(len(gaps)), func(part []int) error {
		found, err := gapsAnswer(ctx, tx, gapsHoldingSQL, repoID, gaps, part, func(rows *sql.Rows) (int, string, error) {
			var i int
			err := rows.Scan(&i)
			return i, "", err
		})
		for i := range found {
			held = append(held, part[i])
		}
		return err
	}); err != nil {
		return nil, err
	}
	if err := inChunks(held, func(part []int) error {
		found, err := gapsAnswer(ctx, tx, firstLiveInSQL, repoID, gaps, part, func(rows *sql.Rows) (int, string, error) {
			var i int
			var ref sql.NullString
			err := rows.Scan(&i, &ref)
			return i, ref.String, err
		})
		for i, ref := range found {
			out[part[i]] = ref
		}
		return err
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// gapIndices is 0 … n-1.
func gapIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// gapsAnswer runs statement over the gaps part names — its list the gaps in
// part's order — and reads each row it answers with scan: the gap's place in
// the list and what the statement says of it. The answer is keyed by that
// place.
func gapsAnswer(ctx context.Context, tx *sql.Tx, statement, repoID string, gaps [][2]string, part []int,
	scan func(*sql.Rows) (int, string, error)) (map[int]string, error) {

	asked := make([][2]string, len(part))
	for i, g := range part {
		asked[i] = gaps[g]
	}
	list, err := json.Marshal(asked)
	if err != nil {
		return nil, fmt.Errorf("code: encode the gaps under a batch's creates: %w", err)
	}
	rows, err := tx.QueryContext(ctx, statement, repoID, string(list))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int]string{}
	for rows.Next() {
		i, ref, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if i < 0 || i >= len(part) {
			return nil, fmt.Errorf("code: the gaps under a batch's creates answered gap %d of %d", i, len(part))
		}
		out[i] = ref
	}
	return out, rows.Err()
}

// conflictDetail is the sentence of a create refused ref_conflict: the ref in
// its way, and what to do. It names that one ref and not the create's own,
// which git prints beside the reason.
func conflictDetail(other string) string {
	return fmt.Sprintf("%s is in the way: a ref is never also a directory of refs, so a clone holding one "+
		"could not fetch the other; delete it first, or choose another name", other)
}

// decide is one push's commands with their results, judged against the
// working state — which it does not change ([working.apply] does, for a push
// that becomes an entry). push is p's place in the batch, which its
// protection verdicts are kept under.
func (w *working) decide(push int, p Proposal, v *verdicts, limits Limits) []Command {
	cmds := slices.Clone(p.Commands)
	// EVERY COMMAND BUT A CREATE IS JUDGED FIRST, so the live refs the push's
	// own deletions free are known before any create is judged against the
	// bound, whatever order git sent them in: deleting a stale branch and
	// adding a new one at MaxRefsPerRepo ends holding no more than it began
	// with. Safe because what follows the per-command rules — upload_expired
	// and repo_size over every ok command, --atomic over the whole push —
	// refuses every ok command together, so a deletion is never refused
	// while a create it made room for stands.
	var creating []int
	var deletes int
	gone := map[string]bool{}
	for i := range cmds {
		c := &cmds[i]
		if c.Result == ResultNG {
			continue
		}
		if r := StatelessRefusal(*c); r != "" {
			refuse(c, r, statelessDetail(*c, r))
			continue
		}
		current := w.current(c.Ref)
		switch {
		case current == c.New:
			c.Result = ResultNoop
			continue
		case current != c.Old:
			refuse(c, RefusalStale, fmt.Sprintf("%s is at %s; fetch first", c.Ref, short(current)))
			continue
		}
		if r, detail := v.verdict(push, i, *c); r != "" {
			refuse(c, r, detail)
			continue
		}
		if current == ZeroOID {
			creating = append(creating, i)
			continue
		}
		if c.New == ZeroOID {
			deletes++
			gone[c.Ref] = true
		}
		c.Result = ResultOK
	}
	// A PUSH'S OWN CREATES COUNT AGAINST ITS LATER ONES, in the order git
	// sent them: a thousand creates at the bound's last free ref are one ok
	// and the rest refused — and a create takes its name from every later
	// one beside it, so of feature and feature/x the first git sent lands.
	// A create in the way of nothing takes a live ref's place first, so one
	// refused ref_conflict never counts against the bound.
	var creates int
	made := map[string]bool{}
	for _, i := range creating {
		c := &cmds[i]
		if other, ok := w.beside(c.Ref, made, gone); ok {
			refuse(c, RefusalRefConflict, conflictDetail(other))
			continue
		}
		if held := w.live - deletes + creates; held >= MaxRefsPerRepo {
			refuse(c, RefusalRepoRefs, fmt.Sprintf("%s holds %d live refs, the most one "+
				"repository may", w.name, held))
			continue
		}
		creates++
		made[c.Ref] = true
		c.Result = ResultOK
	}
	if p.Pack != nil && slices.ContainsFunc(cmds, isOK) {
		detail, expired := uploadExpired(p.Pack.Object, w.now, "run git push again")
		switch {
		case expired:
			refuseOK(cmds, RefusalUploadExpired, detail)
		case w.bytes+p.Pack.Size > limits.RepoMaxBytes:
			refuseOK(cmds, RefusalRepoSize, fmt.Sprintf("%s would hold %s of packs, past %s "+
				"(code.limits.repo_max_bytes)", w.name, gib(w.bytes+p.Pack.Size), gib(limits.RepoMaxBytes)))
		}
	}
	if p.Atomic {
		cmds = Atomic(cmds)
	}
	return cmds
}

// uploadExpired is the one rule a decide holds a stored bundle's key to: a
// bundle whose key was minted objstore.RecordWithin or more before the
// decision may no longer be named by a record — a collection may already have
// judged so old a key unnamed and deleted its bytes, and a row naming it would
// name bytes that are gone — and the sentence that says so, ending in again,
// what its asker does next. ONE RULE AND ONE SENTENCE for every decide that
// names a stored pack (ADR-0008): a push's pack and a compaction's outputs.
func uploadExpired(object objstore.Key, now time.Time, again string) (string, bool) {
	minted := object.Minted()
	if now.Sub(minted) < objstore.RecordWithin {
		return "", false
	}
	return fmt.Sprintf("the pack was uploaded at %s, %s or more before this decision; %s",
		minted.UTC().Format(time.RFC3339), objstore.RecordWithin, again), true
}

// goneDetail is the sentence a decide refuses repo_deleted with: [RepoGone]'s,
// for a repository with no row (repo is then its zero value, and the deletion
// marker names it, read here) or one deleted or being purged — retention, the
// deciding epoch's code.deleted_retention_days, names when its purge may
// begin. ONE READING OF THE FOUR STATES for every decide that refuses one
// (ADR-0008): a push's and a compaction's.
func goneDetail(ctx context.Context, tx *sql.Tx, repoID string, repo pushRepo, found bool,
	retention time.Duration) (string, error) {

	gone := RepoGone{ID: repoID, Name: repo.name, Status: repo.status, DeletedAt: repo.deletedAt,
		Retention: retention}
	if !found {
		var err error
		if gone.Name, gone.Purged, err = deletionIn(ctx, tx, repoID); err != nil {
			return "", err
		}
	}
	return gone.Detail(), nil
}

// apply moves the working state by a push's ok commands, as its record will.
func (w *working) apply(p Proposal, cmds []Command) {
	for _, c := range cmds {
		if c.Result != ResultOK {
			continue
		}
		before := w.current(c.Ref)
		switch {
		case before == ZeroOID && c.New != ZeroOID:
			w.live++
		case before != ZeroOID && c.New == ZeroOID:
			w.live--
		}
		if c.New == ZeroOID {
			w.targets[c.Ref] = ""
		} else {
			w.targets[c.Ref] = c.New
		}
	}
	if p.Pack != nil {
		w.bytes += p.Pack.Size
	}
}

// refuse sets one command ng under code with detail.
func refuse(c *Command, code Refusal, detail string) {
	c.Result, c.Reason, c.Detail = ResultNG, code, detail
}

// refuseOK refuses every ok command under code with detail.
func refuseOK(cmds []Command, code Refusal, detail string) {
	for i := range cmds {
		if cmds[i].Result == ResultOK {
			refuse(&cmds[i], code, detail)
		}
	}
}

// short is an object id as git abbreviates it, or "nothing" for none.
func short(oid string) string {
	if oid == ZeroOID {
		return "nothing"
	}
	return oid[:7]
}

// gib is a byte count as a person reads one.
func gib(n int64) string { return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30)) }

// statelessDetail is the specifics of a refusal no state decides.
func statelessDetail(c Command, r Refusal) string {
	switch r {
	case RefusalReservedRef:
		switch {
		case strings.HasPrefix(c.Ref, reservedPrefix):
			return fmt.Sprintf("%s is under %s, which is the engine's own", c.Ref, reservedPrefix)
		case reservedSeatRef(c.Ref):
			return fmt.Sprintf("%s as a branch would block seats' own namespaces (%s<handle>/), "+
				"since no ref is created beside a directory of its name", c.Ref, SeatNamespaceRoot)
		}
		return fmt.Sprintf("%s is outside %s and %s", c.Ref, branchPrefix, tagPrefix)
	case RefusalTagMoved:
		return fmt.Sprintf("%s names %s and a tag does not move: delete it and push it again",
			c.Ref, short(c.Old))
	}
	// bad_ref: the ref is not echoed, since it is not one — git prints what
	// the client sent beside the reason.
	return r.Sentence()
}

// ProtectionDetail names the rule that refused a command and the flag it
// refused it by — the rule [ProtectionRefusal] answered with its refusal, so
// naming it asks no rule about the ref a second time. ONE SPELLING for the
// decide and the code host, which refuses a protected update before it
// uploads anything (ADR-0008).
//
// IT NAMES NO REF: git prints the ref beside the reason, and the code host
// carries this sentence in a proposal, whose every Detail is bounded
// (maxDetailBytes) — a ref and a pattern of MaxRefNameBytes each could never
// fit there beside the words, and a push whose sentence did not fit was
// refused whole as the receiver's own bug.
func ProtectionDetail(rule Protection, r Refusal) string {
	switch r {
	case RefusalProtectedDirect:
		return fmt.Sprintf("takes no direct push (protection %s: direct_push none)", rule.Pattern)
	case RefusalProtectedDelete:
		return fmt.Sprintf("may not be deleted (protection %s: allow_delete)", rule.Pattern)
	case RefusalProtectedForce:
		return fmt.Sprintf("refuses a force-push (protection %s: allow_force)", rule.Pattern)
	}
	return r.Sentence()
}

// UnheldDetail is the sentence of a branch update the code host refuses
// stale before proposing it: the id the client says the ref was at is no
// object this repository holds — the client's ref came from history never
// pushed here, or one a force-push replaced — so nothing can say how the
// update relates to it. Named by its abbreviation, and no ref, for
// ProtectionDetail's reason: it travels in a proposal.
func UnheldDetail(old string) string {
	return fmt.Sprintf("was at %s, which this repository does not hold; fetch first", short(old))
}

// The push decide's statements, each registered in plan_test.go. The refs a
// batch names are read by the applier's own statement (refsNamedSQL).
const (
	decideRepoSQL = `SELECT name, status, refs, live_bytes, version, deleted_at
		FROM code_repos WHERE id = ?`
	decideDeletionSQL = `SELECT name FROM code_deletions WHERE repo_id = ?`
	pushesNamedSQL    = `SELECT ` + pushColumns + ` FROM code_pushes
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	// gapsHoldingSQL is the place of each [lo, hi] gap of its list that
	// holds a live ref, and firstLiveInSQL the first such ref of each gap:
	// both driven from the list, each gap one ranged seek of the LIVE refs'
	// index ([liveRefsKey]) that stops at its first row — a subquery, so a gap
	// answers one row however many refs lie in it, where a join would answer
	// every one. The live refs' index, never the key: a gap's range holds the
	// tombstones under a create too, and over the key the seek stepped over
	// each of them before its first live row, so a deep chain of creates over
	// a repository's tombstones read every one once a create (planner.go).
	// `r.target != ''` is the index's own predicate, repeated so the planner
	// may use it.
	gapsHoldingSQL = `SELECT CAST(j.key AS INTEGER) FROM json_each(?2) AS j
		WHERE EXISTS (SELECT 1 FROM code_refs AS r INDEXED BY ` + liveRefsKey + `
			WHERE r.repo_id = ?1 AND r.ref > CAST(j.value ->> 0 AS TEXT) AND r.ref < CAST(j.value ->> 1 AS TEXT)
				AND r.target != '')`
	firstLiveInSQL = `SELECT CAST(j.key AS INTEGER), (SELECT r.ref FROM code_refs AS r INDEXED BY ` + liveRefsKey + `
		WHERE r.repo_id = ?1 AND r.ref > CAST(j.value ->> 0 AS TEXT) AND r.ref < CAST(j.value ->> 1 AS TEXT)
			AND r.target != '' ORDER BY r.ref LIMIT 1)
		FROM json_each(?2) AS j`
)
