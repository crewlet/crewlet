package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// THE PUSH DECIDE, RULE BY RULE, over rows an applier wrote: each case is one
// rule of decide.go's order, and each is built so every EARLIER rule would not
// fire — a noop whose old value is wrong, a stale command on a protected ref,
// a protected command that is otherwise ok — so a rule out of order fails the
// case it was moved past.

// decideAt is the decide's clock in this file: the applier's own instant, so
// a pack minted now is fresh and the seeded refs are as old as the case says.
var decideAt = applyClock

// decideRepo is the repository every case decides against: main and feature
// at commits, a tag, a rule on main that allows neither a force-push nor a
// deletion, and a ref nobody pushes to.
const decideRepo = "0190f3c4-0000-7000-8000-00000000d0c1"

// decideEstate is an estate holding decideRepo as decideRows says.
func decideEstate(t *testing.T) *estate {
	t.Helper()
	e := openApplyEstate(t)
	settings := settingsFor("")
	settings.Protections = []Protection{
		{Pattern: "refs/heads/locked", DirectPush: DirectNone},
		{Pattern: "refs/heads/main", DirectPush: DirectPersons},
	}
	e.apply(rec(t, NameSubject("web"), OpCreate,
		Scope{NamePath(NameToken("web")), RepoPath(decideRepo)},
		CreateRepo{RepoID: decideRepo, Name: "web", Settings: settings}))
	e.apply(pushOf(t, decideRepo, pushEntry(uuid.Must(uuid.NewV7()).String(),
		move("refs/heads/main", ZeroOID, oid("A")),
		move("refs/heads/feature", ZeroOID, oid("F")),
		move("refs/tags/v1", ZeroOID, oid("T")))))
	return e
}

// proposalOf is one undecided push of cmds, with no pack.
func proposalOf(cmds ...Command) Proposal {
	return Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: decideRepo,
		Principal: Principal{Kind: PrincipalOperator, OperatorID: "op", Seat: "op"},
		Node:      "node-a", Commands: cmds, ReceivedAt: decideAt,
	}
}

// withPack is p carrying a fresh one-object bundle of size bytes, minted at at.
func withPack(p Proposal, size int64, at time.Time) Proposal {
	p.Pack = &PackRef{
		PackID: p.PushID, Object: objstore.KeyAt(at), Hash: objstore.HashOf([]byte(p.PushID)),
		Size: size, IndexBytes: IndexBytesFor(1), ReverseBytes: ReverseBytesFor(1),
		GitChecksum: oid("pack" + p.PushID), Objects: 1,
	}
	return p
}

// cmd is an undecided command.
func cmd(ref, old, new string) Command { return Command{Ref: ref, Old: old, New: new} }

// decideIn runs the first round of a batch's decide over e's rows.
func decideIn(t *testing.T, e *estate, limits Limits, batch ...Proposal) ([]PushEntry, []decided) {
	t.Helper()
	return decideRound(t, e, limits, &verdicts{}, batch...)
}

// decideRound runs one round of the decide over e's rows, with the protection
// verdicts the batch's earlier rounds formed.
func decideRound(t *testing.T, e *estate, limits Limits, v *verdicts, batch ...Proposal) ([]PushEntry, []decided) {
	t.Helper()
	return decideOn(t, e, decideRepo, limits, DefaultDeletedRetention, v, batch...)
}

// decideOn runs one round of the decide over e's rows for repoID, on an epoch
// whose code.deleted_retention_days is retention.
func decideOn(t *testing.T, e *estate, repoID string, limits Limits, retention time.Duration,
	v *verdicts, batch ...Proposal) ([]PushEntry, []decided) {

	t.Helper()
	var (
		entries []PushEntry
		answers []decided
	)
	if err := e.db.Read(context.Background(), func(tx *sql.Tx) error {
		var err error
		entries, answers, _, err = decideBatch(context.Background(), tx, repoID, batch, limits,
			retention, decideAt, v)
		return err
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	return entries, answers
}

// A PROTECTION VERDICT OUTLIVES ITS ROUND ONLY UNDER THE RULES IT WAS FORMED
// UNDER. The rounds of one publish ask the same commands, so a round whose
// snapshot holds the same rules forms no verdict again — asking every rule
// about every command is the one cost of a round a founder's rules can make
// seconds long — and a round whose snapshot holds other rules, after a
// settings record, forms every verdict again from them rather than answering
// from rules the repository no longer has.
func TestAProtectionVerdictOutlivesItsRoundOnlyUnderItsRules(t *testing.T) {
	t.Parallel()
	e := decideEstate(t)
	batch := []Proposal{
		proposalOf(cmd("refs/heads/locked", ZeroOID, oid("L"))),
		proposalOf(cmd("refs/heads/main", oid("A"), oid("B")), cmd("refs/heads/fresh", ZeroOID, oid("N"))),
	}
	var v verdicts
	want := func(round string, answers []decided, locked Refusal, formed int) {
		t.Helper()
		if got := answers[0].commands[0].Reason; got != locked {
			t.Fatalf("%s: the push to refs/heads/locked was decided %q, want %q", round, got, locked)
		}
		for _, c := range answers[1].commands {
			if c.Result != ResultOK {
				t.Fatalf("%s: %s was decided %s %q, want ok", round, c.Ref, c.Result, c.Reason)
			}
		}
		if v.formed != formed {
			t.Fatalf("%s: %d verdicts formed over the batch's rounds, want %d", round, v.formed, formed)
		}
	}
	_, answers := decideRound(t, e, DefaultLimits(), &v, batch...)
	want("the first round", answers, RefusalProtectedDirect, 3)
	_, answers = decideRound(t, e, DefaultLimits(), &v, batch...)
	want("a round under the same rules", answers, RefusalProtectedDirect, 3)

	// THE RULES CHANGE between rounds: the lock is lifted.
	e.apply(rec(t, RepoSubject(decideRepo), OpSettings, Scope{RepoPath(decideRepo)},
		SettingsRecord{RepoID: decideRepo, Settings: settingsFor("", "refs/heads/main")}))
	_, answers = decideRound(t, e, DefaultLimits(), &v, batch...)
	want("a round under new rules", answers, "", 6)
}

// THE RULES IN ORDER, one command at a time, each the first that fires.
func TestThePushDecide(t *testing.T) {
	t.Parallel()
	forced := cmd("refs/heads/main", oid("A"), oid("B"))
	forced.Forced = true
	prefilled := cmd("refs/heads/feature", oid("F"), oid("G"))
	prefilled.Result, prefilled.Reason, prefilled.Detail = ResultNG, RefusalBlobTooLarge, "a blob of 2 GiB"
	for name, tc := range map[string]struct {
		c      Command
		result CommandResult
		reason Refusal
		detail string // a substring of the Detail the decide forms
	}{
		// 1: a command that would be ok, refused by the receiver.
		"a pre-filled refusal stays": {prefilled, ResultNG, RefusalBlobTooLarge, "a blob of 2 GiB"},
		// 2: a bad ref whose old value is right and that no rule protects.
		"not a ref": {cmd("refs/heads/a..b", ZeroOID, oid("B")), ResultNG, RefusalBadRef, "check-ref-format"},
		"outside the namespaces": {cmd("refs/remotes/o/main", ZeroOID, oid("B")), ResultNG,
			RefusalReservedRef, "outside refs/heads/"},
		"the engine's own": {cmd("refs/crewlet/x/y", ZeroOID, oid("B")), ResultNG,
			RefusalReservedRef, "the engine's own"},
		// 2, before 3: the tag is where the client says.
		"a tag moved": {cmd("refs/tags/v1", oid("T"), oid("U")), ResultNG, RefusalTagMoved, "a tag does not move"},
		// 3, before 4: the old value is wrong and the new one is already there.
		"already there": {cmd("refs/heads/main", oid("Z"), oid("A")), ResultNoop, "", ""},
		// 4, before 5: a stale deletion of a ref no rule lets anybody delete.
		"stale": {cmd("refs/heads/main", oid("Z"), ZeroOID), ResultNG, RefusalStale,
			"refs/heads/main is at " + oid("A")[:7] + "; fetch first"},
		"stale over nothing": {cmd("refs/heads/gone", oid("Z"), oid("B")), ResultNG, RefusalStale, "is at nothing"},
		// 5, before 6 and 7: the old values are right.
		"a protected deletion": {cmd("refs/heads/main", oid("A"), ZeroOID), ResultNG,
			RefusalProtectedDelete, "protection refs/heads/main: allow_delete"},
		"a protected force-push": {forced, ResultNG, RefusalProtectedForce, "protection refs/heads/main: allow_force"},
		"no direct push": {cmd("refs/heads/locked", ZeroOID, oid("B")), ResultNG,
			RefusalProtectedDirect, "protection refs/heads/locked: direct_push none"},
		// 7.
		"an update":  {cmd("refs/heads/main", oid("A"), oid("B")), ResultOK, "", ""},
		"a create":   {cmd("refs/heads/new", ZeroOID, oid("B")), ResultOK, "", ""},
		"a deletion": {cmd("refs/heads/feature", oid("F"), ZeroOID), ResultOK, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			entries, answers := decideIn(t, e, DefaultLimits(), proposalOf(tc.c))
			got := answers[0].commands[0]
			if got.Result != tc.result || got.Reason != tc.reason {
				t.Fatalf("decided %s %s, want %s %s", got.Result, got.Reason, tc.result, tc.reason)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("the detail is %q, want it to say %q", got.Detail, tc.detail)
			}
			if got.Result == ResultNG && got.Detail == "" {
				t.Error("an ng the decide formed carries no detail")
			}
			if inRecord := tc.result == ResultOK; answers[0].inRecord != inRecord || (len(entries) == 1) != inRecord {
				t.Errorf("in the record: %v with %d entries, want %v", answers[0].inRecord, len(entries), inRecord)
			}
			for _, e := range entries {
				for _, c := range e.Commands {
					if c.Detail != "" {
						t.Errorf("the record's command on %s carries the sentence %q", c.Ref, c.Detail)
					}
				}
			}
		})
	}
}

// A BATCH IS DECIDED IN ORDER, each push against what the ones before it did:
// the refs a push moves, the creates it counts against the bound and the
// bytes its pack adds.
func TestABatchSeesItsEarlierPushes(t *testing.T) {
	t.Parallel()
	t.Run("refs", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		entries, answers := decideIn(t, e, DefaultLimits(),
			proposalOf(cmd("refs/heads/main", oid("A"), oid("B"))),
			proposalOf(cmd("refs/heads/main", oid("B"), oid("C"))),
			proposalOf(cmd("refs/heads/main", oid("A"), oid("D"))))
		if len(entries) != 2 {
			t.Fatalf("%d entries, want the two pushes that moved main", len(entries))
		}
		third := answers[2].commands[0]
		if third.Reason != RefusalStale || !strings.Contains(third.Detail, oid("C")[:7]) {
			t.Fatalf("the third push is %s %s (%q), want stale at the second's commit", third.Result,
				third.Reason, third.Detail)
		}
	})
	t.Run("the live refs' bound", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, MaxRefsPerRepo-1, decideRepo)
		_, answers := decideIn(t, e, DefaultLimits(),
			proposalOf(cmd("refs/heads/one", ZeroOID, oid("1"))),
			proposalOf(cmd("refs/heads/two", ZeroOID, oid("2")), cmd("refs/heads/main", oid("A"), oid("B"))))
		if got := answers[0].commands[0]; got.Result != ResultOK {
			t.Fatalf("the last free ref was refused: %s %s", got.Result, got.Reason)
		}
		if got := answers[1].commands[0]; got.Reason != RefusalRepoRefs || got.Detail == "" {
			t.Fatalf("a create past the bound is %s %s %q, want repo_refs", got.Result, got.Reason, got.Detail)
		}
		if got := answers[1].commands[1]; got.Result != ResultOK {
			t.Fatalf("an update beside a refused create is %s %s, want ok", got.Result, got.Reason)
		}
	})
	t.Run("one push's own creates", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, MaxRefsPerRepo-1, decideRepo)
		_, answers := decideIn(t, e, DefaultLimits(), proposalOf(
			cmd("refs/heads/one", ZeroOID, oid("1")), cmd("refs/heads/two", ZeroOID, oid("2"))))
		if a, b := answers[0].commands[0], answers[0].commands[1]; a.Result != ResultOK || b.Reason != RefusalRepoRefs {
			t.Fatalf("two creates at the last free ref are %s and %s %s, want ok and repo_refs",
				a.Result, b.Result, b.Reason)
		}
	})
	t.Run("the repository's bytes", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		size := 32 + IndexBytesFor(1) + ReverseBytesFor(1)
		var live int64
		if err := e.db.Read(context.Background(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(context.Background(), `SELECT live_bytes FROM code_repos WHERE id = ?`, decideRepo).Scan(&live)
		}); err != nil {
			t.Fatal(err)
		}
		limits := DefaultLimits()
		limits.RepoMaxBytes = live + size // room for exactly one more pack
		_, answers := decideIn(t, e, limits,
			withPack(proposalOf(cmd("refs/heads/one", ZeroOID, oid("1"))), size, decideAt),
			withPack(proposalOf(cmd("refs/heads/two", ZeroOID, oid("2"))), size, decideAt))
		if got := answers[0].commands[0]; got.Result != ResultOK {
			t.Fatalf("the pack that fits was refused: %s %s", got.Result, got.Reason)
		}
		if got := answers[1].commands[0]; got.Reason != RefusalRepoSize || !strings.Contains(got.Detail, "repo_max_bytes") {
			t.Fatalf("the pack past the limit is %s %s %q, want repo_size", got.Result, got.Reason, got.Detail)
		}
	})
}

// THE TABLE'S BOUND counts the rows a repository's refs hold — live and
// deleted — for a create of a ref with no row, and counts as gone the expired
// tombstones the record's own sweep takes: a repository at the bound whose
// rows are expired tombstones takes a create, and one whose tombstones are
// fresh does not, while a create over a ref's own tombstone grows nothing.
func TestTheRefsTableIsBoundedWithItsSweep(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		age    time.Duration
		ref    string
		result CommandResult
	}{
		"fresh tombstones":                {age: time.Hour, ref: "refs/heads/new", result: ResultNG},
		"expired tombstones":              {age: RefTombstoneHorizon + time.Hour, ref: "refs/heads/new", result: ResultOK},
		"a create over its own tombstone": {age: time.Hour, ref: "refs/heads/dead-0", result: ResultOK},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			for i := range 10 {
				e.exec(`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES (?, ?, '', 1, ?)`,
					decideRepo, "refs/heads/dead-"+string(rune('0'+i)), store.EncodeTime(decideAt.Add(-tc.age)))
			}
			e.exec(`UPDATE code_repos SET refs = ?, tombstones = 10 WHERE id = ?`, MaxRefsPerRepo-10, decideRepo)
			_, answers := decideIn(t, e, DefaultLimits(), proposalOf(cmd(tc.ref, ZeroOID, oid("N"))))
			got := answers[0].commands[0]
			if got.Result != tc.result {
				t.Fatalf("decided %s %s (%q), want %s", got.Result, got.Reason, got.Detail, tc.result)
			}
			if got.Result == ResultNG && got.Reason != RefusalRepoRefs {
				t.Fatalf("refused %s, want repo_refs", got.Reason)
			}
		})
	}
}

// seedTombstoneRows writes n tombstones of decideRepo named prefix-<i>, deleted
// age before the decide, and counts them in the repository's row.
func (e *estate) seedTombstoneRows(prefix string, n int, age time.Duration) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/%s-%05d', ?, i), '', 1, ? FROM k`,
		n, decideRepo, prefix, store.EncodeTime(decideAt.Add(-age)))
	e.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, n, decideRepo)
}

// THE TOMBSTONES A BATCH BRINGS BACK ARE NOT THE SWEEP'S: a batch that
// recreates every expired tombstone of a repository at the table's bound
// leaves its sweep nothing to take, so a create of a ref with no row beside
// them is refused — while the same create alone, with the expired tombstones
// still the sweep's, is taken.
func TestARecreatedTombstoneIsNotFreeForTheSweep(t *testing.T) {
	t.Parallel()
	recreate := func() []Command {
		var cmds []Command
		for i := range 5 {
			cmds = append(cmds, cmd(fmt.Sprintf("refs/heads/dead-%05d", i), ZeroOID, oid(fmt.Sprint("back", i))))
		}
		return cmds
	}
	for name, tc := range map[string]struct {
		cmds []Command
		want CommandResult
	}{
		"beside the tombstones it brings back": {append(recreate(), cmd("refs/heads/new", ZeroOID, oid("N"))), ResultNG},
		"alone":                                {[]Command{cmd("refs/heads/new", ZeroOID, oid("N"))}, ResultOK},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			e.seedTombstoneRows("dead", 5, RefTombstoneHorizon+time.Hour)
			e.seedTombstoneRows("fresh", 5, time.Hour)
			e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, MaxRefsPerRepo-10, decideRepo)
			_, answers := decideIn(t, e, DefaultLimits(), proposalOf(tc.cmds...))
			cmds := answers[0].commands
			for _, c := range cmds[:len(cmds)-1] {
				if c.Result != ResultOK {
					t.Fatalf("a create over its own tombstone is %s %s (%q), want ok", c.Result,
						c.Reason, c.Detail)
				}
			}
			got := cmds[len(cmds)-1]
			if got.Result != tc.want || (tc.want == ResultNG &&
				(got.Reason != RefusalRepoRefs || !strings.Contains(got.Detail, "deleted refs"))) {
				t.Fatalf("the create of a ref with no row is %s %s (%q), want %s", got.Result,
					got.Reason, got.Detail, tc.want)
			}
		})
	}
}

// THE SWEEP TAKES ONLY WHAT THE RECORD'S ROWS LEAVE IT: of thousands of
// expired tombstones at the table's bound, a batch of three maximal pushes
// counts as free only the rows MaxRecordRows leaves past the most its entries
// can write — so exactly that many of its creates of refs with no row are
// taken, and the rest are refused rather than grown into a table no record
// sweeps back under the bound.
func TestTheSweepFreesOnlyWhatTheRecordLeavesIt(t *testing.T) {
	t.Parallel()
	const expired = 5_000
	e := decideEstate(t)
	e.seedTombstoneRows("dead", expired, RefTombstoneHorizon+time.Hour)
	e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, MaxRefsPerRepo-expired, decideRepo)
	var batch []Proposal
	rows := 0
	for p := range 3 {
		var cmds []Command
		for c := range MaxCommandsPerPush {
			cmds = append(cmds, cmd(fmt.Sprintf("refs/heads/new/%d/%04d", p, c), ZeroOID, oid(fmt.Sprint(p, c))))
		}
		batch = append(batch, proposalOf(cmds...))
		rows += rowBound(batch[p])
	}
	floor := MaxRecordRows - recordRows(rows)
	if floor <= 0 || floor >= expired {
		t.Fatalf("the sweep's floor is %d — the case needs one above none and below the %d "+
			"expired tombstones", floor, expired)
	}
	_, answers := decideIn(t, e, DefaultLimits(), batch...)
	taken := 0
	for _, a := range answers {
		for _, c := range a.commands {
			switch {
			case c.Result == ResultOK:
				taken++
			case c.Reason != RefusalRepoRefs || !strings.Contains(c.Detail, "deleted refs"):
				t.Fatalf("%s is %s %s (%q), want ok or the table's repo_refs", c.Ref, c.Result,
					c.Reason, c.Detail)
			}
		}
	}
	if taken != floor {
		t.Fatalf("%d creates taken, want the %d rows the record leaves its sweep", taken, floor)
	}
}

// A PACK IS JUDGED OVER THE PUSH'S OK COMMANDS: past the write bound it is
// upload_expired, and an --atomic push with any refusal is refused whole,
// leaving a noop a noop.
func TestAPushIsJudgedWhole(t *testing.T) {
	t.Parallel()
	t.Run("an expired upload", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		p := withPack(proposalOf(cmd("refs/heads/main", oid("A"), oid("B")),
			cmd("refs/heads/feature", oid("Z"), oid("F"))), 1<<20, decideAt.Add(-objstore.RecordWithin))
		entries, answers := decideIn(t, e, DefaultLimits(), p)
		if got := answers[0].commands[0]; got.Reason != RefusalUploadExpired || got.Detail == "" {
			t.Fatalf("an update with an expired pack is %s %s, want upload_expired", got.Result, got.Reason)
		}
		if got := answers[0].commands[1]; got.Result != ResultNoop {
			t.Fatalf("a noop beside it is %s %s, want noop", got.Result, got.Reason)
		}
		if len(entries) != 0 {
			t.Fatal("a push with nothing ok is in the record")
		}
	})
	t.Run("atomic", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		p := proposalOf(cmd("refs/heads/main", oid("A"), oid("B")),
			cmd("refs/heads/feature", oid("Z"), oid("G")),
			cmd("refs/tags/v1", ZeroOID, oid("T")))
		p.Atomic = true
		entries, answers := decideIn(t, e, DefaultLimits(), p)
		want := []Refusal{RefusalAtomic, RefusalStale, ""}
		for i, c := range answers[0].commands {
			if c.Reason != want[i] {
				t.Errorf("command %d is %s %s, want %q", i, c.Result, c.Reason, want[i])
			}
		}
		if answers[0].commands[2].Result != ResultNoop {
			t.Errorf("the noop became %s", answers[0].commands[2].Result)
		}
		if len(entries) != 0 {
			t.Fatal("an atomic push with a refusal is in the record")
		}
	})
}

// A PUSH THAT LANDED IS ANSWERED FROM ITS ROW, whatever became of the
// repository since; one named twice in a batch is decided once; and one whose
// id names another repository's push is no push this decide can answer.
func TestAPushIdIsDecidedOnce(t *testing.T) {
	t.Parallel()
	e := decideEstate(t)
	landed := proposalOf(cmd("refs/heads/feature", oid("F"), oid("G")))
	e.apply(pushOf(t, decideRepo, PushEntry{
		PushID: landed.PushID, Principal: landed.Principal, Node: landed.Node,
		Commands: []Command{move("refs/heads/feature", oid("F"), oid("G"))}, ReceivedAt: decideAt,
	}))
	other := proposalOf(cmd("refs/heads/x", ZeroOID, oid("X")))
	const elsewhere = "0190f3c4-0000-7000-8000-00000000d0c2"
	e.apply(create(t, elsewhere, "elsewhere"))
	e.apply(pushOf(t, elsewhere, PushEntry{
		PushID: other.PushID, Principal: other.Principal, Node: other.Node,
		Commands: []Command{move("refs/heads/x", ZeroOID, oid("X"))}, ReceivedAt: decideAt,
	}))
	e.exec(`UPDATE code_repos SET status = 'archived' WHERE id = ?`, decideRepo)
	twice := proposalOf(cmd("refs/heads/main", oid("A"), oid("B")))
	_, answers := decideIn(t, e, DefaultLimits(), landed, other, twice, twice)
	if answers[0].collapsed == nil || answers[0].commands[0].Result != ResultOK {
		t.Fatalf("a landed push on an archived repository is %+v, want its row", answers[0])
	}
	if !errors.Is(answers[1].err, ErrInvalidProposal) {
		t.Fatalf("a push id of another repository answered %v, want ErrInvalidProposal", answers[1].err)
	}
	if answers[2].refused != RefusalRepoArchived || answers[2].commands[0].Reason != RefusalRepoArchived {
		t.Fatalf("a new push to an archived repository is %+v, want repo_archived", answers[2])
	}
	if answers[3].dupOf != 2 {
		t.Fatalf("the repeat of a push id in the batch is %+v, want the first's answer", answers[3])
	}
}

// A PUSH TO A REPOSITORY IT CANNOT REACH IS TOLD WHAT BECAME OF IT. Four
// states answer repo_deleted, and each has its own remedy: a deleted
// repository comes back as it was when declared again — until its purge,
// whose earliest instant the sentence names, on the deciding epoch's
// retention — while one being purged, or purged, never comes back and its
// name declared again is a new, empty repository, and an id no repository
// had is none at all. One sentence for the four told a client whose
// repository was gone for good to declare it again to restore it.
//
// Mutation: answer every state with the deleted sentence and the purging,
// purged and unknown cases fail on "restores"; drop the marker read and the
// purged case fails as unknown; decide with the default retention and the
// deleted case names the wrong instant.
func TestARefusedRepositorySaysWhatBecameOfIt(t *testing.T) {
	t.Parallel()
	const retention = 30 * 24 * time.Hour
	deleted := func(t *testing.T, e *estate) {
		e.apply(rec(t, RepoSubject(decideRepo), OpDelete, Scope{RepoPath(decideRepo)},
			LifecycleRecord{RepoID: decideRepo, ConfigEpoch: 1, Reason: "removed from code.repos"}))
	}
	const never = "0190f3c4-0000-7000-8000-00000000dead"
	for name, tc := range map[string]struct {
		settle func(t *testing.T, e *estate)
		repo   string
		want   []string
		never  []string
	}{
		"deleted": {
			settle: deleted, repo: decideRepo,
			want: []string{"repository web is deleted", "declaring it again in code.repos restores it",
				"may begin from " + applyClock.Add(retention).Format(time.RFC3339)},
			never: []string{"new, empty", "being purged", "does not exist"},
		},
		"being purged": {
			settle: func(t *testing.T, e *estate) {
				deleted(t, e)
				e.apply(partOf(t, decideRepo, 1))
			},
			repo: decideRepo,
			want: []string{"repository web is being purged", "cannot be undone",
				"creates a new, empty repository"},
			never: []string{"restores", "does not exist"},
		},
		"purged": {
			settle: func(t *testing.T, e *estate) {
				deleted(t, e)
				e.apply(rec(t, RepoSubject(decideRepo), OpPurge,
					Scope{NamePath(NameToken("web")), RepoPath(decideRepo)},
					Purge{RepoID: decideRepo, Name: "web", Reason: "past retention"}))
			},
			repo:  decideRepo,
			want:  []string{"repository web was purged", "creates a new, empty repository"},
			never: []string{"restores", "being purged", "does not exist"},
		},
		"never created": {
			repo:  never,
			want:  []string{"repository " + never + " does not exist"},
			never: []string{"restores", "purged"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			if tc.settle != nil {
				tc.settle(t, e)
			}
			p := proposalOf(cmd("refs/heads/main", oid("A"), oid("B")),
				cmd("refs/heads/fresh", ZeroOID, oid("N")))
			p.RepoID = tc.repo
			entries, answers := decideOn(t, e, tc.repo, DefaultLimits(), retention, &verdicts{}, p)
			if len(entries) != 0 {
				t.Fatalf("the refused push is %d entries of a record, want none", len(entries))
			}
			a := answers[0]
			if a.refused != RefusalRepoDeleted {
				t.Fatalf("the push is refused %q, want %q", a.refused, RefusalRepoDeleted)
			}
			for _, want := range tc.want {
				if !strings.Contains(a.detail, want) {
					t.Errorf("the refusal says %q, which does not say %q", a.detail, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(a.detail, never) {
					t.Errorf("the refusal says %q, which says %q — false of this state", a.detail, never)
				}
			}
			for _, c := range a.commands {
				if c.Result != ResultNG || c.Reason != RefusalRepoDeleted || c.Detail != a.detail {
					t.Errorf("%s is %s %q (%q), want ng repo_deleted with the push's sentence",
						c.Ref, c.Result, c.Reason, c.Detail)
				}
			}
		})
	}
}
