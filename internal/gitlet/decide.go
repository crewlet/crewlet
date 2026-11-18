package gitlet

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

// THE PUSH DECIDE: one round of a batch's decision from the rows ONE snapshot
// holds, re-run whole every compare-and-set round, so it reads nothing outside
// tx and keeps only the protection [verdicts] between rounds. Proposals are
// judged in queue order, each against the rows with every earlier entry
// applied, and a command meets the first rule that fires: 1 refused by the
// receiver; 2 [StatelessRefusal]; 2b outside its principal's grant
// ([Grant.RefRule], not_permitted); 3 noop; 4 stale; 4b an expiry's branch no
// longer idle ([ExpiryRule]); 5 a protection ([ProtectionRefusal]); 5b
// default_branch ([DefaultBranchRefusal]); 6 ref_conflict ([working.beside]);
// 7 repo_refs; 8 ok. Then upload_expired, repo_size and --atomic over the push
// ([working.decide]), and the refusals of a push whole ([decideBatch]).

// verdicts are one batch's protection verdicts, kept across the rounds of its
// one publish while each round's snapshot holds the rules they were formed
// under ([verdicts.under]); the zero value holds none. A verdict is a pure
// function of the command and the rules, and asking every rule about every
// command is the one decide cost that grows with what a founder writes: at
// gitref's costliest rule shape, a record of 255-byte refs against
// [MaxProtectionsPerRepo] rules is about two seconds of a core (doc.go's
// measured section). So they are formed once, before the publish
// ([verdicts.formBefore]), and again inside a round only when a record changed
// the rules in between. Only a rule's push half is read ([pushRulesIn]): its
// merge gate holds merges.
type verdicts struct {
	// set is whether stored and rules are a round's: the zero value has
	// read no rules, which is not the same as a repository with none.
	set bool
	// stored are the rules every verdict below was formed under, as the
	// snapshot held them, and rules the same parsed.
	stored []PushRule
	rules  Protections
	// of is each command's verdict, keyed by (proposal index, command index): a
	// batch is the same proposals in every round of its publish.
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

// under makes stored the rules the verdicts answer from, keeping those already
// formed only when stored equals the rules they were formed under, field by
// field and in order.
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

// verdict is the verdict on c, command of the proposal at push, for a push by a
// principal of kind by: formed on first ask and kept. Keyed by place, never by
// kind, since a push has one principal and is the same proposal in every round.
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

// formBefore forms, before the batch's publish, the verdict of every command a
// round can ask about, under repoID's rules as this node's rows hold them now.
// Outside the snapshot because forming is CPU alone: inside it, the formation
// would hold one of the reader pool's few connections (four by default) for its
// whole length, so a few repositories at costly rules would starve every
// replicated read on the node and widen the window in which a peer's push
// stales the round. Only the snapshot's rules count: every round compares its
// own with these ([verdicts.under]) and forms again on a difference. So a read
// or parse failing here is not reported: the round reads the same rows and
// answers for them, since this moves when the work is done, never what is
// decided.
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
	// Every command a round may ask about: of the first proposal of each push id
	// (a later one is answered as its first), each command neither the receiver
	// nor a stateless rule refused. A round asks about fewer, as a noop or stale
	// command never reaches the rules; forming those costs only here.
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
	// committer's next batch ([Committer.commit]): the first whose changes' rows
	// would take the record past [MaxRecordRows] beside the entries before it, and
	// every push after it, so the batch still lands in arrival order.
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

// batchMoves is what a round's record does to the live changes its entries
// move, as its decide counted it in the model its apply writes from
// ([changeBook]): the rows the apply writes about them
// ([changeBook.rowCount]), and the changes whose live queue entry it ejects
// because it moved their head ([changeBook.queueEnds]) — which no record of
// the queue's names, so the committer that landed it announces each
// ([Committer.announcePushEjections]).
type batchMoves struct {
	rows    int
	ejected []string
}

// decideBatch is one round's decision over the rows tx holds: the record's
// entries, every proposal's answer, what the apply will do to the live changes
// ([batchMoves]) and the version read. Pure over (rows, batch, limits,
// retention, seats, now): retention is the deciding epoch's
// code.deleted_retention_days, which dates a deleted repository's purge in its
// refusal; seats is that epoch's chart and calendar, unset only for a batch with
// no seat's push; v's verdicts are reused only under the same rules.
//
// A SEAT'S PUSH IS DECIDED ONLY WITH A CHART: the committer hands such pushes to
// the next committer when it has none ([Committer.commit]), so one here is the
// caller's bug, refused as one: never a refusal no chart made, nor a push let
// through by a grant nobody judged ([GrantOf] answers nothing on a nil roster).
func decideBatch(ctx context.Context, tx *sql.Tx, repoID string, batch []Proposal,
	limits Limits, retention time.Duration, seats SeatRules, now time.Time, v *verdicts) (entries []PushEntry,
	answers []decided, moves batchMoves, version int64, err error) {

	// EVERY READ IS ONE STATEMENT OVER A LIST, never a point read a command
	// (planner.go): the decide holds a reader connection and the committer
	// goroutine and is re-run every round, so a per-command read costs commands ×
	// rounds, nearly four thousand point reads a full batch. A read only some
	// batches need is made only by them ([working.readAround], [newSeatQuota],
	// [readChangeBook], [working.readTargets]).
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
	// A PUSH ID ALREADY IN THE HISTORY applied (forwarded twice, or decided again
	// after a failover) and is answered from its row, read BEFORE the repository's
	// status: told repo_archived, its client would push again what already landed.
	landed, err := pushRowsIn(ctx, tx, ids)
	if err != nil {
		return nil, nil, batchMoves{}, 0, err
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
		return nil, nil, batchMoves{}, 0, err
	}
	var refused Refusal
	var detail string
	switch {
	case !found, repo.status == RepoStatusDeleted, repo.status == RepoStatusPurging:
		// WHICH OF FOUR STATES, because each has its own remedy
		// ([RepoGone]): a deleted repository comes back when declared again,
		// one being purged or purged never does.
		if detail, err = goneDetail(ctx, tx, repoID, repo, found, retention); err != nil {
			return nil, nil, batchMoves{}, 0, err
		}
		refused = RefusalRepoDeleted
	default:
		// ARCHIVED, BEING IMPORTED, OR A PULL MIRROR: each takes no push,
		// whoever pushes — a person's included — in the sentence of what it
		// is ([pushRepo.writeRefusal]).
		refused, detail = repo.writeRefusal()
	}
	if refused != "" {
		for _, i := range open {
			answers[i].refused, answers[i].detail = refused, detail
			answers[i].commands = refuseEvery(batch[i].Commands, refused, detail)
		}
		return nil, answers, batchMoves{}, repo.version, nil
	}
	// THE DROP EPOCH, after the status (an archived or gone repository refuses
	// whatever copy checked the push) and before every rule of the push's own: a
	// receiver checks objects against its synced copy, and one synced before an
	// expunge's begin holds live the objects its commit retires ([ExpungeBegin],
	// [ExpungeCommit]), which that commit's decide, reading no ref, cannot catch.
	// A push checked at another epoch ([Proposal.DropEpoch]) is refused WHOLE,
	// expunging, decided nothing of, its deletions too, since the refusal is of
	// the copy, not a command; run against a copy synced past the begin, it lands.
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
		return nil, nil, batchMoves{}, 0, fmt.Errorf("gitlet: a seat's push to %s reached the decide with no chart to judge "+
			"it by — the committer hands such a push to the repository's next committer", repoID)
	}
	stored, err := pushRulesIn(ctx, tx, repoID)
	if err != nil {
		return nil, nil, batchMoves{}, 0, err
	}
	// PARSED ONCE A BATCH, never once a command and rule (refusal.go), and
	// again only in a round whose rules are not the ones parsed.
	if err = v.under(stored); err != nil {
		return nil, nil, batchMoves{}, 0, fmt.Errorf("gitlet: read %s's protections: %w", repoID, err)
	}
	w, err := newWorking(ctx, tx, repoID, repo, batch, open, now)
	if err != nil {
		return nil, nil, batchMoves{}, 0, err
	}
	// THE BRANCHES THE BATCH'S CHANGES PROPOSE INTO, where no command of it
	// names them: an open is judged by its target's head as well as its
	// source's, and the working state holds only the refs commands name.
	if err = w.readTargets(ctx, tx, repoID, changeTargets(batch, open, repo)); err != nil {
		return nil, nil, batchMoves{}, 0, err
	}
	quota, err := newSeatQuota(ctx, tx, batch, open, seats.Zone, now, limits.PushesPerSeatDay)
	if err != nil {
		return nil, nil, batchMoves{}, 0, err
	}
	// THE LIVE CHANGES THE BATCH'S BRANCHES CAN MOVE and its pushers' authorship
	// of them, read once a round for every command the receiver did not refuse (a
	// superset of this round's ok ones), so the record's change rows are counted
	// as its apply writes them ([changeBook]).
	proposed := make([]PushEntry, 0, len(open))
	for _, i := range open {
		proposed = append(proposed, PushEntry{Principal: batch[i].Principal, Commands: batch[i].Commands})
	}
	book, err := readChangeBook(ctx, tx, repoID, proposed, 0, 0)
	if err != nil {
		return nil, nil, batchMoves{}, 0, err
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
		// AN ATTEMPT ([Attempted]): a git push every command of which a rule on who may
		// move which ref refused, recorded moving nothing and naming no pack or change
		// (the collector takes its stored pack), so its history says who tried; it
		// counts against a seat's day. Any other push with no ok command writes nothing.
		attempt := !slices.ContainsFunc(cmds, isOK)
		if attempt && (p.Via != ViaGit || !Attempted(cmds)) {
			continue
		}
		if attempt {
			// A decide-made attempt keeps its receiver's listing as what it offered.
			p.Offered = max(p.Offered, p.IntroducedCount)
			p.Pack, p.Introduced, p.IntroducedCount, p.Change = nil, nil, 0, nil
		}
		// A SEAT'S DAY, then THE PACK BOUND, LAST: each the push whole —
		// what the receiver refused kept, every other command under the
		// one sentence, the ones found ok here among them, since nothing of
		// it is decided. An attempt past the day keeps its own refusals and
		// is recorded nowhere.
		if detail, over := quota.over(p); over {
			if attempt {
				continue
			}
			answers[i].refused, answers[i].detail = RefusalPushQuota, detail
			answers[i].commands = refuseEvery(p.Commands, RefusalPushQuota, detail)
			continue
		}
		if detail, behind := w.behind(p); behind {
			answers[i].refused, answers[i].detail = RefusalCompactionBehind, detail
			answers[i].commands = refuseEvery(p.Commands, RefusalCompactionBehind, detail)
			continue
		}
		// AND THE CHANGES IT MOVES, counted as the record's apply will write them,
		// after the entries before it: a push whose changes' rows take the record past
		// its bound is decided nothing of, refused whole (too_many_changes) where even a
		// record of its own could not hold them, and carried to lead the next batch
		// where one could. After the pack bound, so a push past both is told the bound
		// that clears in a minute. TRIED ON THE BOOK ITSELF and taken back where it does
		// not fit ([bookTrial]): an entry costs what its own commands reach.
		entry := p.entryWith(cmds, quota.day.Label)
		// AND THE LANDINGS IT MAKES on a protected branch — a person's own
		// push, their recorded override — stamped from this snapshot's rules,
		// so the apply matches no pattern.
		if entry.Landings, err = landingGates.landings(ctx, tx, v.rules, cmds); err != nil {
			return nil, nil, batchMoves{}, 0, err
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
		if recordRows(rows+own+book.decidedRows()) > MaxRecordRows {
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
			return nil, nil, batchMoves{}, 0, routeErr
		}
		fits, rest, fitErr := room.fit(i, len(entries) == 0, routings,
			MaxRecordRows-recordRows(rows+own+book.decidedRows()))
		if fitErr != nil {
			return nil, nil, batchMoves{}, 0, fitErr
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
	ejected, _, _ := book.queueEnds()
	return entries, answers, batchMoves{rows: book.decidedRows(), ejected: ejected}, repo.version, nil
}

// seatQuota is one round's count of each seat's pushes on the deciding company
// day ([Limits.PushesPerSeatDay]): the counter's rows for every seat the open
// proposals name, and each seat's entries the record carries so far. Unlike a
// grant's refusal (rule 2b, recorded as an audit fact), the quota refuses a
// push WHOLE and unrecorded ([RefusalPushQuota]): recording it would cost the
// log a record for every push of a seat pushing in a loop, the very cost the
// quota bounds. Judged after every rule of the push's own, so a push is told a
// reason a retry tomorrow would not change (an attempt counts, and one past the
// bound is recorded nowhere), and before the pack bound, so a push past both is
// told the bound that lasts a day.
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

// newSeatQuota reads the counts of the day now falls on in zone, for every seat
// with a push among the open proposals, in ONE statement over the counter's key
// (none for persons' pushes alone). The counter (code_seat_days) is one row a
// seat and day, kept by the push apply from the day this decide stamps on each
// entry ([PushEntry.Day]): never a count over the push history, whose index
// would cost every seat's push a row for the history's 181 days on every data
// node, nor the usage domain's days, which count no push and are each one
// node's own derivation. The applier's counter is a function of this log alone,
// so every data node's decide reads the same count.
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
		return seatQuota{}, fmt.Errorf("gitlet: the deciding node's clock reads %s, which no company day "+
			"spells, so no seat's push can be counted against one", now.UTC().Format(time.RFC3339))
	}
	list, err := jsonList(seats)
	if err != nil {
		return seatQuota{}, err
	}
	rows, err := tx.QueryContext(ctx, seatDaysSQL, q.day.Label, list)
	if err != nil {
		return seatQuota{}, fmt.Errorf("gitlet: read the seats' pushes on %s: %w", q.day.Label, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seat string
		var pushes int
		if err := rows.Scan(&seat, &pushes); err != nil {
			return seatQuota{}, fmt.Errorf("gitlet: read the seats' pushes on %s: %w", q.day.Label, err)
		}
		q.held[seat] = pushes
	}
	if err := rows.Err(); err != nil {
		return seatQuota{}, fmt.Errorf("gitlet: read the seats' pushes on %s: %w", q.day.Label, err)
	}
	return q, nil
}

// over reports, with its sentence, a SEAT'S push its day's count refuses: the
// counter's pushes plus the entries this record carries ahead of p, at the
// bound or past it. Nobody else's push is counted.
//
// ACCEPTED as inexact across repositories: a committer does not see a seat's
// pushes other repositories' committers decided and this node has not applied,
// so C committers deciding at once may land it past the bound by (C − 1) ×
// min(room left, its entries in one batch, MaxPushesPerRecord), never more than
// its pushes in flight less one and never cumulatively, while no two
// committers' clocks stand a day apart (clock_skew reports at an hour; the apply
// keeps two days, [countSeatDays]). An exact count would queue a seat's pushes
// in every repository behind one subject to bound a few pushes in a thousand.
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

// quotaDetail is the sentence of a seat's push refused push_quota: who, its
// pushes since its company day began (refused attempts among them), the bound,
// and the day's end in the company's zone, when the count starts again. Formed
// by the push decide alone, the one judge of the count.
func quotaDetail(seat string, pushed, limit int, day period.Window) string {
	return fmt.Sprintf("seat %s has made %s, refused attempts included, since %s began on the company's calendar, "+
		"and code.limits.pushes_per_seat_day lets a seat make %s a day: nothing was decided; the count starts "+
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
	// importSeq is how many of its import's chunks have landed: what an
	// import's chunk and its end are decided in their turn by.
	importSeq int64
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
	// mirrored is a pull mirror ([RepoRef.Mirrored]), which takes no push
	// and no change of its own.
	mirrored bool
}

// grantRef is the repository id names as a grant reads it: its name, which a
// refusal names, its owner and its visibility.
func (r pushRepo) grantRef(id string) RepoRef {
	return RepoRef{ID: id, Name: r.name, Owner: r.owner, Visibility: r.visibility}
}

// writeRefusal is the refusal its STATUS gives every write that would move a
// ref of the live repository r — a push's every command, a change's every
// write — with its sentence, or "" where it takes them: an archived repository
// is read-only, one being imported takes nothing until its import is done, and
// a pull mirror moves only by its syncs ([RepoStateRefusal], the rule an engine
// write and a refs listing's deletion verdict judge by too).
func (r pushRepo) writeRefusal() (Refusal, string) {
	return RepoStateRefusal(RepoRef{Name: r.name, Status: r.status, ManagedBy: r.managedBy, Mirrored: r.mirrored})
}

// pushRepoIn reads a repository's row for a decide.
func pushRepoIn(ctx context.Context, tx *sql.Tx, id string) (pushRepo, bool, error) {
	var r pushRepo
	var status, ownerKind, visibility, managed string
	var deleted sql.NullInt64
	err := tx.QueryRowContext(ctx, decideRepoSQL, id).Scan(&r.name, &status, &r.refs,
		&r.liveBytes, &r.livePacks, &r.version, &deleted, &r.dropEpoch, &r.expungeID, &ownerKind,
		&r.owner.Ref, &visibility, &managed, &r.defaultBranch, &r.mirrored, &r.importSeq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pushRepo{}, false, nil
	case err != nil:
		return pushRepo{}, false, fmt.Errorf("gitlet: read repository %s for its pushes: %w", id, err)
	}
	r.status, r.owner.Kind, r.visibility = RepoStatus(status), OwnerKind(ownerKind), Visibility(visibility)
	r.managedBy = ManagedBy(managed)
	if deleted.Valid {
		r.deletedAt = store.DecodeTime(deleted.Int64)
	}
	return r, true, nil
}

// deletionIn reads the deletion marker of a repository with no row: its name,
// and true where the marker exists (a purged repository, recorded for ever),
// false for an id this log never had. Read only on a refusal's path, here and
// by [Committer.purgedDetail], over the marker's primary key.
func deletionIn(ctx context.Context, tx *sql.Tx, id string) (string, bool, error) {
	var name string
	err := tx.QueryRowContext(ctx, decideDeletionSQL, id).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("gitlet: read repository %s's deletion marker: %w", id, err)
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
		return nil, fmt.Errorf("gitlet: read the pushes a batch names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, _, err := scanPush(rows)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read the pushes a batch names: %w", err)
		}
		out[row.PushID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the pushes a batch names: %w", err)
	}
	return out, nil
}

// working is the repository as the batch's earlier pushes leave it: the refs
// the batch names, and the counts the bounds are judged on.
//
// MaxRefsPerRepo bounds LIVE refs only: no row and a tombstone both read as
// ZeroOID ([working.current]), so a create of either is one more live ref and a
// deletion one fewer, in the push that makes it too ([working.decide]).
// Tombstones are bounded apart by the applier ([MaxTombstonesPerRepo]), which
// forgets the oldest or writes deletions as removals rather than refuse
// anybody; counted here, deleting stale branches would refuse every new one
// until the deletions aged out.
type working struct {
	name string
	now  time.Time
	// defaultBranch is the repository's default branch, by name: the one
	// branch no push deletes (rule 5b, [DefaultBranchRefusal]).
	defaultBranch string

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
		return nil, fmt.Errorf("gitlet: read the refs a batch names: %w", err)
	}
	w := &working{name: repo.name, now: now, defaultBranch: repo.defaultBranch, targets: targets, live: repo.refs,
		bytes: repo.liveBytes, packs: repo.livePacks, occupied: map[string]string{}, touched: map[string]bool{}}
	if err = w.readAround(ctx, tx, repoID, batch, open, seen, deleted); err != nil {
		return nil, err
	}
	if w.expiry, err = expiryRowsIn(ctx, tx, repoID, repo, batch, open); err != nil {
		return nil, fmt.Errorf("gitlet: read the refs an expiry names: %w", err)
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
		var at, source int64
		var row ExpiryRow
		if err := rows.Scan(&ref, &row.Target, &row.Seat, &at, &row.Held, &source, &row.ChangeTarget); err != nil {
			return nil, err
		}
		row.At, row.Default = store.DecodeTime(at), ref == branchPrefix+repo.defaultBranch
		if source > 0 {
			// THE SNAPSHOT'S NAME of the repository, as the change's number
			// is the snapshot's: a rename since the page names it as it is now.
			row.OpenChange = ChangeRef(repo.name, source)
		}
		out[ref] = row
	}
	return out, rows.Err()
}

// expiryRefsSQL is rule 4b's row of each ref of a list (?2) in the repository
// ?1: code_refs' key, driven from the list; the NUMBER of the live change the
// ref is the source of (0 for none — a repository numbers its changes from 1),
// one seek of code_changes_open_idx and a read of that one row, since the
// sentence keeping the branch names the change; and an EXISTS seek of the
// live changes by target (code_changes_target_idx, its leading two columns).
// The live states are spelled as both indexes are partial on them,
// unqualified (no other table here has a state). Registered in plan_test.go.
const expiryRefsSQL = `SELECT r.ref, r.target, r.seat, r.at, r.held_position,
		COALESCE((SELECT s.number FROM code_changes AS s INDEXED BY code_changes_open_idx
			WHERE s.repo_id = ?1 AND s.source_ref = r.ref AND ` + liveChanges + `), 0),
		EXISTS (SELECT 1 FROM code_changes AS t INDEXED BY code_changes_target_idx
			WHERE t.repo_id = ?1 AND t.target_ref = r.ref AND ` + liveChanges + `)
	FROM json_each(?2) AS j CROSS JOIN code_refs AS r
	WHERE r.repo_id = ?1 AND r.ref = CAST(j.value AS TEXT)`

// readAround reads what is around every ref the batch may create, for
// [working.beside]: into targets the rows of each directory a create would make
// and of its bound, into watched what the working state can hold live, and into
// occupied each create's first live ref under it the working state does not
// answer. seen (every ref the batch names) gains the directories and bounds;
// deleted is every ref a command deletes. A ref the batch MAY create is one a
// command gives a value, that no stateless rule refuses, over no live value or
// over one a command of the batch deletes; a batch of updates reads nothing
// here. The cost follows what the creates spell, never what the repository
// holds: a primary-key seek per directory (a ref of n components below its
// namespace makes n−1, at most about 120) and a ranged seek per gap ([liveIn]).
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
		return fmt.Errorf("gitlet: read the directories a batch's creates would make: %w", err)
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

	// THE GAPS: every create's range (ref + "/", ref + "0") cut at every create's
	// bounds and every name with a live row, each gap asked ONCE for its first
	// live ref (a live name is the working state's to answer, since the batch may
	// delete it). Ranges nest or are apart, so one gap serves every create whose
	// range holds it, and the gaps number at most the bounds plus the live names;
	// asked per create, a nested chain of creates re-read everything below it
	// (BenchmarkDecideABatch, fanned). ONE SORTED SWEEP over the cuts makes the
	// gaps and each create's run of them: its range opens at its lower bound and
	// closes at its upper. A lower bound, ending in '/', is no ref and no other
	// bound; an upper, ending in '0', may be a live name too, and is one cut.
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
		return fmt.Errorf("gitlet: read the refs under a batch's creates: %w", err)
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
// carries, so a list is at most about a mebibyte (refs are at most
// MaxRefNameBytes) however deep a batch's creates: a record's worth of the
// deepest refs makes some three hundred thousand directories, whose one
// statement costs several hundred megabytes of encoding where parts cost the
// same time (BenchmarkDecideABatch's deepest arm).
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
			return fmt.Errorf("gitlet: read the branches a batch's changes propose into: %w", err)
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
// the push being decided leave things (made: its creates ok so far; gone: its
// deletions ok), and whether there is one: a directory ref would make that is
// live, a live ref under ref the batch names nowhere, or a watched one under it
// live by now. A client keeps refs as files, so refs/heads/feature and
// refs/heads/feature/x can never both be live ("cannot lock ref"). git's
// receive-pack refuses the pair in its ref transaction, which here never runs
// (every command goes to the hook), and only the snapshot sees both refs, so
// the decide refuses it (ref_conflict). The push's own deletions free a name
// (git takes deleting feature beside creating feature/v2), an earlier push's
// create takes one, and a tombstone is in nobody's way.
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

// liveIn is, for each gap (lo, hi), both exclusive, the first live ref of
// repoID between them in byte order, or "" for none. Two statements, the
// second only for a gap that holds one: whether it does ([gapsHoldingSQL]),
// then which ([firstLiveInSQL]). As one statement it costs about twice as much
// a gap, and a gap holding a live ref is a refused create, which nearly none is.
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
		return nil, fmt.Errorf("gitlet: encode the gaps under a batch's creates: %w", err)
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
			return nil, fmt.Errorf("gitlet: the gaps under a batch's creates answered gap %d of %d", i, len(part))
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
		// 2b THE GRANT, before any state of the ref is read. Rare, since the receiver
		// checked the grant before the bytes moved: only where the chart moved since.
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
		// 5b THE DEFAULT BRANCH, which no push deletes whoever sends it: after the
		// protections, so a protected default branch is refused by the rule naming it.
		if r, why := DefaultBranchRefusal(*c, w.name, w.defaultBranch); r != "" {
			refuse(c, r, why)
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

// behind is the pack bound over p, a push with an ok command left: true, with
// its sentence, when p carries a pack and the repository holds MaxPacksPerRepo
// live packs or more, the batch's earlier entries' counted in. It refuses the
// push WHOLE, compaction_behind, until a compaction merges the packs. Asked
// LAST, so a push refused for a reason of its own is told that reason; and only
// of a push carrying a pack, so a deletion or a move onto a commit already held
// is decided at the bound as ever. A refused push is never applied, so its pack
// counts for no later push. The receiver refuses the same push before uploading
// its bundle, from its own synced row (internal/gitlet/host's precheck); this
// decide is the authority.
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
// refused it by: the rule [ProtectionRefusal] answered, so no rule is asked
// about the ref again. ONE SPELLING for the decide and Gitlet, which refuses a
// protected update before it uploads anything (ADR-0008).
//
// IT NAMES NO REF: git prints the ref beside the reason, and the sentence rides
// a proposal whose every Detail is bounded (maxDetailBytes), where a ref and a
// pattern of MaxRefNameBytes each could not fit beside the words and the push
// would be refused whole as the receiver's bug. So each sentence is its pattern
// and at most 64 bytes of words; why a seat's change reaches a persons-rule ref
// only through a person is Gitlet's "What git prints" row.
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

// UnheldDetail is the sentence of a branch update Gitlet refuses
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
		drop_epoch, expunge_id, owner_kind, owner, visibility, managed_by, default_branch, mirror_from != '',
		import_seq
		FROM code_repos
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
	// gapsHoldingSQL is the place of each [lo, hi] gap of its list that holds a
	// live ref, and firstLiveInSQL the first such ref of each gap: driven from the
	// list, each gap one ranged seek of the LIVE refs' index ([liveRefsKey])
	// stopping at its first row, as a subquery, so a gap answers one row where a
	// join answers every ref in it. Never the key: a gap's range holds the
	// tombstones under a create too, which a seek over the key steps over one by
	// one (planner.go). `r.target != ''` repeats the index's own predicate so the
	// planner may use it.
	gapsHoldingSQL = `SELECT CAST(j.key AS INTEGER) FROM json_each(?2) AS j
		WHERE EXISTS (SELECT 1 FROM code_refs AS r INDEXED BY ` + liveRefsKey + `
			WHERE r.repo_id = ?1 AND r.ref > CAST(j.value ->> 0 AS TEXT) AND r.ref < CAST(j.value ->> 1 AS TEXT)
				AND r.target != '')`
	firstLiveInSQL = `SELECT CAST(j.key AS INTEGER), (SELECT r.ref FROM code_refs AS r INDEXED BY ` + liveRefsKey + `
		WHERE r.repo_id = ?1 AND r.ref > CAST(j.value ->> 0 AS TEXT) AND r.ref < CAST(j.value ->> 1 AS TEXT)
			AND r.target != '' ORDER BY r.ref LIMIT 1)
		FROM json_each(?2) AS j`
)
