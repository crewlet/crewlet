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
		Via:       ViaGit, Node: "node-a", Commands: cmds, ReceivedAt: decideAt,
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
// whose code.deleted_retention_days is retention and which holds no chart —
// the cases of persons' pushes, whose decide reads none.
func decideOn(t *testing.T, e *estate, repoID string, limits Limits, retention time.Duration,
	v *verdicts, batch ...Proposal) ([]PushEntry, []decided) {

	t.Helper()
	entries, answers, err := decideErr(e, repoID, limits, retention, SeatRules{}, decideAt, v, batch...)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return entries, answers
}

// decideErr is one round of the decide over e's rows for repoID, on an epoch
// whose code.deleted_retention_days is retention and whose chart and
// calendar are seats, at the instant now — answering its error.
func decideErr(e *estate, repoID string, limits Limits, retention time.Duration, seats SeatRules,
	now time.Time, v *verdicts, batch ...Proposal) ([]PushEntry, []decided, error) {

	var (
		entries []PushEntry
		answers []decided
	)
	err := e.db.Read(context.Background(), func(tx *sql.Tx) error {
		var err error
		entries, answers, _, _, err = decideBatch(context.Background(), tx, repoID, batch, limits,
			retention, seats, now, v)
		return err
	})
	return entries, answers, err
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
		// The seats' root and one seat's namespace as a branch are
		// nobody's, a person's push included: under ref_conflict either
		// would block seats' namespaces.
		"the seats' root": {cmd("refs/heads/seats", ZeroOID, oid("B")), ResultNG,
			RefusalReservedRef, "seats' own namespaces"},
		"a namespace as a branch": {cmd("refs/heads/seats/ana", ZeroOID, oid("B")), ResultNG,
			RefusalReservedRef, "seats' own namespaces"},
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

// DELETED REFS NEVER REFUSE A CREATE: MaxRefsPerRepo bounds a repository's
// LIVE refs, and its tombstones — the deletions a delta still has to carry —
// are bounded apart, by the applier ([MaxTombstonesPerRepo]), which forgets
// the oldest rather than refuses anybody. Judged against the whole table, a
// repository of 40 000 live refs that deleted 60 000 stale branches refused
// every new branch for two days: a limit stated in live refs, not met.
//
// Mutation: count the tombstones beside the live refs against MaxRefsPerRepo
// and the first two rows are refused repo_refs.
func TestDeletedRefsNeverRefuseACreate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		live, tombstones int
		ref              string
		result           CommandResult
	}{
		"ten fresh tombstones": {live: MaxRefsPerRepo - 10, tombstones: 10,
			ref: "refs/heads/new", result: ResultOK},
		"tombstones past their own bound": {live: MaxRefsPerRepo - 10, tombstones: MaxTombstonesPerRepo + 10,
			ref: "refs/heads/new", result: ResultOK},
		"a create over its own tombstone": {live: MaxRefsPerRepo - 10, tombstones: 10,
			ref: "refs/heads/dead-00000", result: ResultOK},
		"the live refs at their bound": {live: MaxRefsPerRepo,
			ref: "refs/heads/new", result: ResultNG},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			if tc.tombstones > 0 {
				e.seedTombstoneRows("dead", tc.tombstones, time.Hour)
			}
			e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, tc.live, decideRepo)
			_, answers := decideIn(t, e, DefaultLimits(), proposalOf(cmd(tc.ref, ZeroOID, oid("N"))))
			got := answers[0].commands[0]
			if got.Result != tc.result {
				t.Fatalf("decided %s %s (%q), want %s", got.Result, got.Reason, got.Detail, tc.result)
			}
			if got.Result != ResultNG {
				return
			}
			// THE DETAIL NAMES THE LIVE COUNT, the one a person can act on.
			if want := fmt.Sprintf("web holds %d live refs", tc.live); got.Reason != RefusalRepoRefs ||
				!strings.Contains(got.Detail, want) {
				t.Fatalf("refused %s (%q), want repo_refs saying %q", got.Reason, got.Detail, want)
			}
		})
	}
}

// A PUSH'S OWN DELETIONS MAKE ROOM FOR ITS CREATES, in whichever order git
// sends them: MaxRefsPerRepo bounds the live refs a repository holds, and a
// push that deletes a stale branch and adds a new one at the bound ends
// holding no more than it began with — `git push origin :old new`, or a
// --prune or --mirror push. Only a deletion the decide takes counts: one
// refused stale or by a protection frees nothing, and a deletion frees room
// for ONE create, the first. Safe because every refusal after the
// per-command rules — upload_expired, repo_size, --atomic — refuses every ok
// command of the push together, so no deletion is refused while a create it
// made room for stands.
//
// Mutation: judge a create against the live refs and the push's creates
// alone, and the first two rows refuse the create repo_refs.
func TestAPushsOwnDeletionsMakeRoomForItsCreates(t *testing.T) {
	t.Parallel()
	drop := cmd("refs/heads/feature", oid("F"), ZeroOID)
	for name, tc := range map[string]struct {
		cmds   []Command
		atomic bool
		want   []Refusal
		result []CommandResult
	}{
		"a deletion before the create": {cmds: []Command{drop, cmd("refs/heads/new", ZeroOID, oid("N"))},
			result: []CommandResult{ResultOK, ResultOK}},
		"a deletion after the create": {cmds: []Command{cmd("refs/heads/new", ZeroOID, oid("N")), drop},
			result: []CommandResult{ResultOK, ResultOK}},
		"atomic": {cmds: []Command{cmd("refs/heads/new", ZeroOID, oid("N")), drop}, atomic: true,
			result: []CommandResult{ResultOK, ResultOK}},
		"one deletion, two creates": {cmds: []Command{cmd("refs/heads/a", ZeroOID, oid("N")), drop,
			cmd("refs/heads/b", ZeroOID, oid("M"))},
			result: []CommandResult{ResultOK, ResultOK, ResultNG},
			want:   []Refusal{"", "", RefusalRepoRefs}},
		"a stale deletion frees nothing": {cmds: []Command{cmd("refs/heads/feature", oid("Z"), ZeroOID),
			cmd("refs/heads/new", ZeroOID, oid("N"))},
			result: []CommandResult{ResultNG, ResultNG}, want: []Refusal{RefusalStale, RefusalRepoRefs}},
		"a protected deletion frees nothing": {cmds: []Command{cmd("refs/heads/main", oid("A"), ZeroOID),
			cmd("refs/heads/new", ZeroOID, oid("N"))},
			result: []CommandResult{ResultNG, ResultNG}, want: []Refusal{RefusalProtectedDelete, RefusalRepoRefs}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, MaxRefsPerRepo, decideRepo)
			p := proposalOf(tc.cmds...)
			p.Atomic = tc.atomic
			_, answers := decideIn(t, e, DefaultLimits(), p)
			for i, got := range answers[0].commands {
				var want Refusal
				if tc.want != nil {
					want = tc.want[i]
				}
				if got.Result != tc.result[i] || got.Reason != want {
					t.Errorf("%s decided %s %s (%q), want %s %s", got.Ref, got.Result, got.Reason,
						got.Detail, tc.result[i], want)
				}
			}
		})
	}
}

// A REF IS NEVER BESIDE A DIRECTORY OF ITS NAME. Every ref is a file on every
// client's disk (refs/remotes/origin/<branch>), so refs/heads/feature and
// refs/heads/feature/x cannot both be live: a clone that holds one cannot
// fetch the other ("cannot lock ref"), and the receive-pack that would have
// refused the pair runs no ref transaction here. So a create beside a live
// ref above it or below it is refused ref_conflict, naming the ref in the
// way — judged against what the batch has done by then, in order, as two
// records would be: the push's own deletions free the name as git's own
// transaction lets them, an earlier push's create takes it, and a deleted
// ref (a tombstone) is in nobody's way.
//
// Mutations: the ancestor walk skipped → the "below a branch" rows ok; the
// descendant probe skipped → the "above a branch" rows ok; the probe reading
// tombstones as live → the tombstone rows refused; the push's own deletions
// ignored → the rename rows refused; the batch's earlier pushes ignored → the
// batch rows decided as the snapshot alone says; a nested create's bound not
// read → "a ref named like a nested create's bound" ok; the ranges holding a
// gap not counted (a range's close ending every range) → "a ref past a nested
// create's range" ok; a live name not cutting the gaps → the rows that delete
// what is under a create ("a directory renamed to a branch", "an earlier
// push's directory deleted") refused, and "half a directory deleted" naming
// the deleted topic/a, the snapshot answering for refs the batch deletes.
func TestARefIsNeverBesideADirectoryOfItsName(t *testing.T) {
	t.Parallel()
	// The repository decideEstate holds, and more: topic/a and topic/b,
	// deep/x/y/z, ring/y0, nest/b, the tag rc, and the tombstones gone/a and
	// old.
	seed := func(t *testing.T) *estate {
		e := decideEstate(t)
		e.apply(pushOf(t, decideRepo, pushEntry(uuid.Must(uuid.NewV7()).String(),
			move("refs/heads/topic/a", ZeroOID, oid("TA")), move("refs/heads/topic/b", ZeroOID, oid("TB")),
			move("refs/heads/deep/x/y/z", ZeroOID, oid("D")), move("refs/tags/rc", ZeroOID, oid("R")),
			move("refs/heads/ring/y0", ZeroOID, oid("Y")), move("refs/heads/nest/b", ZeroOID, oid("B")),
			move("refs/heads/gone/a", ZeroOID, oid("G")), move("refs/heads/old", ZeroOID, oid("O")))))
		e.apply(pushOf(t, decideRepo, pushEntry(uuid.Must(uuid.NewV7()).String(),
			move("refs/heads/gone/a", oid("G"), ZeroOID), move("refs/heads/old", oid("O"), ZeroOID))))
		return e
	}
	type want struct {
		reason Refusal
		names  string // the ref the detail names in the way
	}
	ok := want{}
	conflict := func(ref string) want { return want{RefusalRefConflict, ref} }
	for name, tc := range map[string]struct {
		batch [][]Command
		want  [][]want
	}{
		"below a branch": {batch: [][]Command{{cmd("refs/heads/feature/x", ZeroOID, oid("N"))}},
			want: [][]want{{conflict("refs/heads/feature")}}},
		"far below a branch": {batch: [][]Command{{cmd("refs/heads/main/a/b/c", ZeroOID, oid("N"))}},
			want: [][]want{{conflict("refs/heads/main")}}},
		"above a branch": {batch: [][]Command{{cmd("refs/heads/topic", ZeroOID, oid("N"))}},
			want: [][]want{{conflict("refs/heads/topic/a")}}},
		"far above a branch": {batch: [][]Command{{cmd("refs/heads/deep", ZeroOID, oid("N")),
			cmd("refs/heads/deep/x", ZeroOID, oid("M"))}},
			want: [][]want{{conflict("refs/heads/deep/x/y/z"), conflict("refs/heads/deep/x/y/z")}}},
		"below a tag": {batch: [][]Command{{cmd("refs/tags/rc/1", ZeroOID, oid("N"))}},
			want: [][]want{{conflict("refs/tags/rc")}}},
		// Nested creates share the snapshot's answers: ring/y0 is the bound
		// of ring/y's range and under ring's, and nest/b lies in nest's range
		// past where nest/a's ends.
		"a ref named like a nested create's bound": {batch: [][]Command{{
			cmd("refs/heads/ring", ZeroOID, oid("N")), cmd("refs/heads/ring/y", ZeroOID, oid("M"))}},
			want: [][]want{{conflict("refs/heads/ring/y0"), ok}}},
		"a ref past a nested create's range": {batch: [][]Command{{
			cmd("refs/heads/nest", ZeroOID, oid("N")), cmd("refs/heads/nest/a", ZeroOID, oid("M"))}},
			want: [][]want{{conflict("refs/heads/nest/b"), ok}}},
		// A branch and a tag are files in different directories.
		"a branch named like a tag": {batch: [][]Command{{cmd("refs/heads/rc/1", ZeroOID, oid("N")),
			cmd("refs/heads/v1", ZeroOID, oid("M"))}}, want: [][]want{{ok, ok}}},
		// Beside a name is not under it: '-' and '.' sort between feature
		// and feature/, so an order-based shortcut would meet them.
		"siblings of a name": {batch: [][]Command{{cmd("refs/heads/feature-x", ZeroOID, oid("N")),
			cmd("refs/heads/feature.x", ZeroOID, oid("M")), cmd("refs/heads/feat", ZeroOID, oid("K")),
			cmd("refs/heads/topi", ZeroOID, oid("L"))}}, want: [][]want{{ok, ok, ok, ok}}},
		"over a deleted directory": {batch: [][]Command{{cmd("refs/heads/gone", ZeroOID, oid("N"))}},
			want: [][]want{{ok}}},
		"below a deleted branch": {batch: [][]Command{{cmd("refs/heads/old/x", ZeroOID, oid("N"))}},
			want: [][]want{{ok}}},
		// One push: its deletions free the name, as git's transaction lets
		// them, whichever order git sent the commands in.
		"a branch renamed to a directory": {batch: [][]Command{{cmd("refs/heads/feature/v2", ZeroOID, oid("N")),
			cmd("refs/heads/feature", oid("F"), ZeroOID)}}, want: [][]want{{ok, ok}}},
		"a directory renamed to a branch": {batch: [][]Command{{cmd("refs/heads/topic/a", oid("TA"), ZeroOID),
			cmd("refs/heads/topic/b", oid("TB"), ZeroOID), cmd("refs/heads/topic", ZeroOID, oid("N"))}},
			want: [][]want{{ok, ok, ok}}},
		// ...and only those that land: topic/b is still there.
		"half a directory deleted": {batch: [][]Command{{cmd("refs/heads/topic/a", oid("TA"), ZeroOID),
			cmd("refs/heads/topic", ZeroOID, oid("N"))}},
			want: [][]want{{ok, conflict("refs/heads/topic/b")}}},
		"a stale deletion frees nothing": {batch: [][]Command{{cmd("refs/heads/feature", oid("Z"), ZeroOID),
			cmd("refs/heads/feature/x", ZeroOID, oid("N"))}},
			want: [][]want{{{RefusalStale, ""}, conflict("refs/heads/feature")}}},
		// One push's creates, in the order git sent them.
		"two creates, the branch first": {batch: [][]Command{{cmd("refs/heads/d", ZeroOID, oid("N")),
			cmd("refs/heads/d/e", ZeroOID, oid("M"))}}, want: [][]want{{ok, conflict("refs/heads/d")}}},
		"two creates, the directory first": {batch: [][]Command{{cmd("refs/heads/q/r", ZeroOID, oid("N")),
			cmd("refs/heads/q", ZeroOID, oid("M"))}}, want: [][]want{{ok, conflict("refs/heads/q/r")}}},
		// A create refused for another reason takes no name.
		"a refused create takes no name": {batch: [][]Command{{cmd("refs/heads/locked", ZeroOID, oid("N")),
			cmd("refs/heads/locked/x", ZeroOID, oid("M"))}},
			want: [][]want{{{RefusalProtectedDirect, ""}, ok}}},
		// A batch, push by push, against what the ones before it did.
		"an earlier push's deletion frees the name": {batch: [][]Command{
			{cmd("refs/heads/feature", oid("F"), ZeroOID)}, {cmd("refs/heads/feature/x", ZeroOID, oid("N"))}},
			want: [][]want{{ok}, {ok}}},
		"an earlier push's create takes the name": {batch: [][]Command{
			{cmd("refs/heads/n", ZeroOID, oid("N"))}, {cmd("refs/heads/n/m", ZeroOID, oid("M"))},
			{cmd("refs/heads/p/q", ZeroOID, oid("P"))}, {cmd("refs/heads/p", ZeroOID, oid("Q"))}},
			want: [][]want{{ok}, {conflict("refs/heads/n")}, {ok}, {conflict("refs/heads/p/q")}}},
		"an earlier push's directory deleted": {batch: [][]Command{
			{cmd("refs/heads/topic/a", oid("TA"), ZeroOID), cmd("refs/heads/topic/b", oid("TB"), ZeroOID)},
			{cmd("refs/heads/topic", ZeroOID, oid("N"))}},
			want: [][]want{{ok, ok}, {ok}}},
		// An update or a deletion is never a create, whatever is around it.
		"updates and deletions": {batch: [][]Command{{cmd("refs/heads/main", oid("A"), oid("B")),
			cmd("refs/heads/topic/a", oid("TA"), oid("TC"))}}, want: [][]want{{ok, ok}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := seed(t)
			batch := make([]Proposal, len(tc.batch))
			for i, cmds := range tc.batch {
				batch[i] = proposalOf(cmds...)
			}
			_, answers := decideIn(t, e, DefaultLimits(), batch...)
			for p, a := range answers {
				for i, got := range a.commands {
					w := tc.want[p][i]
					if got.Reason != w.reason || (w.reason == "") != (got.Result != ResultNG) {
						t.Errorf("push %d, %s: decided %s %s (%q), want %q", p, got.Ref, got.Result,
							got.Reason, got.Detail, w.reason)
						continue
					}
					if w.names != "" && !strings.Contains(got.Detail, w.names+" is in the way") {
						t.Errorf("push %d, %s: the detail %q does not name %s in its way", p, got.Ref,
							got.Detail, w.names)
					}
				}
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

// A PUSH PAST THE PACK BOUND IS REFUSED compaction_behind, WHOLE AND
// UNDECIDED: a repository whose compaction has not caught up with its pushes
// takes no more packs. At MaxPacksPerRepo − 1 live packs a batch's first push
// carrying a pack is the last one the repository takes, and every later push
// carrying one is refused whole — every command the receiver did not refuse,
// the ones the decide found ok among them, under the one sentence that names
// the packs the repository would hold — and is no entry of the record; a push
// carrying no pack adds none, and is decided as ever. What the bound counts is
// the repository's packs and its batch's ENTRIES' — a push carrying a pack
// that is no entry, refused for a reason of its own, adds nothing, and nor
// does one the bound refused. A repository already past the bound, as a
// later build's committer may have left it, is refused the same, its sentence
// naming what it holds.
//
// Mutation: drop the bound and the third and fourth pushes are entries; count
// the pack of a push that is no entry (the stale one) and the second is
// refused too; count the pack of a push the bound refused and the fourth's
// sentence names a pack more than the repository would hold.
func TestAPushPastThePackBoundIsRefusedCompactionBehind(t *testing.T) {
	t.Parallel()
	size := 32 + IndexBytesFor(1) + ReverseBytesFor(1)
	// behind holds answer a to a refusal of the whole push under the
	// sentence detail, every command but the receiver's own refusals under
	// it, and a out of the record.
	behind := func(t *testing.T, name string, a decided, detail string) {
		t.Helper()
		if a.refused != RefusalCompactionBehind || a.detail != detail || a.inRecord {
			t.Fatalf("%s: answered %q (%q), in the record: %v; want compaction_behind (%q), out of it", name,
				a.refused, a.detail, a.inRecord, detail)
		}
		for _, c := range a.commands {
			if c.Reason == RefusalBlobTooLarge {
				continue
			}
			if c.Result != ResultNG || c.Reason != RefusalCompactionBehind || c.Detail != detail {
				t.Errorf("%s: %s is %s %s (%q), want ng compaction_behind under the push's sentence", name,
					c.Ref, c.Result, c.Reason, c.Detail)
			}
		}
	}
	t.Run("the last pack and every one after it", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		e.exec(`UPDATE code_repos SET live_packs = ? WHERE id = ?`, MaxPacksPerRepo-1, decideRepo)
		prefilled := cmd("refs/heads/big", ZeroOID, oid("G"))
		prefilled.Result, prefilled.Reason, prefilled.Detail = ResultNG, RefusalBlobTooLarge, "a blob of 2 GiB"
		last := withPack(proposalOf(cmd("refs/heads/one", ZeroOID, oid("1"))), size, decideAt)
		deletion := proposalOf(cmd("refs/heads/feature", oid("F"), ZeroOID))
		entries, answers := decideIn(t, e, DefaultLimits(),
			withPack(proposalOf(cmd("refs/heads/main", oid("Z"), oid("B"))), size, decideAt),
			last,
			withPack(proposalOf(cmd("refs/heads/two", ZeroOID, oid("2")),
				cmd("refs/heads/main", oid("A"), oid("C"))), size, decideAt),
			withPack(proposalOf(prefilled, cmd("refs/heads/three", ZeroOID, oid("3"))), size, decideAt),
			deletion)
		if got := answers[0]; got.commands[0].Reason != RefusalStale || got.refused != "" || got.inRecord {
			t.Fatalf("a stale push carrying a pack is %s %s (whole: %q), want stale and out of the record",
				got.commands[0].Result, got.commands[0].Reason, got.refused)
		}
		if got := answers[1]; got.commands[0].Result != ResultOK || !got.inRecord {
			t.Fatalf("the push carrying the last pack the repository takes is %s %s (whole: %q, %q), want ok",
				got.commands[0].Result, got.commands[0].Reason, got.refused, got.detail)
		}
		at := BehindDetail("web", MaxPacksPerRepo)
		behind(t, "a push past the bound", answers[2], at)
		behind(t, "the push after it", answers[3], at)
		if got := answers[3].commands[0]; got.Reason != RefusalBlobTooLarge || got.Detail != "a blob of 2 GiB" {
			t.Errorf("the receiver's own refusal became %s %q", got.Reason, got.Detail)
		}
		if got := answers[4]; got.commands[0].Result != ResultOK || !got.inRecord {
			t.Fatalf("a push carrying no pack is %s %s (whole: %q), want ok", got.commands[0].Result,
				got.commands[0].Reason, got.refused)
		}
		if len(entries) != 2 || entries[0].PushID != last.PushID || entries[1].PushID != deletion.PushID {
			t.Fatalf("the record's entries are %d, want the last pack's push and the deletion", len(entries))
		}
	})
	t.Run("past the bound", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		e.exec(`UPDATE code_repos SET live_packs = ? WHERE id = ?`, MaxPacksPerRepo+3, decideRepo)
		entries, answers := decideIn(t, e, DefaultLimits(),
			withPack(proposalOf(cmd("refs/heads/one", ZeroOID, oid("1"))), size, decideAt),
			proposalOf(cmd("refs/heads/feature", oid("F"), ZeroOID)))
		behind(t, "a push past the bound", answers[0], BehindDetail("web", MaxPacksPerRepo+3))
		if got := answers[1]; got.commands[0].Result != ResultOK || len(entries) != 1 {
			t.Fatalf("a push carrying no pack is %s %s with %d entries, want ok, the record's one",
				got.commands[0].Result, got.commands[0].Reason, len(entries))
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
		PushID: landed.PushID, Principal: landed.Principal, Via: landed.Via, Node: landed.Node,
		Commands: []Command{move("refs/heads/feature", oid("F"), oid("G"))}, ReceivedAt: decideAt,
	}))
	other := proposalOf(cmd("refs/heads/x", ZeroOID, oid("X")))
	const elsewhere = "0190f3c4-0000-7000-8000-00000000d0c2"
	e.apply(create(t, elsewhere, "elsewhere"))
	e.apply(pushOf(t, elsewhere, PushEntry{
		PushID: other.PushID, Principal: other.Principal, Via: other.Via, Node: other.Node,
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
