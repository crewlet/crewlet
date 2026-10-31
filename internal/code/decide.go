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
	"github.com/crewlet/crewlet/internal/period"
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
//     2b. a command the principal's GRANT does not cover ([Grant.RefRule],
//     not_permitted) — a seat's judged from the deciding epoch's chart
//     ([SeatRules]), a person's and a token's from rules that need none;
//  3. the ref already at the new value is a noop;
//  4. the ref not at the old value is stale;
//     4b. a command of an EXPIRY entry ([ViaExpiry]) whose ref is no longer
//     its seat's idle branch ([ExpiryRule]) is stale — pushed since it was
//     listed, held by a live turn since, the repository's default branch, a
//     live change's source or target, or last set by somebody else;
//  5. a protection ([ProtectionRefusal]), every matching rule consulted —
//     and a rule that takes a person's push alone refuses everybody else's;
//  6. a create beside a live ref above it or with a live ref below it
//     (ref_conflict, [working.beside]);
//  7. a create past MaxRefsPerRepo live refs (repo_refs) — past HALF of them
//     for a seat outside the repository's owners ([OutsideSeatShare]) — this
//     and 6 judged after every other command of its push, so the push's own
//     deletions free their room and their names whatever order git sent
//     them in;
//  8. otherwise ok.
//
// Then over the push's ok commands, a pack named longer ago than a write may
// still name it is refused upload_expired, and one that takes the repository
// past code.limits.repo_max_bytes — past half of it, for a seat outside the
// repository's owners — repo_size; and an --atomic push with any refusal is
// refused whole ([Atomic]). A push with an ok command left is an entry of the
// record — unless a seat's day's quota or the pack bound refuses it whole
// (below); one with none is answered here and writes nothing. A push checked
// at another drop epoch than the repository's meets none of these rules: it
// is refused whole before them (below).
//
// # A seat's push is held to its grant, recorded, and to its day, not
//
// A grant's refusal is RECORDED on its command (not_permitted, rule 2b): who
// tried to move what past their grant is an audit fact, and the receiving
// node's own check of the grant before the bytes moved makes it rare — it
// arises only where the chart moved between the two. The day's quota is NOT
// ([RefusalPushQuota]): a seat whose pushes this company day — every
// repository together, [Limits.PushesPerSeatDay] — reach the bound has its
// push refused WHOLE, no entry of the record, the batch's other entries going
// on, because a recorded refusal would cost the log a record a push of a
// seat pushing in a loop, the very cost the quota bounds. The quota is judged
// on a push that would otherwise be an entry — after every rule of the push's
// own, so a stale, protected, oversized or --atomic push is told the reason
// running it again tomorrow would not change, and is no entry to count — and
// before the pack bound, so a push past both is told the bound that lasts a
// day rather than sent to run again in a minute.
//
// THE COUNT IS ONE ROW A SEAT AND DAY (code_seat_days), read once a round for
// every seat the batch names, which the push apply keeps from the day this
// decide stamps on every seat's entry ([PushEntry.Day]) — never a count over
// the push history, whose index would cost every seat's push an entry for the
// 181 days the history keeps, on every data node, to answer a question about
// one day. Nor the usage domain's days: they count no push, and each is one
// node's own derivation that a read joins later, which no decide can be held
// to; the counter is the applier's, a function of this log alone, so every
// data node's decide reads the same count.
//
// It counts across REPOSITORIES, and a repository's committer does not see the
// seat's pushes that other repositories' committers decided and this node has
// not applied, so a seat may land past its bound by at most (C − 1) × min(the
// room left, its entries in one batch, at most MaxPushesPerRecord) for C
// committers deciding its pushes in one instant — never more than its pushes
// in flight at once less one, since a turn's tool calls run one after another,
// and never cumulative: once its count of a day passes the bound, every later
// batch deciding on that day refuses it, on a committer whose clock is still
// on the day before another's as on any — the push apply keeps a seat's two
// newest days ([countSeatDays]) — as long as no two committers' clocks stand
// a day apart, which clock_skew reports long before, at an hour off the
// fleet's. ACCEPTED as the product's call: an exact count would make every
// repository's decide of one seat arbitrate on one subject, putting the
// pushes of a seat working in many repositories behind one another to bound
// an overshoot that is a few pushes on a bound of a thousand.
//
// # A push checked before an expunge began is refused whole, before any rule
//
// An expunge's begin leaves dropping what no ref reached at its scan, and its
// commit retires those packs but for the rescue its builder read off the refs
// at a sync of its own ([ExpungeBegin], [ExpungeCommit]); the commit's decide
// reads no ref, so a ref landed onto a dropping object after that sync names
// an object the commit retires. A receiver checks a push's objects against
// its synced copy, and a copy synced before the begin holds those packs'
// objects live, so a command of a push checked against it may move a ref onto
// one of them. The copy's drop epoch rides the proposal ([Proposal.DropEpoch]),
// and a push carrying another than the row's is refused WHOLE, expunging,
// every command the receiver did not refuse under one sentence
// ([ExpungingDetail]): it is no entry, nothing of it is decided, and it lands
// when it is run again against a copy synced past the begin. Judged once the
// repository's status admits pushes — an archived or gone repository refuses
// the push whatever copy checked it — and BEFORE every rule of the push's
// own, because those would judge commands checked against objects the
// repository no longer offers a push; a deletion relies on no object, but
// the refusal is of the copy, not of a command, and a push is one answer. A
// begin is rare — one an expunge, and a second waits for the first's commit —
// so a retry meets it again only past another whole expunge.
//
// # The pack bound refuses a push whole, and decides nothing of it
//
// A repository's pushes outrun its compaction only so far: a push that would
// be an entry and CARRIES A PACK, into a repository holding MaxPacksPerRepo
// live packs or more — its batch's earlier entries' packs counted in
// ([working.behind]) — is refused WHOLE, compaction_behind, every command the
// receiver did not refuse under one sentence ([BehindDetail]), the ones this
// decide found ok among them; it is no entry, nothing of it is decided, and it
// lands when it is run again once a compaction has merged the packs. Judged
// LAST, after every rule of the push's own: a push refused for a reason of its
// own — stale, a protection, its pack's size — is told that reason, which
// running it again would not change, and adds no pack, so it counts for
// nothing; and a push carrying no pack adds none, so a deletion, or a ref
// moved onto a commit the repository already holds, is decided as ever, at the
// bound or past it. The receiver refuses the same push before it uploads the
// push's bundle, from its own synced row read again (internal/code/host's
// precheck), which spares every refused retry an upload; this decide, from the
// rows of its one snapshot, is the authority.
//
// # A push's changes are counted, and one that does not fit leads the next
//
// A push record writes the rows of the live changes its pushes move — a
// revision, an author, a merge, a close (revisions.go) — and of the change a
// push opens from its options, decided on the state the push leaves
// (pushchange.go: its row, its owner as its first author, the push as its
// first revision, its description), beside its entries' own, so the decide
// counts them as it admits each entry: the live changes the
// batch's branch commands can move, and the authors its pushers already are of
// them, read ONCE a round into one model ([changeBook]), moved by every entry
// it admits as the apply will move it, and the record held to [MaxRecordRows]
// with what that model writes. Each entry is tried on the model itself and
// taken back where the record cannot hold it ([bookTrial]), so an entry costs
// the round what its own commands reach, never what the batch's do. The
// batcher cannot count them — they are what the repository holds, which only
// a snapshot reads — so a push whose changes' rows would take the record past
// its bound beside the entries before it is CARRIED: the round decides
// nothing of it or of any push after it, and the committer runs them, in the
// order they arrived, as its next batch, the carried push first
// ([Committer.commit]). There its rows are the record's first; a push whose
// changes' rows would take even a record of its own past the bound — weighed
// against the changes as the entries before it leave them, since those land
// first — is refused WHOLE, too_many_changes ([RefusalTooManyChanges]),
// nothing of it decided. Judged after the pack bound, so a push past both is
// told the bound that clears in a minute.
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
// and one that creates none reads neither; a batch carrying a SEAT'S push
// reads one more, every seat's count of the day over the counter's key, and
// one of persons' pushes alone reads none; a batch whose commands name a
// branch reads up to three more, each driven from its list — the live changes
// its branches head, the ones whose targets it sets to their heads, and the
// authors its pushers are of the changes whose heads they move (revisions.go);
// and a batch whose pushes ask for a change reads one more, the branches their
// changes propose into that no command of it names (pushchange.go).
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
// only when they differ — a record changing THIS repository's protections,
// an activation's settings or a tool's protect request, landing between the
// read and the round or between two rounds. Every other round, the first
// included, pays the reads alone. The rules a round reads are what a push is
// judged by and nothing more ([PushRule]): a rule's merge gate holds merges,
// so neither read selects one ([pushRulesIn]).

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
	stored []PushRule
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
func (v *verdicts) under(stored []PushRule) error {
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
// rules, for a push its principal of kind by makes, formed the first time a
// round asks and kept for the rest — KEYED BY THE PUSH, unchanged by the kind,
// since a push has one principal and is the same proposal in every round.
func (v *verdicts) verdict(push, command int, c Command, by PrincipalKind) (Refusal, string) {
	key := [2]int{push, command}
	if got, ok := v.of[key]; ok {
		return got.refusal, got.detail
	}
	var got verdict
	if r, rule := ProtectionRefusal(c, v.rules, by); r != "" {
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

	var stored []PushRule
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		stored, err = pushRulesIn(ctx, tx, repoID)
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
				v.verdict(push, command, c, p.Principal.Kind)
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
	// refused is a refusal of the whole push — repo_deleted, repo_archived,
	// expunging, compaction_behind, too_many_changes — and detail its
	// sentence, which every command it covers carries too.
	refused Refusal
	detail  string
	// carried is a push this round decided nothing of, which leads the
	// committer's next batch: the first whose changes' rows would take the
	// record past its bound beside the entries before it, and every push
	// after it, so the batch's pushes still land in the order they arrived.
	carried bool
	// err is a proposal no rule decides: its push id names a push to
	// another repository.
	err error
	// dupOf is the index of an earlier proposal in the batch with this
	// proposal's push id, whose answer is this one's; -1 for none.
	dupOf int
	// change is what the proposal's change came to ([pushOpen]) for one
	// whose entry is in the round's record; nil for every other, and for
	// one that asked for no change.
	change *ChangeAnswer
}

// decideBatch is one round's decision over the rows tx holds, for the batch in
// order: the entries of the record, every proposal's answer, the rows the
// record's apply will write about the live changes its entries move
// ([changeBook]), and the version the decision read. Pure over (the rows, the
// batch, limits, retention, seats, now): retention is
// code.deleted_retention_days on the deciding node's epoch, from which a
// deleted repository's refusal names when its purge may begin; seats is that
// epoch's chart and calendar, which a seat's push is judged by and which a
// batch carrying none may leave unset; v is the protection verdicts the
// batch's earlier rounds formed, which this round reads only where its own
// snapshot holds the rules they were formed under.
//
// A SEAT'S PUSH IS DECIDED ONLY WITH A CHART. The committer hands every one on
// to the repository's next committer before a batch with no chart is decided
// ([Committer.commit]), so a seat's push reaching here with none is the
// caller's bug, refused as one — never a refusal recorded that no chart made,
// and never a push let through by the grant nobody judged ([GrantOf] answers
// an agent nothing on a nil roster).
func decideBatch(ctx context.Context, tx *sql.Tx, repoID string, batch []Proposal,
	limits Limits, retention time.Duration, seats SeatRules, now time.Time, v *verdicts) (entries []PushEntry,
	answers []decided, changeRows int, version int64, err error) {

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
		return nil, nil, 0, 0, err
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
		return nil, nil, 0, 0, err
	}
	var refused Refusal
	var detail string
	switch {
	case !found, repo.status == RepoStatusDeleted, repo.status == RepoStatusPurging:
		// WHICH OF FOUR STATES, because each has its own remedy
		// ([RepoGone]): a deleted repository comes back when declared again,
		// one being purged or purged never does.
		if detail, err = goneDetail(ctx, tx, repoID, repo, found, retention); err != nil {
			return nil, nil, 0, 0, err
		}
		refused = RefusalRepoDeleted
	case repo.status == RepoStatusArchived:
		refused = RefusalRepoArchived
		detail = ArchivedDetail(repo.name, repo.managedBy)
	}
	if refused != "" {
		for _, i := range open {
			answers[i].refused, answers[i].detail = refused, detail
			answers[i].commands = refuseEvery(batch[i].Commands, refused, detail)
		}
		return nil, answers, 0, repo.version, nil
	}
	// THE DROP EPOCH, before every rule of the push's own: a push checked
	// against a copy at another epoch is refused whole, and the rest of the
	// round reads nothing of it.
	open = slices.DeleteFunc(open, func(i int) bool {
		p := batch[i]
		if p.DropEpoch == repo.dropEpoch {
			return false
		}
		detail := ExpungingDetail(repo.name, p.DropEpoch, repo.dropEpoch)
		answers[i].refused, answers[i].detail = RefusalExpunging, detail
		answers[i].commands = refuseEvery(p.Commands, RefusalExpunging, detail)
		return true
	})

	if seats.Roster == nil && slices.ContainsFunc(open, func(i int) bool {
		return batch[i].Principal.Kind.readsChart() || batch[i].Change != nil
	}) {
		return nil, nil, 0, 0, fmt.Errorf("code: a seat's push to %s reached the decide with no chart to judge "+
			"it by — the committer hands such a push to the repository's next committer", repoID)
	}
	stored, err := pushRulesIn(ctx, tx, repoID)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// PARSED ONCE A BATCH, never once a command and rule (refusal.go), and
	// again only in a round whose rules are not the ones parsed.
	if err = v.under(stored); err != nil {
		return nil, nil, 0, 0, fmt.Errorf("code: read %s's protections: %w", repoID, err)
	}
	w, err := newWorking(ctx, tx, repoID, repo, batch, open, now)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// THE BRANCHES THE BATCH'S CHANGES PROPOSE INTO, where no command of it
	// names them: an open is judged by its target's head as well as its
	// source's, and the working state holds only the refs commands name.
	if err := w.readTargets(ctx, tx, repoID, changeTargets(batch, open, repo)); err != nil {
		return nil, nil, 0, 0, err
	}
	quota, err := newSeatQuota(ctx, tx, batch, open, seats.Zone, now, limits.PushesPerSeatDay)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// THE LIVE CHANGES THE BATCH'S BRANCHES CAN MOVE, and the authors its
	// pushers are of them, read once a round — for every command the
	// receiver did not refuse, a superset of the ones this round finds ok —
	// so the record's change rows are counted as its apply will write them,
	// entry by entry ([changeBook]).
	proposed := make([]PushEntry, 0, len(open))
	for _, i := range open {
		proposed = append(proposed, PushEntry{Principal: batch[i].Principal, Commands: batch[i].Commands})
	}
	book, err := readChangeBook(ctx, tx, repoID, proposed, 0, 0)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// rows is what the record's entries make its apply write besides their
	// changes' rows, as [withinRecord] counts it; carrying is set once a push
	// is carried, which carries every push after it.
	rows, carrying := 0, false
	// room is what the record's entries leave of its bytes for the routings
	// of the changes they merge and open ([routedEntry]).
	room := routingRoom{batch: batch, open: open, repoID: repoID, repo: repo, roster: seats.Roster}
	// THE REPOSITORY AS A GRANT READS IT, from the round's own row: its
	// owner and its visibility, which a seat's relation is judged on.
	ref := repo.grantRef(repoID)
	// THE GATES OF THE BRANCHES A PUSH LANDS ON, read once a round and only
	// for a round with a landing, since a push is judged by its rules'
	// push half alone ([pushRulesIn]).
	landingGates := pushGates{repoID: repoID}
	for _, i := range open {
		if carrying {
			answers[i].carried = true
			continue
		}
		p := batch[i]
		cmds := w.decide(i, p, v, limits, GrantOf(p.Principal, ref, seats.Roster))
		answers[i].commands = cmds
		if !slices.ContainsFunc(cmds, isOK) {
			continue
		}
		// A SEAT'S DAY, then THE PACK BOUND, LAST: each the push whole —
		// what the receiver refused kept, every other command under the
		// one sentence, the ones found ok here among them, since nothing of
		// it is decided.
		if detail, over := quota.over(p); over {
			answers[i].refused, answers[i].detail = RefusalPushQuota, detail
			answers[i].commands = refuseEvery(p.Commands, RefusalPushQuota, detail)
			continue
		}
		if detail, behind := w.behind(p); behind {
			answers[i].refused, answers[i].detail = RefusalCompactionBehind, detail
			answers[i].commands = refuseEvery(p.Commands, RefusalCompactionBehind, detail)
			continue
		}
		// AND THE CHANGES IT MOVES, counted as the record's apply will write
		// them, after the entries before it: a push whose changes' rows take
		// the record past its bound is decided nothing of — refused whole,
		// too_many_changes, where even a record of its own could never hold
		// them, and carried to lead the next batch where one could. TRIED ON
		// THE BOOK ITSELF and taken back where it does not fit ([bookTrial]):
		// what an entry costs is what its own commands reach.
		entry := p.entryWith(cmds, quota.day.Label)
		// AND THE LANDINGS IT MAKES on a protected branch — a person's own
		// push, their recorded override — stamped from this snapshot's rules,
		// so the apply matches no pattern.
		if entry.Landings, err = landingGates.landings(ctx, tx, v.rules, cmds); err != nil {
			return nil, nil, 0, 0, err
		}
		merged := len(book.writes.merged)
		book.try()
		book.move(entry)
		// AND THE CHANGE IT ASKS FOR, opened in its own entry where its rules
		// let it ([pushOpen]) — after the entry's own moves, since a push to
		// a branch heading a live change revises that one rather than opens
		// another — and its rows counted with the record's.
		var change *ChangeAnswer
		var opened string
		if p.Change != nil {
			entry.Change, change = pushOpen(p, cmds, w, book, repo)
			if entry.Change != nil {
				opened, _ = book.open(entry)
			}
		}
		own := entryRows(entry)
		if recordRows(rows+own+book.rowCount()) > MaxRecordRows {
			// ALONE is the push leading the next record, the entries before
			// it landed: what it would cost there decides whether it can
			// land at all.
			book.takeBack()
			changeRows, moved := book.alone(entry)
			if alone := recordRows(own + changeRows); alone > MaxRecordRows {
				detail := tooManyChangesDetail(moved, len(entry.Landings), alone)
				answers[i].refused, answers[i].detail = RefusalTooManyChanges, detail
				answers[i].commands = refuseEvery(p.Commands, RefusalTooManyChanges, detail)
				continue
			}
			answers[i].carried, carrying = true, true
			continue
		}
		// AND WHOM IT CONCERNS: the changes it merges and the one it opens,
		// each routed in this snapshot — carried to lead the next record
		// where the routings do not fit this one's bytes beside the entries
		// already in it ([routingRoom]).
		routings, routeErr := room.route(ctx, tx, book, entry, book.writes.merged[merged:], opened)
		if routeErr != nil {
			return nil, nil, 0, 0, routeErr
		}
		fits, rest, fitErr := room.fit(i, len(entries) == 0, routings,
			MaxRecordRows-recordRows(rows+own+book.rowCount()))
		if fitErr != nil {
			return nil, nil, 0, 0, fitErr
		}
		if !fits {
			book.takeBack()
			answers[i].carried, carrying = true, true
			continue
		}
		entry.Routing = room.kept
		carrying = rest
		book.keep()
		rows += own
		w.apply(p, cmds)
		quota.count(p)
		answers[i].inRecord, answers[i].change = true, change
		entries = append(entries, entry)
	}
	return entries, answers, book.rowCount(), repo.version, nil
}

// seatQuota is one round's count of each seat's pushes on the deciding
// company day ([Limits.PushesPerSeatDay]): what the counter holds for every
// seat the round's open proposals name — code_seat_days, read ONCE a round
// inside its snapshot, for a round carrying a seat's push at all — and the
// entries of each seat the round's record carries so far.
type seatQuota struct {
	// day is the company day the round counts against, on the deciding
	// node's clock in the company's zone; its zero value for a round that
	// carries no seat's push, which counts nothing.
	day period.Window
	// limit is code.limits.pushes_per_seat_day on the deciding epoch.
	limit int
	// held is each seat's count of the day as the counter holds it, no row
	// being zero; carried, each seat's entries this round's record carries
	// so far.
	held, carried map[string]int
}

// newSeatQuota reads the counts of the day now falls on, on the calendar of
// zone, for every seat whose push is among the open proposals of batch, in ONE
// statement over the counter's key — none at all for a round of persons'
// pushes alone.
func newSeatQuota(ctx context.Context, tx *sql.Tx, batch []Proposal, open []int, zone *time.Location,
	now time.Time, limit int) (seatQuota, error) {

	q := seatQuota{limit: limit, held: map[string]int{}, carried: map[string]int{}}
	var seats []string
	for _, i := range open {
		if p := batch[i].Principal; p.Kind == PrincipalAgent && !slices.Contains(seats, p.Seat) {
			seats = append(seats, p.Seat)
		}
	}
	if len(seats) == 0 {
		return q, nil
	}
	if q.day = period.At(period.Day, now, zone); q.day.Label == "" {
		// A CLOCK NO LABEL SPELLS — past the year 9999 — has no company
		// day to count a seat's push against, and a day of "" would count
		// every such push against nothing.
		return seatQuota{}, fmt.Errorf("code: the deciding node's clock reads %s, which no company day "+
			"spells, so no seat's push can be counted against one", now.UTC().Format(time.RFC3339))
	}
	list, err := jsonList(seats)
	if err != nil {
		return seatQuota{}, err
	}
	rows, err := tx.QueryContext(ctx, seatDaysSQL, q.day.Label, list)
	if err != nil {
		return seatQuota{}, fmt.Errorf("code: read the seats' pushes on %s: %w", q.day.Label, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seat string
		var pushes int
		if err := rows.Scan(&seat, &pushes); err != nil {
			return seatQuota{}, fmt.Errorf("code: read the seats' pushes on %s: %w", q.day.Label, err)
		}
		q.held[seat] = pushes
	}
	if err := rows.Err(); err != nil {
		return seatQuota{}, fmt.Errorf("code: read the seats' pushes on %s: %w", q.day.Label, err)
	}
	return q, nil
}

// over reports a SEAT'S push its day's count refuses, with the sentence: the
// seat's pushes this day as the counter holds them, its entries this round's
// record carries ahead of p, at the bound or past it. A push by anybody else
// is never counted.
func (q seatQuota) over(p Proposal) (string, bool) {
	if p.Principal.Kind != PrincipalAgent {
		return "", false
	}
	pushed := q.held[p.Principal.Seat] + q.carried[p.Principal.Seat]
	if pushed < q.limit {
		return "", false
	}
	return quotaDetail(p.Principal.Seat, pushed, q.limit, q.day), true
}

// count counts p, an entry of the round's record, against its seat's day.
func (q seatQuota) count(p Proposal) {
	if p.Principal.Kind == PrincipalAgent {
		q.carried[p.Principal.Seat]++
	}
}

// quotaDetail is the sentence of a seat's push refused push_quota: who, how
// many pushes it has made since its company day began, the bound, and the
// instant the count starts again — the day's end on the company's calendar,
// in its zone. Formed by the push decide alone, the one judge of the count.
func quotaDetail(seat string, pushed, limit int, day period.Window) string {
	return fmt.Sprintf("seat %s has landed %s since %s began on the company's calendar, and "+
		"code.limits.pushes_per_seat_day lets a seat land %s a day: nothing was decided; the count starts "+
		"again at %s", seat, pushCount(pushed), day.Label, pushCount(limit), day.End.Format(time.RFC3339))
}

// pushCount is n pushes as a sentence counts them.
func pushCount(n int) string {
	if n == 1 {
		return "1 push"
	}
	return fmt.Sprintf("%d pushes", n)
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

// pushRepo is what a decide reads of a repository's row: a push batch's, a
// compaction's ([decideCompaction]) and an expunge's ([decideExpunge]).
type pushRepo struct {
	name      string
	status    RepoStatus
	refs      int
	liveBytes int64
	// livePacks is the repository's live packs, which the pack bound
	// judges a push carrying one by ([working.behind]).
	livePacks int64
	version   int64
	// deletedAt is when a deleted or purging repository was deleted, and
	// zero for any other.
	deletedAt time.Time
	// dropEpoch is how many expunges have begun on the repository, and
	// expungeID the one open ("" for none) — what an expunge's begin and
	// commit are decided by, a push checked at another epoch refused by,
	// and a publication refused by while one is open. No decide reads the
	// purge requested: a request forms its record whatever is open.
	dropEpoch int64
	expungeID string
	// owner and visibility are what a seat's grant on the repository is
	// judged by ([GrantOf]): its relation through the owner, and the row
	// visibility decides.
	owner      Owner
	visibility Visibility
	// managedBy is who manages it, which the remedy a refusal of a deleted
	// or archived repository names follows ([RepoGone], [ArchivedDetail]).
	managedBy ManagedBy
	// defaultBranch is its default branch, by name ("main"): what a push's
	// change proposes into where its options name no target ([pushOpen]).
	defaultBranch string
}

// grantRef is the repository id names as a grant reads it: its name, which a
// refusal names, its owner and its visibility.
func (r pushRepo) grantRef(id string) RepoRef {
	return RepoRef{ID: id, Name: r.name, Owner: r.owner, Visibility: r.visibility}
}

// pushRepoIn reads a repository's row for a decide.
func pushRepoIn(ctx context.Context, tx *sql.Tx, id string) (pushRepo, bool, error) {
	var r pushRepo
	var status, ownerKind, visibility, managed string
	var deleted sql.NullInt64
	err := tx.QueryRowContext(ctx, decideRepoSQL, id).Scan(&r.name, &status, &r.refs,
		&r.liveBytes, &r.livePacks, &r.version, &deleted, &r.dropEpoch, &r.expungeID, &ownerKind,
		&r.owner.Ref, &visibility, &managed, &r.defaultBranch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pushRepo{}, false, nil
	case err != nil:
		return pushRepo{}, false, fmt.Errorf("code: read repository %s for its pushes: %w", id, err)
	}
	r.status, r.owner.Kind, r.visibility = RepoStatus(status), OwnerKind(ownerKind), Visibility(visibility)
	r.managedBy = ManagedBy(managed)
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
	// packs is the repository's live packs, the batch's earlier entries'
	// counted in: what the pack bound judges a push carrying one by
	// ([working.behind]).
	packs int64
	// expiry is, for a batch holding an expiry entry and for no other, the
	// row of every ref those entries name as rule 4b judges it
	// ([ExpiryRow], read by expiryRefsSQL in the round's snapshot); touched,
	// every ref an earlier entry of the batch set ok, whose snapshot row
	// predates that push.
	expiry  map[string]ExpiryRow
	touched map[string]bool
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
		packs: repo.livePacks, occupied: map[string]string{}, touched: map[string]bool{}}
	if err := w.readAround(ctx, tx, repoID, batch, open, seen, deleted); err != nil {
		return nil, err
	}
	if w.expiry, err = expiryRowsIn(ctx, tx, repoID, repo, batch, open); err != nil {
		return nil, fmt.Errorf("code: read the refs an expiry names: %w", err)
	}
	return w, nil
}

// expiryRowsIn reads, ONLY for a batch whose open proposals hold an expiry
// entry, the row rule 4b judges of every ref those entries name — in ONE
// statement over code_refs' key, driven from the list (planner.go), with a
// seek of each live-change index — so a batch of persons' and seats' pushes
// pays nothing for it; whether a ref is the default branch is the round's own
// repository row's. nil for every other batch.
func expiryRowsIn(ctx context.Context, tx *sql.Tx, repoID string, repo pushRepo, batch []Proposal,
	open []int) (map[string]ExpiryRow, error) {

	var refs []string
	for _, i := range open {
		if batch[i].Via != ViaExpiry {
			continue
		}
		for _, c := range batch[i].Commands {
			refs = append(refs, c.Ref)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	list, err := jsonList(refs)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, expiryRefsSQL, repoID, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ExpiryRow{}
	for rows.Next() {
		var ref string
		var at int64
		var row ExpiryRow
		if err := rows.Scan(&ref, &row.Target, &row.Seat, &at, &row.Held, &row.Change); err != nil {
			return nil, err
		}
		row.At, row.Default = store.DecodeTime(at), ref == branchPrefix+repo.defaultBranch
		out[ref] = row
	}
	return out, rows.Err()
}

// expiryRefsSQL is rule 4b's row of each ref of a list (?2) in the repository
// ?1: code_refs' key, driven from the list, and an EXISTS seek of the live
// changes by source (code_changes_open_idx) and by target
// (code_changes_target_idx, its leading two columns), the live states spelled
// as both indexes are partial on them, unqualified (no other table here has
// a state). Registered in plan_test.go.
const expiryRefsSQL = `SELECT r.ref, r.target, r.seat, r.at, r.held_position,
		EXISTS (SELECT 1 FROM code_changes AS s INDEXED BY code_changes_open_idx
			WHERE s.repo_id = ?1 AND s.source_ref = r.ref AND ` + liveChanges + `)
		OR EXISTS (SELECT 1 FROM code_changes AS t INDEXED BY code_changes_target_idx
			WHERE t.repo_id = ?1 AND t.target_ref = r.ref AND ` + liveChanges + `)
	FROM json_each(?2) AS j CROSS JOIN code_refs AS r
	WHERE r.repo_id = ?1 AND r.ref = CAST(j.value AS TEXT)`

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

// after is ref's value once the push being decided has applied as well as the
// batch's entries before it: its ok command on ref where it has one —
// ZeroOID for a deletion — and otherwise what the working state holds.
func (w *working) after(cmds []Command, ref string) string {
	for _, c := range cmds {
		if c.Ref == ref && c.Result == ResultOK {
			return c.New
		}
	}
	return w.current(ref)
}

// readTargets reads, in one statement a part, the rows of the refs that no
// command of the batch names — the branches its changes propose into
// ([changeTargets]) — into the working state, which then answers them as it
// answers the refs its commands name. They take no part in what is around a
// create (readAround, read before): no command moves them, so no create is
// beside one that the batch could change.
func (w *working) readTargets(ctx context.Context, tx *sql.Tx, repoID string, refs []string) error {
	var unread []string
	for _, ref := range refs {
		if _, held := w.targets[ref]; !held {
			unread = append(unread, ref)
		}
	}
	return inChunks(unread, func(part []string) error {
		rows, err := refsNamed(ctx, tx, repoID, part)
		if err != nil {
			return fmt.Errorf("code: read the branches a batch's changes propose into: %w", err)
		}
		maps.Copy(w.targets, rows)
		return nil
	})
}

// changeTargets is the branch each open proposal's change proposes into — the
// one its options name, else the repository's default branch — each once.
func changeTargets(batch []Proposal, open []int, repo pushRepo) []string {
	var out []string
	seen := map[string]bool{}
	for _, i := range open {
		if c := batch[i].Change; c != nil {
			if ref := changeTarget(c, repo); !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

// changeTarget is the branch c proposes into: the one its options name, else
// the repository's default branch, as a ref.
func changeTarget(c *ChangeOption, repo pushRepo) string {
	if c.Target != "" {
		return c.Target
	}
	return branchPrefix + repo.defaultBranch
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
// that becomes an entry) — and against g, its principal's grant on the
// repository. push is p's place in the batch, which its protection verdicts
// are kept under.
//
// A SEAT OUTSIDE THE REPOSITORY'S OWNERS ([Grant.outside]) is held to
// [OutsideSeatShare] of its live refs and of code.limits.repo_max_bytes,
// judged on the repository's whole counts as everybody's are, and every other
// principal to the whole of each.
func (w *working) decide(push int, p Proposal, v *verdicts, limits Limits, g Grant) []Command {
	refBound, byteBound := MaxRefsPerRepo, limits.RepoMaxBytes
	if g.outside() {
		refBound, byteBound = MaxRefsPerRepo/OutsideSeatShare, limits.RepoMaxBytes/OutsideSeatShare
	}
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
		// 2b THE GRANT, before any state of the ref is read: a command the
		// principal may not make is refused for that, whatever the ref holds.
		if r, why := g.RefRule(*c); r != "" {
			refuse(c, r, why)
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
		// 4b AN EXPIRY'S BRANCH THAT IS NO LONGER IDLE, after rule 4 so a
		// branch moved since its listing is told it moved. Its row is in
		// the snapshot, since the branch is live in it; one missing there is
		// read as a branch no seat set, kept.
		if p.Via == ViaExpiry {
			if r, why := ExpiryRule(w.expiry[c.Ref], w.touched[c.Ref], p.ReceivedAt, p.Seen.Packed()); r != "" {
				refuse(c, r, why)
				continue
			}
		}
		if r, detail := v.verdict(push, i, *c, p.Principal.Kind); r != "" {
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
		if held := w.live - deletes + creates; held >= refBound {
			refuse(c, RefusalRepoRefs, refsDetail(w.name, held, refBound, g))
			continue
		}
		creates++
		made[c.Ref] = true
		c.Result = ResultOK
	}
	if p.Pack != nil && slices.ContainsFunc(cmds, isOK) {
		detail, expired := uploadExpired("the pack", p.Pack.Object, w.now, "run git push again")
		switch {
		case expired:
			refuseOK(cmds, RefusalUploadExpired, detail)
		case w.bytes+p.Pack.Size > byteBound:
			refuseOK(cmds, RefusalRepoSize, sizeDetail(w.name, w.bytes+p.Pack.Size, byteBound, limits, g))
		}
	}
	if p.Atomic {
		cmds = Atomic(cmds)
	}
	return cmds
}

// refsDetail is the sentence of a create refused repo_refs: the repository
// holds held live refs, at or past bound — the most it holds, or for a seat
// outside its owners the half of that such a seat may fill, which the
// sentence then says, naming whose the rest is.
func refsDetail(name string, held, bound int, g Grant) string {
	if !g.outside() {
		return fmt.Sprintf("%s holds %d live refs, the most one repository may", name, held)
	}
	return fmt.Sprintf("%s holds %d live refs, and %s creates refs there only while %s holds fewer than %d: "+
		"half of the %d one repository may, the other half kept for its owners", name, held,
		outsider(g.Owner), name, bound, MaxRefsPerRepo)
}

// sizeDetail is the sentence of a push refused repo_size: the repository
// would hold would of packs, past bound — code.limits.repo_max_bytes, or for
// a seat outside its owners the half of it such a seat may fill, which the
// sentence then says, naming whose the rest is.
func sizeDetail(name string, would, bound int64, limits Limits, g Grant) string {
	if !g.outside() {
		return fmt.Sprintf("%s would hold %s of packs, past %s (code.limits.repo_max_bytes)", name,
			gib(would), gib(bound))
	}
	return fmt.Sprintf("%s would hold %s of packs, past %s: %s fills at most half of "+
		"code.limits.repo_max_bytes (%s), the other half kept for its owners", name, gib(would), gib(bound),
		outsider(g.Owner), gib(limits.RepoMaxBytes))
}

// outsider names a seat outside a repository's owners, by who owns it.
func outsider(o Owner) string {
	if o.Kind == OwnerSeat {
		return fmt.Sprintf("a seat other than %s, which owns it,", o.Ref)
	}
	return fmt.Sprintf("a seat outside unit %s, which owns it,", o.Ref)
}

// uploadExpired is the one rule a decide holds a stored object's key to: an
// object whose key was minted objstore.RecordWithin or more before the
// decision may no longer be named by a record — a collection may already have
// judged so old a key unnamed and deleted its bytes, and a row naming it would
// name bytes that are gone — and the sentence that says so, beginning with
// what, the object it names, and ending in again, what its asker does next.
// ONE RULE AND ONE SENTENCE for every decide that names a stored object
// (ADR-0008): a push's pack, a compaction's outputs and a publication's layer
// parts.
func uploadExpired(what string, object objstore.Key, now time.Time, again string) (string, bool) {
	minted := object.Minted()
	if now.Sub(minted) < objstore.RecordWithin {
		return "", false
	}
	return fmt.Sprintf("%s was uploaded at %s, %s or more before this decision; %s", what,
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

	gone := RepoGone{ID: repoID, Name: repo.name, Status: repo.status, DeletedAt: repo.deletedAt, ManagedBy: repo.managedBy,
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
		w.touched[c.Ref] = true
	}
	if p.Pack != nil {
		w.bytes += p.Pack.Size
		w.packs++
	}
}

// behind is the pack bound over p, a push with an ok command left — an entry
// of the record unless this refuses it: true, with the sentence of its
// refusal, when p carries a pack and the repository holds MaxPacksPerRepo live
// packs or more with the batch's earlier entries' packs counted in. Asked
// LAST, so a push refused for a reason of its own is told that reason and
// counts no pack; and only of a push carrying a pack, since one carrying
// none adds none — a deletion, or a ref moved onto a commit the repository
// already holds, is decided at the bound and past it as ever. A push it
// refuses is never applied, so its pack is counted for no later push.
func (w *working) behind(p Proposal) (string, bool) {
	if p.Pack == nil || w.packs < MaxPacksPerRepo {
		return "", false
	}
	return BehindDetail(w.name, w.packs), true
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
// refused whole as the receiver's own bug. So each sentence is its pattern
// and at most 64 bytes of words, the persons rule's included, which says what
// the rule takes rather than why a seat's change reaches the ref only through
// a person: that is the code host's "What git prints" row.
func ProtectionDetail(rule PushRule, r Refusal) string {
	switch {
	case r == RefusalProtectedDirect && rule.DirectPush == DirectPersons:
		// A PUSH BY ANYBODY BUT A PERSON under a rule that takes a
		// person's: the only way a persons rule refuses anything
		// ([ProtectionRefusal]).
		return fmt.Sprintf("takes a person's push only (protection %s: direct_push persons)", rule.Pattern)
	case r == RefusalProtectedDirect:
		return fmt.Sprintf("takes no direct push (protection %s: direct_push none)", rule.Pattern)
	case r == RefusalProtectedDelete:
		return fmt.Sprintf("may not be deleted (protection %s: allow_delete)", rule.Pattern)
	case r == RefusalProtectedForce:
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
	decideRepoSQL = `SELECT name, status, refs, live_bytes, live_packs, version, deleted_at,
		drop_epoch, expunge_id, owner_kind, owner, visibility, managed_by, default_branch FROM code_repos
		WHERE id = ?`
	decideDeletionSQL = `SELECT name FROM code_deletions WHERE repo_id = ?`
	// seatDaysSQL is each named seat's pushes counted on one company day: ONE
	// row a seat of the counter the push apply keeps (code_seat_days), each
	// a seek of its key driven from the round's list of seats — never a count
	// over the push history.
	seatDaysSQL = `SELECT s.seat, s.pushes FROM json_each(?2) AS j CROSS JOIN code_seat_days AS s
		WHERE s.seat = CAST(j.value AS TEXT) AND s.day = ?1`
	pushesNamedSQL = `SELECT ` + pushColumns + ` FROM code_pushes
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
