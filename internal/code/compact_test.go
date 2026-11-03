package code_test

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// COMPACTIONS THROUGH THE WHOLE PATH: proposed to a committer, decided in one
// snapshot, published as one record on a real embedded broker, applied by the
// shipped applier and answered from the rows it wrote.

// packedRepo is a fresh repository on n named name holding count live packs,
// each the one-object pack of a push of its own, and their ids, sorted.
func packedRepo(t *testing.T, n *node, name string, count int) (string, []string) {
	t.Helper()
	repo := n.ensure(name, suiteSettings(), t1).RepoID
	return repo, pushPacks(t, n, repo, count)
}

// pushPacks lands count pushes on repo, each carrying a one-object pack of its
// own, and answers the packs' ids, sorted.
func pushPacks(t *testing.T, n *node, repo string, count int) []string {
	t.Helper()
	ids := make([]string, count)
	for i := range count {
		e := entry(uuid.Must(uuid.NewV7()).String(),
			set(fmt.Sprintf("refs/heads/%s", uuid.NewString()), code.ZeroOID, oidOf(uuid.NewString())))
		ids[i] = e.Pack.PackID
		n.push(repo, e)
	}
	slices.Sort(ids)
	return ids
}

// compactionOf is a compaction of repo's packs retire into ONE output holding
// objects objects, planned at n's own position, its id minted now and its
// output's key at minted — every field as a builder sets it.
func compactionOf(n *node, repo string, retire []string, objects int64, minted time.Time) code.CompactionProposal {
	cid := statelog.NewOpID(time.Now(), "")
	return code.CompactionProposal{Compaction: code.Compaction{
		RepoID: repo, CompactionID: cid, Node: n.id, Seen: n.waiter.Committed(),
		Retire: slices.Clone(retire), Add: []code.PackRef{compactOutput(cid, 0, objects, minted)},
	}}
}

// compactOutput is output i of compaction cid, a bundle of objects objects
// whose key was minted at minted.
func compactOutput(cid string, i int, objects int64, minted time.Time) code.PackRef {
	return code.PackRef{
		PackID: code.DeriveOutputPackID(cid, i), Object: objstore.KeyAt(minted),
		Hash:         objstore.HashOf([]byte(fmt.Sprint(cid, i))),
		Size:         32*objects + code.IndexBytesFor(objects) + code.ReverseBytesFor(objects),
		IndexBytes:   code.IndexBytesFor(objects),
		ReverseBytes: code.ReverseBytesFor(objects),
		GitChecksum:  oidOf(fmt.Sprint("compacted", cid, i)), Objects: objects,
	}
}

// requireApplied fails t unless res is a compaction applied at a position.
func requireApplied(t *testing.T, res code.CompactResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProposeCompaction: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" {
		t.Fatalf("the compaction answered %+v, want applied at a position", res)
	}
}

// livePacks is repo's live pack ids on n, sorted.
func (n *node) livePacks(repo string) []string {
	n.t.Helper()
	var out []string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(),
			`SELECT pack_id FROM code_packs WHERE repo_id = ? ORDER BY pack_id`, repo)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read %s's packs: %v", repo, err)
	}
	return out
}

// A COMPACTION ASKED TWICE IS ONE COMPACTION: proposed again after it landed —
// as its builder's router asks the repository's runner-up after a committer
// went silent — it is answered applied at the record it landed in, from the
// rows, and no second record is appended. Its operation id is the
// compaction's own, so the ledger collapses the second asking onto the first.
//
// Mutation: publish a compaction under an operation id minted for its batch,
// and the second asking is decided again — its inputs gone — and answered
// stale.
func TestACompactionAskedTwiceIsOneCompaction(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 3)
	c, _ := committerOn(t, n, n.pub)
	p := compactionOf(n, repo, ids, 3, harnessAt)
	first, err := c.ProposeCompaction(t.Context(), p)
	requireApplied(t, first, err)
	before := n.end()
	again, err := c.ProposeCompaction(t.Context(), p)
	requireApplied(t, again, err)
	if again.Position != first.Position {
		t.Fatalf("asked again it answered at %v, and the first at %v — one compaction, one record",
			again.Position, first.Position)
	}
	if got := n.end(); got != before {
		t.Fatalf("asking again appended %d records, want none", got-before)
	}
	if got, want := n.livePacks(repo), []string{p.Compaction.Add[0].PackID}; !slices.Equal(got, want) {
		t.Fatalf("the repository's live packs are %v, want only the output %v", got, want)
	}
	if n := n.count(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, repo); n != len(ids) {
		t.Fatalf("%d retired rows, want the %d inputs", n, len(ids))
	}
}

// A COMPACTION RETRIED AFTER ITS OUTPUTS WERE RETIRED IS APPLIED: a later
// compaction may merge its outputs before its builder hears back — its answer
// lost, its builder's router asking the runner-up under the same id — and the
// ledger collapses the asking onto the record that landed, whose outputs are
// rows of code_packs_retired by then, which keep the position each was added
// at. It is answered applied at that record, from those rows, with no record
// appended and nothing logged as a domain that lies.
//
// Mutation: answer from code_packs alone, and the asking is answered unknown
// and logged code_compaction_unanswerable at ERROR — and the node that built
// the compaction adopts nothing.
func TestACompactionRetriedAfterItsOutputsWereRetiredIsApplied(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 3)
	logs := &lineLog{}
	c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	p := compactionOf(n, repo, ids[:2], 2, harnessAt)
	first, err := c.ProposeCompaction(t.Context(), p)
	requireApplied(t, first, err)
	out := p.Compaction.Add[0].PackID
	later := compactionOf(n, repo, sorted(out, ids[2]), 3, harnessAt)
	res, err := c.ProposeCompaction(t.Context(), later)
	requireApplied(t, res, err)
	if got, want := n.livePacks(repo), []string{later.Compaction.Add[0].PackID}; !slices.Equal(got, want) ||
		n.count(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ? AND pack_id = ?`, repo, out) != 1 {
		t.Fatalf("the premise: the live packs are %v, want only the later output %v, and the first "+
			"compaction's output %s retired", got, want, out)
	}

	before := n.end()
	again, err := c.ProposeCompaction(t.Context(), p)
	requireApplied(t, again, err)
	if again.Position != first.Position {
		t.Fatalf("asked again it answered at %v, and the first at %v — one compaction, one record",
			again.Position, first.Position)
	}
	if got := n.end(); got != before {
		t.Fatalf("asking again appended %d records, want none", got-before)
	}
	if lines := logs.lines("code_compaction_unanswerable"); len(lines) != 0 {
		t.Fatalf("asking again logged %v; an output a later compaction retired is a row this node holds",
			lines)
	}
}

// A COMPACTION OF A PACK NO LONGER LIVE IS STALE, and so is one of a pack that
// is full or split into parts: every input is held to the rows of the
// committer's snapshot — one row, of part `pack`, live and not full
// (code.PackRow.Full) — and the first that is not is named, the compaction
// refused undecided, nothing appended, and the next pass plans again.
//
// Mutation: drop the full check, and a compaction retiring a full pack is
// applied — and merged again at every pass.
func TestACompactionOfAPackNoLongerLiveIsStale(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		// settle readies the repository's rows and answers the inputs a
		// compaction then names, and the one of them that is refused.
		settle func(t *testing.T, n *node, c *code.Committer, repo string, ids []string) ([]string, string)
		why    string
	}{
		"retired by a compaction first": {func(t *testing.T, n *node, c *code.Committer, repo string,
			ids []string) ([]string, string) {
			res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids[:2], 2, harnessAt))
			requireApplied(t, res, err)
			return []string{ids[1], ids[2]}, ids[1]
		}, "is no longer live"},
		"full by its bundle": {func(t *testing.T, n *node, _ *code.Committer, repo string,
			ids []string) ([]string, string) {
			id := uuid.Must(uuid.NewV7()).String()
			e := entry(id, set("refs/heads/full", code.ZeroOID, oidOf("full")))
			e.Pack.Size = code.FullBundleBytes
			n.push(repo, e)
			return sorted(ids[0], e.Pack.PackID), e.Pack.PackID
		}, "is full"},
		"split into parts": {func(t *testing.T, n *node, _ *code.Committer, repo string,
			ids []string) ([]string, string) {
			n.exec(`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, state, added_position)
				SELECT repo_id, pack_id, 'part-2', object, hash, size, index_bytes, reverse_bytes,
					git_checksum, objects, kind, state, added_position
				FROM code_packs WHERE repo_id = ? AND pack_id = ?`, repo, ids[0])
			return ids[:2], ids[0]
		}, "is split into parts"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			repo, ids := packedRepo(t, n, "web", 3)
			c, _ := committerOn(t, n, n.pub)
			retire, refused := tc.settle(t, n, c, repo, ids)
			before := n.end()
			res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, retire, 1, harnessAt))
			if err != nil {
				t.Fatalf("ProposeCompaction: %v", err)
			}
			if res.Refusal != code.RefusalStale || res.Outcome != "" ||
				!strings.Contains(res.Detail, "pack "+refused+" "+tc.why) {
				t.Fatalf("the compaction answered %+v, want stale saying pack %s %s", res, refused, tc.why)
			}
			if got := n.end(); got != before {
				t.Fatalf("a stale compaction appended %d records, want none", got-before)
			}
		})
	}
}

// sorted is ids in order.
func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// A COMPACTION IS REFUSED FOR WHAT ITS REPOSITORY IS: a deleted one is
// repo_deleted, told — through the committer's own retention — when its
// purge may begin, as a push to it is; one BEING PURGED, one PURGED to its
// marker and an id no repository ever had are repo_deleted too, each in the
// sentence of its own state (code.RepoGone), and none appends a record; an
// ARCHIVED one is compacted, since it is still read and every cold sync of it
// pays for every pack it holds.
//
// Mutations: refuse an archived repository's compaction as a push to it is
// refused, and an archived repository is never compacted; drop the rule's
// check of what the repository is, and a compaction is decided and applied
// against one being purged, whose packs are still live between the purge's
// parts, while one purged or never created is answered stale — as if a next
// pass could plan again.
func TestACompactionIsRefusedForWhatItsRepositoryIs(t *testing.T) {
	t.Parallel()
	const retention = 11 * 24 * time.Hour
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo, ids := packedRepo(t, n, "web", 2)
		if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), repo); err != nil {
			t.Fatal(err)
		}
		n.drain()
		c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
			o.Retention = func() time.Duration { return retention }
		})
		before := n.end()
		res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
		if err != nil {
			t.Fatal(err)
		}
		var deleted int64
		if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT deleted_at FROM code_repos WHERE id = ?`,
				repo).Scan(&deleted)
		}); err != nil {
			t.Fatal(err)
		}
		want := "may begin from " + store.DecodeTime(deleted).Add(retention).UTC().Format(time.RFC3339)
		if res.Refusal != code.RefusalRepoDeleted || !strings.Contains(res.Detail, want) {
			t.Fatalf("the compaction answered %+v, want repo_deleted saying %q", res, want)
		}
		if got := n.end(); got != before {
			t.Fatalf("a refused compaction appended %d records, want none", got-before)
		}
	})
	t.Run("archived", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo, ids := packedRepo(t, n, "web", 2)
		s := suiteSettings()
		s.Archived = true
		n.ensure("web", s, t2)
		c, _ := committerOn(t, n, n.pub)
		res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
		requireApplied(t, res, err)
	})
	const never = "0190f3c4-0000-7000-8000-00000000dead"
	for name, tc := range map[string]struct {
		// settle readies the node's rows and answers the repository a
		// compaction then names, and the packs it retires.
		settle func(t *testing.T, n *node) (string, []string)
		says   string
	}{
		"purging": {func(t *testing.T, n *node) (string, []string) {
			repo, ids := packedRepo(t, n, "web", 2)
			n.deleteAt(repo, harnessAt.Add(-2*keep))
			// ONE ROW PAST A PART, so the purge's first step is a part —
			// its pushes first, its packs left live — and the repository
			// is left purging.
			n.seedRows(repo, code.PurgePartRows+1, 0, 0)
			if res, err := n.store.PurgeStep(t.Context(), repo, keep); err != nil || res.Done {
				t.Fatalf("PurgeStep = %+v, %v; want one part, the purge not done", res, err)
			}
			if row, found := n.repo("web"); !found || row.status != string(code.RepoStatusPurging) {
				t.Fatalf("the premise: web is %+v (found %v), want it purging", row, found)
			}
			return repo, ids
		}, "repository web is being purged"},
		"purged": {func(t *testing.T, n *node) (string, []string) {
			repo, ids := packedRepo(t, n, "web", 2)
			n.deleteAt(repo, harnessAt.Add(-2*keep))
			var p purged
			if report, err := passOver(n.store, n.reader, &p).Pass(t.Context()); err != nil ||
				!slices.Equal(report.Purged, []string{"web"}) {
				t.Fatalf("Pass = %+v, %v; want web purged", report, err)
			}
			return repo, ids
		}, "repository web was purged"},
		"never created": {func(_ *testing.T, n *node) (string, []string) {
			// ANOTHER REPOSITORY'S RECORD, so the log holds a position the
			// compaction names as planned at, as every builder's does.
			n.ensure("web", suiteSettings(), t1)
			return never, sorted(uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String())
		}, "repository " + never + " does not exist"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			repo, ids := tc.settle(t, n)
			c, _ := committerOn(t, n, n.pub)
			before := n.end()
			res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
			if err != nil {
				t.Fatal(err)
			}
			if res.Refusal != code.RefusalRepoDeleted || res.Outcome != "" ||
				!strings.Contains(res.Detail, tc.says) {
				t.Fatalf("the compaction answered %+v, want repo_deleted saying %q", res, tc.says)
			}
			if got := n.end(); got != before {
				t.Fatalf("a refused compaction appended %d records, want none", got-before)
			}
		})
	}
}

// A COMPACTION IS DECIDED AT THE PACK BOUND, AND MAKES ROOM: the bound holds
// pushes back and never a compaction, which adds no pack and is the one way a
// repository at MaxPacksPerRepo live packs ever takes a pack again. At the
// bound, and past it as a later build's committer may leave a repository, a
// push carrying a pack is refused compaction_behind whole and undecided,
// while a compaction of two of the repository's live packs into one is
// applied, its live packs dropping by exactly one, since the apply counts the
// rows it moves. At the bound that one pack is room, and the same push run
// again is an entry of a record; past it the bound still holds the push back,
// its sentence naming what the repository still holds.
//
// Each repository holds two real live packs and its row counts the rest: the
// decides read the count off the row and the inputs off their own rows, so
// the case stores no two thousand pushes.
//
// Mutation: refuse a compaction of a repository holding MaxPacksPerRepo live
// packs or more, and both arms fail on the compaction's answer — the build
// under which a repository that reached the bound would refuse every push
// carrying a pack for good.
func TestACompactionIsDecidedAtThePackBoundAndMakesRoom(t *testing.T) {
	t.Parallel()
	for name, held := range map[string]int64{
		"at the bound":   code.MaxPacksPerRepo,
		"past the bound": code.MaxPacksPerRepo + 3,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			repo, ids := packedRepo(t, n, "web", 2)
			n.exec(`UPDATE code_repos SET live_packs = ? WHERE id = ?`, held, repo)
			c, _ := committerOn(t, n, n.pub)
			// push proposes a push carrying a pack, creating a branch of
			// its own.
			push := func() answer {
				ref := "refs/heads/" + uuid.NewString()
				res, err := c.Propose(t.Context(), packed(proposal(repo,
					update(ref, code.ZeroOID, oidOf(ref))), harnessAt))
				return answer{res, err}
			}
			// behind fails t unless a is a refusal of the whole push at the
			// bound, undecided, its sentence naming packs live packs.
			behind := func(when string, a answer, packs int64) {
				t.Helper()
				want := code.BehindDetail("web", packs)
				if a.err != nil || a.res.Outcome != "" || a.res.Answer.Refused != code.RefusalCompactionBehind ||
					a.res.Answer.Detail != want {
					t.Fatalf("%s, a push carrying a pack answered %q %q (%q), %v; want compaction_behind (%q), "+
						"undecided", when, a.res.Outcome, a.res.Answer.Refused, a.res.Answer.Detail, a.err, want)
				}
				requireOnePlacement(t, a.res.Answer)
			}
			behind("before the compaction", push(), held)

			res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
			requireApplied(t, res, err)
			left := int64(n.count(`SELECT live_packs FROM code_repos WHERE id = ?`, repo))
			if left != held-1 {
				t.Fatalf("the compaction left %d live packs of %d, want %d: two retired, one added", left, held,
					held-1)
			}
			if left >= code.MaxPacksPerRepo {
				behind("still past the bound after the compaction", push(), left)
				return
			}
			requireOK(t, push())
		})
	}
}

// A COMPACTION THAT INVENTS OBJECTS IS REFUSED BEFORE ITS RECORD: outputs that
// hold more objects than the rows of the packs they replace count are a
// builder's bug — a merge writes no object its inputs did not hold — so the
// decide refuses it ErrInvalidProposal, nothing is appended, and the node logs
// it at ERROR where an operator reads it. No lower bound: outputs that hold
// FEWER are inputs that shared objects, merged once.
//
// Mutation: drop the arithmetic, and the compaction lands.
func TestACompactionThatInventsObjectsIsRefusedBeforeItsRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 3)
	logs := &lineLog{}
	c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	before := n.end()
	res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 4, harnessAt))
	if !errors.Is(err, code.ErrInvalidProposal) || res != (code.CompactResult{}) {
		t.Fatalf("a compaction whose outputs hold 4 objects of the inputs' 3 answered %+v, %v; want "+
			"ErrInvalidProposal", res, err)
	}
	if got := n.end(); got != before {
		t.Fatalf("an invented object appended %d records, want none", got-before)
	}
	if lines := logs.lines("code_compaction_invalid"); len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("the refusal was logged %v, want code_compaction_invalid once at ERROR", lines)
	}
	res, err = c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
	requireApplied(t, res, err)
}

// A COMPACTION IS NEVER BATCHED WITH PUSHES: queued between two pushes behind
// a batch in flight, it is decided as a record of its own, after the push
// before it and before the push after it — a record carries one op, and the
// repository's proposals land in the order they arrived.
//
// Mutation: let a batch gather a compaction behind its pushes, and the three
// are decided as one push record.
func TestACompactionIsNeverBatchedWithPushes(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 2)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", code.ZeroOID, oidOf("A"))))
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	before := n.end()

	ahead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B"))))
	eventually(t, "the push ahead queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
	compacted := make(chan struct {
		res code.CompactResult
		err error
	}, 1)
	go func() {
		res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
		compacted <- struct {
			res code.CompactResult
			err error
		}{res, err}
	}()
	eventually(t, "the compaction queued", func() bool { return code.CommitterQueued(c, repo) == 2 })
	behind := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/main", oidOf("B"), oidOf("C"))))
	eventually(t, "the push behind queued", func() bool { return code.CommitterQueued(c, repo) == 3 })
	gate.open()

	requireOK(t, await(t, lead))
	first, second := await(t, ahead), await(t, behind)
	requireOK(t, first)
	requireOK(t, second)
	var got struct {
		res code.CompactResult
		err error
	}
	select {
	case got = <-compacted:
	case <-time.After(30 * time.Second):
		t.Fatal("the compaction was not answered in 30 s")
	}
	requireApplied(t, got.res, got.err)
	positions := []uint64{first.res.Position.Seq, got.res.Position.Seq, second.res.Position.Seq}
	if !slices.IsSorted(positions) || positions[0] == positions[1] || positions[1] == positions[2] {
		t.Fatalf("the push ahead, the compaction and the push behind landed at %v, want three records "+
			"in that order", positions)
	}
	if appended := n.end() - before; appended != 4 {
		t.Fatalf("%d records appended, want 4: the lead's, then the push, the compaction and the push "+
			"each alone", appended)
	}
}

// A COMPACTION BEHIND WHAT ITS BUILDER SAW IS ASKED OF ANOTHER NODE: decided
// from a snapshot at or past its Seen — the rows the builder planned from — or,
// where the committer's node cannot reach that position in time, refused about
// that node (ErrNotServing), which the router reads as a node that ran nothing
// and asks the repository's runner-up under the same compaction id. Nothing is
// appended.
//
// Mutation: publish with no session floor, and the node behind decides the
// compaction from rows that do not hold the inputs — stale, for ever, from the
// node that would be asked first.
func TestACompactionIsDecidedPastWhatItsBuilderSaw(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	repo, ids := packedRepo(t, a, "web", 2)
	b.hold()
	c, _ := committerOn(t, b, b.pub)
	before := b.end()
	res, err := c.ProposeCompaction(t.Context(), compactionOf(a, repo, ids, 2, harnessAt))
	if !errors.Is(err, code.ErrNotServing) {
		t.Fatalf("a committer behind the compaction's Seen answered %+v, %v; want ErrNotServing", res, err)
	}
	if got := b.end(); got != before {
		t.Fatalf("%d records appended, want none", got-before)
	}
	b.release()
	res, err = c.ProposeCompaction(t.Context(), compactionOf(a, repo, ids, 2, harnessAt))
	requireApplied(t, res, err)
}

// A COMPACTION IS ANSWERED ONLY FROM ITS OUTPUTS' ROWS: a publish that answers
// applied for a record that wrote no row is answered unknown, never applied —
// which would hand its builder outputs to adopt that no row names — and logged
// at ERROR, since no committer of this domain forms such a record.
//
// Mutation: answer applied from the publish's word, and the compaction is
// applied with no output in the rows.
func TestACompactionIsAnsweredOnlyFromItsOutputsRows(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 2)
	logs := &lineLog{}
	c, _ := committerOn(t, n, decideOnly{n}, func(o *code.CommitterOptions) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	res, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
	if err != nil || res.Outcome != statelog.OutcomeUnknown || !strings.Contains(res.Detail, "not in the rows") {
		t.Fatalf("a compaction whose record wrote no row answered %+v, %v; want unknown", res, err)
	}
	if lines := logs.lines("code_compaction_unanswerable"); len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("logged %v, want code_compaction_unanswerable once at ERROR", lines)
	}
}

// A BUSY COMPACTION IS COUNTED APART FROM A BUSY PUSH, and a busy expunge
// record from both: all meet the same queue and the same node's bounds, and
// all are refused busy undecided past them — a push's commands each refused, a
// compaction and an expunge record whole — each counted under its own `kind`,
// so an operator tells pushes refused for a backlog from a compaction or an
// expunge that met one.
//
// Mutation: count every busy refusal without its kind, and no series names
// whose proposal it was.
func TestABusyCompactionIsCountedApartFromABusyPush(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 2)
	c, rec := committerOn(t, n, n.pub)
	code.SetCommitterNodeBytes(c, 1)
	before := n.end()
	push, err := c.Propose(t.Context(), proposal(repo, update("refs/heads/main", code.ZeroOID, oidOf("A"))))
	if err != nil || push.Answer.Refused != code.RefusalBusy {
		t.Fatalf("a push past the node's bytes answered %+v, %v; want busy", push.Answer, err)
	}
	compaction, err := c.ProposeCompaction(t.Context(), compactionOf(n, repo, ids, 2, harnessAt))
	if err != nil || compaction.Refusal != code.RefusalBusy || compaction.Outcome != "" ||
		!strings.Contains(compaction.Detail, "across this node's repositories") {
		t.Fatalf("a compaction past the node's bytes answered %+v, %v; want busy, the node full", compaction, err)
	}
	ask := purgeOf(repo)
	expunge, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Request: &ask})
	if err != nil || expunge.Refusal != code.RefusalBusy || expunge.Outcome != "" ||
		!strings.Contains(expunge.Detail, "across this node's repositories") {
		t.Fatalf("an expunge record past the node's bytes answered %+v, %v; want busy, the node full", expunge, err)
	}
	for _, kind := range []string{"push", "compaction", "expunge"} {
		attrs := metrics.Attrs{"kind": kind, "reason": "node"}
		if got := readingOf(rec, metrics.CodeCommitBusy, attrs).Total; got != 1 {
			t.Errorf("the busy counter reads %d under %v, want 1", got, attrs)
		}
	}
	if got := n.end(); got != before {
		t.Fatalf("busy refusals appended %d records, want none", got-before)
	}
}

// readingOf is one instrument's series in rec with exactly attrs.
func readingOf(rec *metrics.Recorder, name string, attrs metrics.Attrs) metrics.Snapshot {
	for _, s := range rec.Read() {
		if s.Name == name && maps.Equal(s.Attrs, map[string]string(attrs)) {
			return s
		}
	}
	return metrics.Snapshot{}
}

// lineLog is a logger's output a case reads back, one JSON object a line.
type lineLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines is every line logged under msg, decoded.
func (l *lineLog) lines(msg string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	scan := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for scan.Scan() {
		var line map[string]any
		if json.Unmarshal(scan.Bytes(), &line) == nil && line["msg"] == msg {
			out = append(out, line)
		}
	}
	return out
}

// THE DUE LISTING IS MOST PACKS FIRST: every active and archived repository
// holding at least the floor of live packs, the most first and then by id,
// each with the version its row holds — paged by its cursor, a page's rows in
// that order across the pages — and never a deleted one, nor one under the
// floor. THREE REPOSITORIES TIE for the most, so a page of two ends INSIDE
// the tie, and the next resumes within it: after the cursor's id among the
// repositories at the cursor's count, then below that count, each listed
// once.
//
// Mutations: order the listing by id, and a repository near the bound waits
// behind every barely due one; drop or blank the cursor's `id < ?` arm, and
// the page after the cursor skips the rest of the tie, passed over for a
// poll however near MaxPacksPerRepo it stands; resume by `id > ?`, and the
// page lists the tie's first again in place of its rest.
func TestTheDueListingIsMostPacksFirst(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	// Every count is the floor's, so the case holds whatever the floor is: a
	// just past it, d and g above it, the tie (b, c and h) above them, f the
	// most and deleted, and e under it.
	floor := code.CompactMinMerge
	packs := map[string]int{"a": floor + 1, "b": floor + 22, "c": floor + 22, "d": floor + 4, "e": floor - 5,
		"f": floor + 42, "g": floor + 12, "h": floor + 22}
	ids := map[string]string{}
	for name, count := range packs {
		ids[name] = n.ensure(name, suiteSettings(), t1).RepoID
		n.exec(`UPDATE code_repos SET live_packs = ?, live_objects = ? WHERE id = ?`, count, 1000*count, ids[name])
	}
	// AN ARCHIVED ONE IS LISTED, a deleted one never.
	n.exec(`UPDATE code_repos SET status = 'archived' WHERE id = ?`, ids["d"])
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), ids["f"]); err != nil {
		t.Fatal(err)
	}
	n.drain()
	code.SetPageLimits(n.reader, 2, code.MaxPageBytes)
	var (
		got   []code.CompactionCandidate
		pages int
		// onPage is the page each repository was listed on.
		onPage = map[string]int{}
	)
	q := code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: code.CompactMinMerge,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	for {
		page, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, c := range page.Due {
			onPage[c.ID] = pages
		}
		got = append(got, page.Due...)
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
		if pages > 10 {
			t.Fatal("the listing did not end")
		}
	}
	// THE TIE, by id descending, and then the rest by their counts.
	want := []string{ids["b"], ids["c"], ids["h"]}
	slices.Sort(want)
	slices.Reverse(want)
	want = append(want, ids["g"], ids["d"], ids["a"])
	var listed []string
	for _, c := range got {
		listed = append(listed, c.ID)
		var version int64
		if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT version FROM code_repos WHERE id = ?`,
				c.ID).Scan(&version)
		}); err != nil {
			t.Fatal(err)
		}
		if c.Version.Packed() != version || c.Name == "" || int(c.LivePacks) != packs[c.Name] ||
			int(c.LiveObjects) != 1000*packs[c.Name] {
			t.Errorf("%s is listed as %+v, want its name, its %d live packs holding %d objects and its row's "+
				"version %d", c.ID, c, packs[c.Name], 1000*packs[c.Name], version)
		}
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("the due listing is %v, want %v: most live packs first, then by id, each once, no deleted "+
			"repository and none under the floor", listed, want)
	}
	// THE PREMISE: a page ended inside the tie, so a page began after a
	// cursor whose count the rest of the tie shares.
	if onPage[want[1]] == onPage[want[2]] {
		t.Fatalf("the tie at %d was listed on one page (%d); the case needs a page to end inside it",
			packs["b"], onPage[want[1]])
	}
	if pages < 3 {
		t.Fatalf("the listing took %d pages of two, want at least three", pages)
	}
}

// A DUTY LISTING NAMES NO ASKER: the compactor's `compaction_due` read, the
// publisher's `publication_due` read, and the maintainer's `expunge_due` read
// and its `expunge_tips` read of a repository's ref drops, and a home's
// `mirror_push` read of the push mirror and the green rows it publishes, are
// the engine's own, so the domain refuses each with an asker — the one place
// every surface a principal reads the code host through meets — answers it
// with none, and still refuses a principal's read that names none; and each
// takes nothing its listing does not read.
//
// Mutation: accept an asker on any duty listing, and a principal's read could
// list every repository's packs past its grant — or the objects a
// repository's history left behind.
func TestADutyListingNamesNoAsker(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := n.ensure("web", suiteSettings(), t1).RepoID
	stale := statelog.Freshness{Level: statelog.ReadStale}
	tips := &code.ExpungeTipsQuery{RepoID: web, Since: harnessAt}
	push := &code.MirrorPushQuery{RepoID: web}
	for name, tc := range map[string]struct {
		q     code.ReadQuery
		field string
	}{
		"a duty listing naming an asker": {code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: 1,
			Asker: asker, Fresh: stale}, "asker"},
		"a duty listing naming no floor": {code.ReadQuery{Kind: code.ReadCompactionDue, Fresh: stale},
			"min_packs"},
		"a duty listing naming a limit": {code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: 1, Limit: 5,
			Fresh: stale}, "limit"},
		"a publication listing naming an asker": {code.ReadQuery{Kind: code.ReadPublicationDue, Asker: asker,
			Fresh: stale}, "asker"},
		"a publication listing naming a floor": {code.ReadQuery{Kind: code.ReadPublicationDue, MinPacks: 1,
			Fresh: stale}, "min_packs"},
		"a publication listing naming a limit": {code.ReadQuery{Kind: code.ReadPublicationDue, Limit: 5,
			Fresh: stale}, "limit"},
		"a publication listing naming a repository": {code.ReadQuery{Kind: code.ReadPublicationDue, Repo: "web",
			Fresh: stale}, "repo"},
		"an expunge listing naming an asker": {code.ReadQuery{Kind: code.ReadExpungeDue, Asker: asker,
			Fresh: stale}, "asker"},
		"an expunge listing naming a floor": {code.ReadQuery{Kind: code.ReadExpungeDue, MinPacks: 1,
			Fresh: stale}, "min_packs"},
		"an expunge listing naming a limit": {code.ReadQuery{Kind: code.ReadExpungeDue, Limit: 5,
			Fresh: stale}, "limit"},
		"an import listing naming an asker": {code.ReadQuery{Kind: code.ReadImportsDue, Asker: asker,
			Fresh: stale}, "asker"},
		"an import listing naming a limit": {code.ReadQuery{Kind: code.ReadImportsDue, Limit: 5,
			Fresh: stale}, "limit"},
		"a mirror listing naming an asker": {code.ReadQuery{Kind: code.ReadMirrorsDue, Asker: asker,
			Fresh: stale}, "asker"},
		"a mirror listing naming a repository": {code.ReadQuery{Kind: code.ReadMirrorsDue, Repo: "web",
			Fresh: stale}, "repo"},
		"a tips read naming an asker": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips,
			Asker: asker, Fresh: stale}, "asker"},
		"a tips read naming no repository": {code.ReadQuery{Kind: code.ReadExpungeTips, Fresh: stale},
			"expunge_tips"},
		"a tips read naming no instant": {code.ReadQuery{Kind: code.ReadExpungeTips,
			ExpungeTips: &code.ExpungeTipsQuery{RepoID: web}, Fresh: stale}, "expunge_tips.since"},
		"a tips read naming a limit": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips, Limit: 5,
			Fresh: stale}, "limit"},
		"a tips read naming a repository by name": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips,
			Repo: "web", Fresh: stale}, "repo"},
		"an expunge listing naming tips": {code.ReadQuery{Kind: code.ReadExpungeDue, ExpungeTips: tips,
			Fresh: stale}, "expunge_tips"},
		"a push mirror read naming an asker": {code.ReadQuery{Kind: code.ReadMirrorPush, MirrorPush: push,
			Asker: asker, Fresh: stale}, "asker"},
		"a push mirror read naming no repository": {code.ReadQuery{Kind: code.ReadMirrorPush, Fresh: stale},
			"mirror_push"},
		"a push mirror read naming a repository by name": {code.ReadQuery{Kind: code.ReadMirrorPush,
			MirrorPush: &code.MirrorPushQuery{RepoID: "web"}, Fresh: stale}, "mirror_push.repo_id"},
		"a push mirror read naming a cursor": {code.ReadQuery{Kind: code.ReadMirrorPush, MirrorPush: push,
			Cursor: "x", Fresh: stale}, "cursor"},
		"a push mirror read naming a commit": {code.ReadQuery{Kind: code.ReadMirrorPush, MirrorPush: push,
			Commit: strings.Repeat("a", 40), Fresh: stale}, "commit"},
		"a mirror listing naming a push mirror": {code.ReadQuery{Kind: code.ReadMirrorsDue, MirrorPush: push,
			Fresh: stale}, "mirror_push"},
		"a tips read naming a home's change work": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips,
			ChangesDue: &code.ChangesDueQuery{RecheckBefore: harnessAt}, Fresh: stale}, "changes_due"},
		"a tips read naming a home's check work": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips,
			ChecksDue: &code.ChecksDueQuery{RepoID: web}, Fresh: stale}, "checks_due"},
		"a tips read naming a change's checks": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips,
			Checks: &code.CheckQuery{Change: 1}, Fresh: stale}, "checks"},
		"a tips read asking for path owners": {code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: tips,
			PathOwners: true, Fresh: stale}, "path_owners"},
		"a change work listing naming tips": {code.ReadQuery{Kind: code.ReadChangesDue,
			ChangesDue: &code.ChangesDueQuery{RecheckBefore: harnessAt}, ExpungeTips: tips, Fresh: stale},
			"expunge_tips"},
		"a check work listing naming tips": {code.ReadQuery{Kind: code.ReadChecksDue, ExpungeTips: tips,
			ChecksDue: &code.ChecksDueQuery{RefreshBefore: harnessAt}, Fresh: stale}, "expunge_tips"},
		"a principal's listing naming no asker": {code.ReadQuery{Kind: code.ReadRepos, Fresh: stale}, "asker"},
		"a principal's listing naming a floor": {code.ReadQuery{Kind: code.ReadRepos, Asker: asker, MinPacks: 1,
			Fresh: stale}, "min_packs"},
		"a principal's listing naming tips": {code.ReadQuery{Kind: code.ReadRepos, Asker: asker, ExpungeTips: tips,
			Fresh: stale}, "expunge_tips"},
	} {
		if _, err := n.reader.Read(t.Context(), tc.q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Read = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	for _, q := range []code.ReadQuery{
		{Kind: code.ReadCompactionDue, MinPacks: 1, Fresh: stale},
		{Kind: code.ReadPublicationDue, Fresh: stale},
		{Kind: code.ReadExpungeDue, Fresh: stale},
		{Kind: code.ReadExpungeTips, ExpungeTips: tips, Fresh: stale},
	} {
		if _, err := n.reader.Read(t.Context(), q); err != nil {
			t.Fatalf("a %s listing naming no asker: %v", q.Kind, err)
		}
		q.Cursor = "not-a-cursor"
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrBadCursor) {
			t.Fatalf("a %s listing with a cursor it never issued: %v, want ErrBadCursor", q.Kind, err)
		}
	}
}

// A COMPACTION IS CHECKED UNSTORED AS IT WILL BE STORED: ValidateUnstored
// answers what Validate answers once every output's put has filled in its key
// and digest — a compaction both take, one writing more outputs than a
// compaction records both refuse, an output whose sizes lay out no bundle
// both refuse — so the git node that built one holds it to every rule its
// committer will before a byte of it is uploaded (internal/code/compact). An
// output already stored is refused: Validate is the check of that.
//
// Mutation: hold the outputs to Validate as they stand — no key, no digest —
// rather than as they will be stored, and the compaction both take is refused
// unstored.
func TestAnUnstoredCompactionIsCheckedAsItWillBeStored(t *testing.T) {
	t.Parallel()
	cid := statelog.NewOpID(time.Now(), "")
	stored := code.CompactionProposal{Compaction: code.Compaction{
		RepoID: uuid.NewString(), CompactionID: cid, Node: "node-a",
		Seen:   statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9},
		Retire: sorted(uuid.NewString(), uuid.NewString(), uuid.NewString()),
		Add:    []code.PackRef{compactOutput(cid, 0, 2, harnessAt)},
	}}
	unstored := func(p code.CompactionProposal) code.CompactionProposal {
		add := slices.Clone(p.Compaction.Add)
		for i := range add {
			add[i].Object, add[i].Hash = objstore.Key{}, ""
		}
		p.Compaction.Add = add
		return p
	}
	if err := stored.Validate(); err != nil {
		t.Fatalf("the stored compaction the case builds is refused: %v", err)
	}
	if err := unstored(stored).ValidateUnstored(); err != nil {
		t.Errorf("an unstored compaction whose stored form validates: %v", err)
	}
	if err := stored.ValidateUnstored(); !errors.Is(err, code.ErrInvalidProposal) {
		t.Errorf("ValidateUnstored of a stored output: %v, want ErrInvalidProposal", err)
	}

	badSizes := stored
	badSizes.Compaction.Add = slices.Clone(stored.Compaction.Add)
	badSizes.Compaction.Add[0].IndexBytes++
	tooMany := stored
	tooMany.Compaction.Retire, tooMany.Compaction.Add = nil, nil
	for i := range code.MaxCompactOutputs + 1 {
		tooMany.Compaction.Retire = append(tooMany.Compaction.Retire, uuid.NewString())
		tooMany.Compaction.Add = append(tooMany.Compaction.Add, compactOutput(cid, i, 2, harnessAt))
	}
	slices.Sort(tooMany.Compaction.Retire)
	for name, p := range map[string]code.CompactionProposal{
		"an output laying out no bundle":        badSizes,
		"more outputs than a compaction writes": tooMany,
	} {
		stored, unstored := p.Validate(), unstored(p).ValidateUnstored()
		if stored == nil || unstored == nil || stored.Error() != unstored.Error() {
			t.Errorf("%s: stored %v, unstored %v — want one refusal", name, stored, unstored)
		}
	}
}

// A PACK IS FULL AT ITS CUT, SCALED: FullAt at the production cut is Full — a
// pack section at FullPackBytes, or a bundle at FullBundleBytes — and at a
// smaller cut both bounds scale by that cut's share of CompactPackBytes, so an
// output of a suite's cut, built over mebibytes (internal/code/compact's test
// knob), is full by the rule production's outputs are, and a plan made at that
// cut never merges it again.
//
// Mutation: hold FullAt to the production bounds whatever its cut, and no
// output of a scaled cut is ever full.
func TestAPackIsFullAtItsCutScaled(t *testing.T) {
	t.Parallel()
	const scaled = 4 << 20
	// row is a pack of pack bytes in a bundle of bundle bytes, its indexes
	// the rest.
	row := func(pack, bundle int64) code.PackRow {
		return code.PackRow{PackID: "p", Size: bundle, IndexBytes: bundle - pack, GitChecksum: oidOf("p"),
			Objects: 1}
	}
	fullPack := code.FullPackBytes / (code.CompactPackBytes / scaled)
	for name, tc := range map[string]struct {
		row code.PackRow
		cut int64
		// full is Full's answer, and fullAt FullAt's at cut.
		full, fullAt bool
	}{
		"a pack section at the bound": {row(code.FullPackBytes, code.FullPackBytes+64), code.CompactPackBytes,
			true, true},
		"a pack section a byte under": {row(code.FullPackBytes-1, code.FullPackBytes-1), code.CompactPackBytes,
			false, false},
		"a bundle at the bound": {row(code.FullPackBytes/2, code.FullBundleBytes), code.CompactPackBytes,
			true, true},
		"a scaled pack section at its bound": {row(fullPack, fullPack+64), scaled, false, true},
		"a scaled pack a byte under":         {row(fullPack-1, fullPack-1), scaled, false, false},
		"a scaled bundle at its bound":       {row(fullPack/2, fullPack), scaled, false, true},
	} {
		if got := tc.row.Full(); got != tc.full {
			t.Errorf("%s: Full = %v, want %v", name, got, tc.full)
		}
		if got := tc.row.FullAt(code.CompactPackBytes); got != tc.row.Full() {
			t.Errorf("%s: FullAt the production cut = %v and Full %v — want one rule", name, got,
				tc.row.Full())
		}
		if got := tc.row.FullAt(tc.cut); got != tc.fullAt {
			t.Errorf("%s: FullAt(%d) = %v, want %v", name, tc.cut, got, tc.fullAt)
		}
	}
}
