package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
//  6. a create past the refs a repository may hold (repo_refs);
//  7. otherwise ok.
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
// command, which a maximal batch makes three thousand of. (It holds no write
// lane; the APPLY does, and reads the same way for that reason,
// apply_push.go.)
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
		got = verdict{refusal: r, detail: protectionDetail(c, rule, r)}
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
// the decision read. Pure over (the rows, the batch, limits, now): v is the
// protection verdicts the batch's earlier rounds formed, which this round
// reads only where its own snapshot holds the rules they were formed under.
func decideBatch(ctx context.Context, tx *sql.Tx, repoID string, batch []Proposal,
	limits Limits, now time.Time, v *verdicts) (entries []PushEntry, answers []decided,
	version int64, err error) {

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
	switch {
	case !found, repo.status == RepoStatusDeleted, repo.status == RepoStatusPurging:
		refused = RefusalRepoDeleted
	case repo.status == RepoStatusArchived:
		refused = RefusalRepoArchived
	}
	if refused != "" {
		named := repoID
		if found {
			named = repo.name
		}
		detail := fmt.Sprintf("repository %s is %s", named, map[Refusal]string{
			RefusalRepoDeleted:  "deleted; declaring it again restores it until its purge",
			RefusalRepoArchived: "archived and read-only",
		}[refused])
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
		cmds, err := w.decide(i, p, v, limits)
		if err != nil {
			return nil, nil, 0, err
		}
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

// pushRepo is what the push decide reads of a repository's row.
type pushRepo struct {
	name       string
	status     RepoStatus
	refs       int
	tombstones int
	liveBytes  int64
	version    int64
}

// pushRepoIn reads a repository's row for the push decide.
func pushRepoIn(ctx context.Context, tx *sql.Tx, id string) (pushRepo, bool, error) {
	var r pushRepo
	var status string
	err := tx.QueryRowContext(ctx, decideRepoSQL, id).Scan(&r.name, &status, &r.refs,
		&r.tombstones, &r.liveBytes, &r.version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pushRepo{}, false, nil
	case err != nil:
		return pushRepo{}, false, fmt.Errorf("code: read repository %s for its pushes: %w", id, err)
	}
	r.status = RepoStatus(status)
	return r, true, nil
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
type working struct {
	ctx    context.Context
	tx     *sql.Tx
	repoID string
	name   string
	now    time.Time

	// targets is every ref the batch names that has a ROW: its value, or
	// "" for a tombstone. A ref absent here has no row at all.
	targets map[string]string
	// live is the repository's live refs; table is the rows its refs hold
	// — live and tombstones — which a create of a ref with no row grows.
	live, table int
	// bytes is the packs the repository holds.
	bytes int64

	// sweepFloor is the fewest expired tombstones the record's own sweep
	// takes, whatever the batch decides: what MaxRecordRows leaves past
	// the most rows the batch's entries can write.
	sweepFloor int
	// recreatable is the tombstones the batch names with a command that
	// would bring the ref back — rows a sweep cannot count on taking.
	recreatable int
	// free is the expired tombstones the record's sweep will take, read
	// once a round and only when a create meets the table's bound.
	free      int
	freeKnown bool
}

// newWorking reads the refs every open proposal's commands name, in ONE
// statement, and sets the counts the bounds are judged on.
func newWorking(ctx context.Context, tx *sql.Tx, repoID string, repo pushRepo, batch []Proposal,
	open []int, now time.Time) (*working, error) {

	var names []string
	seen := map[string]bool{}
	rows := 0
	for _, i := range open {
		rows += rowBound(batch[i])
		for _, c := range batch[i].Commands {
			if c.Result != ResultNG && !seen[c.Ref] {
				seen[c.Ref] = true
				names = append(names, c.Ref)
			}
		}
	}
	targets, err := refsNamed(ctx, tx, repoID, names)
	if err != nil {
		return nil, fmt.Errorf("code: read the refs a batch names: %w", err)
	}
	w := &working{
		ctx: ctx, tx: tx, repoID: repoID, name: repo.name, now: now, targets: targets,
		live: repo.refs, table: repo.refs + repo.tombstones, bytes: repo.liveBytes,
		sweepFloor: max(0, MaxRecordRows-recordRows(rows)),
	}
	recreated := map[string]bool{}
	for _, i := range open {
		for _, c := range batch[i].Commands {
			if target, row := targets[c.Ref]; row && target == "" && c.New != ZeroOID &&
				c.Result != ResultNG {
				recreated[c.Ref] = true
			}
		}
	}
	w.recreatable = len(recreated)
	return w, nil
}

// current is ref's value, ZeroOID where it has none, and whether it has a row.
func (w *working) current(ref string) (string, bool) {
	target, row := w.targets[ref]
	if target == "" {
		return ZeroOID, row
	}
	return target, row
}

// decide is one push's commands with their results, judged against the
// working state — which it does not change ([working.apply] does, for a push
// that becomes an entry). push is p's place in the batch, which its
// protection verdicts are kept under.
func (w *working) decide(push int, p Proposal, v *verdicts, limits Limits) ([]Command, error) {
	cmds := slices.Clone(p.Commands)
	// A PUSH'S OWN CREATES COUNT AGAINST ITS LATER ONES: a thousand
	// creates at the bound's last free ref are one ok and the rest refused.
	var creates, newRows int
	for i := range cmds {
		c := &cmds[i]
		if c.Result == ResultNG {
			continue
		}
		if r := StatelessRefusal(*c); r != "" {
			refuse(c, r, statelessDetail(*c, r))
			continue
		}
		current, row := w.current(c.Ref)
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
			if w.live+creates >= MaxRefsPerRepo {
				refuse(c, RefusalRepoRefs, fmt.Sprintf("%s holds %d live refs, the most one "+
					"repository may", w.name, w.live+creates))
				continue
			}
			if !row {
				free, err := w.freeTombstones()
				if err != nil {
					return nil, err
				}
				if held := w.table + newRows; held+1 > MaxRefsPerRepo+free {
					refuse(c, RefusalRepoRefs, fmt.Sprintf("%s's refs hold %d rows counting its "+
						"deleted refs, the most they may hold (%d); a deleted ref's row goes %s "+
						"after its deletion", w.name, held, MaxRefsPerRepo, RefTombstoneHorizon))
					continue
				}
				newRows++
			}
			creates++
		}
		c.Result = ResultOK
	}
	if p.Pack != nil && slices.ContainsFunc(cmds, isOK) {
		minted := p.Pack.Object.Minted()
		switch {
		case w.now.Sub(minted) >= objstore.RecordWithin:
			refuseOK(cmds, RefusalUploadExpired, fmt.Sprintf("the pack was uploaded at %s, %s or "+
				"more before this decision; run git push again", minted.UTC().Format(time.RFC3339),
				objstore.RecordWithin))
		case w.bytes+p.Pack.Size > limits.RepoMaxBytes:
			refuseOK(cmds, RefusalRepoSize, fmt.Sprintf("%s would hold %s of packs, past %s "+
				"(code.limits.repo_max_bytes)", w.name, gib(w.bytes+p.Pack.Size), gib(limits.RepoMaxBytes)))
		}
	}
	if p.Atomic {
		cmds = Atomic(cmds)
	}
	return cmds, nil
}

// apply moves the working state by a push's ok commands, as its record will.
func (w *working) apply(p Proposal, cmds []Command) {
	for _, c := range cmds {
		if c.Result != ResultOK {
			continue
		}
		before, row := w.current(c.Ref)
		switch {
		case before == ZeroOID && c.New != ZeroOID:
			w.live++
			if !row {
				w.table++
			}
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

// freeTombstones is how many of the repository's tombstones the record's own
// sweep will take: the expired ones a batch's commands do not bring back, up
// to the fewest the record's row budget leaves the sweep.
//
// # Why the table's bound counts them free
//
// The table's rows are what the bound protects — every row is read by a full
// snapshot and rewritten into every cache's packed-refs — and a tombstone is
// removed only by a push record's sweep. Judged without them, a repository at
// the bound whose rows are mostly expired tombstones would refuse every push
// that creates a ref: a repository whose only traffic is new branches would
// make no record, so nothing would ever sweep, and it would stay refused. Its
// own record's sweep takes them, so they are counted as gone.
//
// EXPIRED BY THIS NODE'S CLOCK, where the sweep judges by the record's broker
// instant — later than the decision, so it takes at least as many, unless this
// clock runs ahead of the broker's; then the table may pass the bound by the
// deletions of that skew out of a 48-hour horizon, rows the next record sweeps.
func (w *working) freeTombstones() (int, error) {
	if w.freeKnown {
		return w.free, nil
	}
	w.freeKnown = true
	if w.sweepFloor == 0 {
		// NOTHING IS LEFT FOR THE SWEEP, so nothing is asked: the count
		// below would be limited to the recreatable tombstones and free
		// none of them.
		return 0, nil
	}
	// THE LIMIT IS THE SWEEP'S BUDGET, and the one place it is applied: the
	// count stops at sweepFloor past the tombstones the batch may bring
	// back, so what is left after them is never more than the sweep takes —
	// and the covering scan never reads further than that.
	var expired int
	if err := w.tx.QueryRowContext(w.ctx, expiredTombstoneCountSQL, w.repoID,
		store.EncodeTime(w.now.Add(-RefTombstoneHorizon)), w.sweepFloor+w.recreatable).
		Scan(&expired); err != nil {
		return 0, fmt.Errorf("code: count %s's expired tombstones: %w", w.repoID, err)
	}
	w.free = max(0, expired-w.recreatable)
	return w.free, nil
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
		if strings.HasPrefix(c.Ref, reservedPrefix) {
			return fmt.Sprintf("%s is under %s, which is the engine's own", c.Ref, reservedPrefix)
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

// protectionDetail names the rule that refused c and the flag it refused it
// by — the rule [ProtectionRefusal] answered with its refusal, so naming it
// asks no rule about the ref a second time.
func protectionDetail(c Command, rule Protection, r Refusal) string {
	switch r {
	case RefusalProtectedDirect:
		return fmt.Sprintf("%s takes no direct push (protection %s: direct_push none)",
			c.Ref, rule.Pattern)
	case RefusalProtectedDelete:
		return fmt.Sprintf("%s may not be deleted (protection %s: allow_delete)", c.Ref, rule.Pattern)
	case RefusalProtectedForce:
		return fmt.Sprintf("%s refuses a force-push (protection %s: allow_force)", c.Ref, rule.Pattern)
	}
	return r.Sentence()
}

// The push decide's statements, each registered in plan_test.go. The refs a
// batch names are read by the applier's own statement (refsNamedSQL).
const (
	decideRepoSQL = `SELECT name, status, refs, tombstones, live_bytes, version FROM code_repos
		WHERE id = ?`
	pushesNamedSQL = `SELECT ` + pushColumns + ` FROM code_pushes
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	expiredTombstoneCountSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_refs
		WHERE repo_id = ? AND target = '' AND at < ? LIMIT ?)`
)
