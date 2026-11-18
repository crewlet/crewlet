package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// BenchmarkDecideABatch times the push decide of ONE BATCH over a repository
// of ten thousand refs and [MaxProtectionsPerRepo] rules. Per arm: "formation"
// is [verdicts.formBefore] alone, on the committer goroutine, the protection
// verdicts formed before the publish holding no snapshot; "round" is a round inside the publisher's
// snapshot with those verdicts, as every round under unchanged rules runs;
// "reformed" is a round whose snapshot holds rules the formation did not read
// (an activation between the two), forming every verdict inside the snapshot:
// the longest a round holds its reader. A round runs on the repository's one
// committer goroutine holding a reader-pool connection, again for every round
// a peer's record forces (up to the publisher's sixteen); doc.go's measured
// section records the figures, and [CommitBudget] is held against the worst.
func BenchmarkDecideABatch(b *testing.B) {
	quiet(b)
	e := openApplyEstate(b)
	repository := func(id string, rules []Protection) {
		settings := settingsFor("")
		settings.Protections = rules
		if err := settings.Validate(); err != nil {
			b.Fatalf("the arm's rules are not a repository's: %v", err)
		}
		name := "bench-" + id[len(id)-2:]
		e.apply(rec(b, NameSubject(name), OpCreate,
			Scope{NamePath(NameToken(name)), RepoPath(id)},
			CreateRepo{RepoID: id, Name: name, Settings: settings}))
		e.seed(id, 0, 10_000, 0)
	}

	const typical = "0190f3c4-0000-7000-8000-00000000be01"
	var team []Protection
	for i := range MaxProtectionsPerRepo {
		team = append(team, Protection{PushRule: PushRule{
			Pattern: fmt.Sprintf("refs/heads/team%02d/release/**", i), DirectPush: DirectNone,
		}})
	}
	repository(typical, team)

	// worst asks every rule of gitref's costliest shape ([costliestRules])
	// about a record full ([filledRecord]) of [widestRef]s, none matching;
	// worstRefused adds a last rule refusing every ref (direct_push none), so
	// the sentence naming the refusing rule is formed too.
	const worst, worstRefused = "0190f3c4-0000-7000-8000-00000000be02",
		"0190f3c4-0000-7000-8000-00000000be03"
	costliest := costliestRules(MaxProtectionsPerRepo)
	repository(worst, costliest)
	// THE MATCHING RULE SORTS LAST ('a' after '*'), so the sentence's walk
	// meets every costly rule before it, as the decision's walk did.
	refusing := append(slices.Clone(costliest[:MaxProtectionsPerRepo-1]),
		Protection{PushRule: PushRule{Pattern: "refs/heads/" + widestComponent + "/**", DirectPush: DirectNone}})
	repository(worstRefused, refusing)

	// maximal is three pushes of [MaxCommandsPerPush] creates, group
	// [MaxPushesPerRecord] pushes of one; each command is asked of every team
	// rule, since none matches.
	var maximal, group []Proposal
	for p := range 3 {
		var cmds []Command
		for c := range MaxCommandsPerPush {
			cmds = append(cmds, Command{Ref: fmt.Sprintf("refs/heads/team%02d/work/%d/%04d", c%100, p, c),
				Old: ZeroOID, New: oid(fmt.Sprint(p, c))})
		}
		maximal = append(maximal, benchProposal(typical, cmds))
	}
	for p := range MaxPushesPerRecord {
		group = append(group, benchProposal(typical, []Command{{
			Ref: fmt.Sprintf("refs/heads/team%02d/work/one", p), Old: ZeroOID, New: oid(fmt.Sprint(p))}}))
	}
	widest := filledRecord(b, worst, widestRef)
	widestRefused := filledRecord(b, worstRefused, widestRef)
	// deepest: a record of [deepestRef]s, each making 118 directories, the
	// most seeks [RefusalRefConflict] asks of a round.
	deepest := filledRecord(b, typical, deepestRef)

	// buried creates every directory of [buriedChain] in one push over
	// [MaxTombstonesPerRepo] tombstones under its deepest directory, so every
	// create's range spans every tombstone.
	const buried = "0190f3c4-0000-7000-8000-00000000be04"
	repository(buried, team)
	chain := buriedChain()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('%s/t%06d', ?, i), '', 1, 0 FROM k`,
		MaxTombstonesPerRepo, buried, chain[len(chain)-1])
	e.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, MaxTombstonesPerRepo, buried)
	nested := make([]Command, len(chain))
	for i, ref := range chain {
		nested[i] = Command{Ref: ref, Old: ZeroOID, New: oid(fmt.Sprint("buried", i))}
	}
	// THE FIRST CREATE TAKES THE CHAIN; every later one is refused only after
	// every read the rule asks of it.
	firstLands := func(ref string) Refusal {
		if ref == chain[0] {
			return ""
		}
		return RefusalRefConflict
	}

	// changed moves the heads of [MaxCommandsPerPush] live changes their author
	// pushes ([estate.seedLiveChanges]), then [MaxPushesPerRecord]-1 creates
	// moving none: the most changes in [changeBook] beside the most entries
	// reaching none.
	const changed = "0190f3c4-0000-7000-8000-00000000be05"
	repository(changed, team)
	e.seedLiveChanges(changed, MaxCommandsPerPush)
	moving := make([]Command, MaxCommandsPerPush)
	for c := range moving {
		moving[c] = Command{Ref: liveSource(c), Old: liveHead(c), New: oid(fmt.Sprint("rebased", c))}
	}
	rebased := []Proposal{benchProposal(changed, moving)}
	for p := 1; p < MaxPushesPerRecord; p++ {
		rebased = append(rebased, benchProposal(changed, []Command{{
			Ref: fmt.Sprintf("refs/heads/team%02d/work/one", p), Old: ZeroOID, New: oid(fmt.Sprint(p))}}))
	}

	type arm struct {
		name, repo string
		batch      []Proposal
		// refused is every command's protection verdict, and decided what a
		// round decides of each ref — the verdict, where nil.
		refused Refusal
		decided func(ref string) Refusal
	}
	for _, shape := range []arm{
		{"maximal", typical, maximal, "", nil},
		{"group", typical, group, "", nil},
		{"worst", worst, widest, "", nil},
		{"worst-refused", worstRefused, widestRefused, RefusalProtectedDirect, nil},
		{"deepest", typical, deepest, "", nil},
		{"buried", buried, []Proposal{benchProposal(buried, nested)}, "", firstLands},
		// fanned: the chain's creates, then the rest of a record under its
		// deepest directory, so each chain create holds every name below it:
		// the most gaps a record's ranges are cut into, and the most names a
		// nested create's walk passes.
		{"fanned", typical, filledRecord(b, typical, func(p, c int) string {
			if i := p*MaxCommandsPerPush + c; i < len(chain) {
				return chain[i]
			} else {
				return chain[len(chain)-1] + fmt.Sprintf("/s%06d", i)
			}
		}), "", firstLands},
		{"changes", changed, rebased, "", nil},
	} {
		if shape.decided == nil {
			shape.decided = func(string) Refusal { return shape.refused }
		}
		ctx := context.Background()
		commands := 0
		for _, p := range shape.batch {
			commands += len(p.Commands)
		}
		// formation fails unless it formed every command's verdict as the arm
		// says, so a short formation, or one a round redoes, is never measured.
		formation := func() (*verdicts, error) {
			v := &verdicts{}
			v.formBefore(ctx, e.db.Reader(), shape.repo, shape.batch)
			if v.formed != commands {
				return nil, fmt.Errorf("the formation formed %d verdicts of %d commands", v.formed, commands)
			}
			for key, got := range v.of {
				if got.refusal != shape.refused {
					return nil, fmt.Errorf("command %v's verdict is %q, and the arm measures %q", key,
						got.refusal, shape.refused)
				}
			}
			return v, nil
		}
		round := func(v *verdicts) error {
			return e.db.Read(ctx, func(tx *sql.Tx) error {
				_, answers, _, _, err := decideBatch(ctx, tx, shape.repo, shape.batch, DefaultLimits(),
					DefaultDeletedRetention, SeatRules{}, applyClock, v)
				if err != nil {
					return err
				}
				// EVERY COMMAND DECIDED AS THE ARM SAYS, so a round cut short
				// by an unmeant refusal is never what is measured.
				for i, a := range answers {
					if len(a.commands) != len(shape.batch[i].Commands) {
						return fmt.Errorf("push %d was answered %d commands of %d", i,
							len(a.commands), len(shape.batch[i].Commands))
					}
					for _, c := range a.commands {
						if want := shape.decided(c.Ref); c.Reason != want {
							return fmt.Errorf("%s was decided %s %q, and the arm measures %q",
								c.Ref, c.Result, c.Reason, want)
						}
					}
				}
				return nil
			})
		}
		b.Run(shape.name+"/round", func(b *testing.B) {
			// AFTER THE LOOP, which resets the reported metrics as it
			// starts the timer.
			defer b.ReportMetric(float64(commands), "commands/op")
			formed, err := formation()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := round(formed); err != nil {
					b.Fatal(err)
				}
			}
			if formed.formed != commands {
				b.Fatalf("the rounds formed %d verdicts again under the formation's rules",
					formed.formed-commands)
			}
		})
		b.Run(shape.name+"/formation", func(b *testing.B) {
			defer b.ReportMetric(float64(commands), "commands/op")
			b.ReportAllocs()
			for b.Loop() {
				if _, err := formation(); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(shape.name+"/reformed", func(b *testing.B) {
			defer b.ReportMetric(float64(commands), "commands/op")
			b.ReportAllocs()
			for b.Loop() {
				v := &verdicts{}
				if err := round(v); err != nil {
					b.Fatal(err)
				}
				if v.formed != commands {
					b.Fatalf("the round formed %d verdicts of %d commands", v.formed, commands)
				}
			}
		})
	}
}

// widestComponent is one component of [widestRef]: six bytes, the width at
// which gitref measured its costliest shape (every width from one byte to
// twenty-four costs within a fifth of it).
const widestComponent = "aaaaaa"

// widestRef is the costliest ref to ask [costliestRules] about: thirty-four
// components of [widestComponent] under refs/heads/ and a last one naming the
// command, 255 bytes in all — [MaxRefNameBytes], so the run between the rule's
// two **s is tried at as many places as a ref offers.
func widestRef(push, command int) string {
	return "refs/heads/" + strings.Repeat(widestComponent+"/", 34) + fmt.Sprintf("%02d%04d", push, command)
}

// deepestRef is the ref whose create asks the most of the snapshot: a first
// component naming the command, then one-byte components to [MaxRefNameBytes]
// — 118 directories it would make, each a seek of the key, none of them a
// directory another command's create would make too.
func deepestRef(push, command int) string {
	ref := fmt.Sprintf("refs/heads/d%02d%04d", push, command)
	for len(ref)+len("/a") <= MaxRefNameBytes {
		ref += "/a"
	}
	return ref
}

// buriedChain is every directory the buried arm's tombstones sit under,
// shallowest first: refs/heads/a, then one "/a" more each while [buriedLeaf]
// still fits in [MaxRefNameBytes], 118 refs.
func buriedChain() []string {
	out := []string{"refs/heads/a"}
	for {
		next := out[len(out)-1] + "/a"
		if len(next)+len(buriedLeaf) > MaxRefNameBytes {
			return out
		}
		out = append(out, next)
	}
}

// buriedLeaf is the last component of a buried arm tombstone, as the seeding
// statement prints it.
const buriedLeaf = "/t000000"

// costliestRules are n distinct rules of gitref's costliest shape: a run of
// four globs between two **s, the first three matching every
// [widestComponent] wherever tried, the fourth failing on its last byte, one
// no component of [widestRef] holds and distinct per rule. Sorted, as a
// repository's rules are stored.
func costliestRules(n int) []Protection {
	const last = "bcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_+"
	if n > len(last) {
		panic(fmt.Sprintf("costliestRules: %d rules asked for and %d distinct bytes to end them with", n, len(last)))
	}
	glob := "*" + strings.Repeat("a*", len(widestComponent))
	out := make([]Protection, n)
	for i := range n {
		out[i] = Protection{PushRule: PushRule{
			Pattern:    "refs/heads/**/" + strings.Repeat(glob+"/", 3) + glob + last[i:i+1] + "*/**",
			DirectPush: DirectPersons,
		}}
	}
	slices.SortFunc(out, func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
	return out
}

// benchProposal is a push of cmds to repo.
func benchProposal(repo string, cmds []Command) Proposal {
	return Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo,
		Principal: Principal{Kind: PrincipalOperator, OperatorID: "op", Seat: "op"},
		Via:       ViaGit, Node: "node-a", Commands: cmds, ReceivedAt: applyClock,
	}
}

// filledRecord is the most commands one record carries of the refs refOf
// names, gathered as [Committer.gather] gathers a batch: pushes of
// [MaxCommandsPerPush] creations while the next fits the record's bytes and
// rows, then the widest push that fits what they leave.
func filledRecord(b *testing.B, repo string, refOf func(push, command int) string) []Proposal {
	b.Helper()
	push := func(p, n int) Proposal {
		cmds := make([]Command, n)
		for c := range n {
			cmds[c] = Command{Ref: refOf(p, c), Old: ZeroOID, New: oid(fmt.Sprint(repo, p, c))}
		}
		return benchProposal(repo, cmds)
	}
	bytes, rows := recordOverheadBytes, 0
	fits := func(p Proposal) (int, bool) {
		n, err := p.measure()
		if err != nil {
			b.Fatalf("a bench push is not a proposal: %v", err)
		}
		return n, bytes+n <= MaxRecordBytes && recordRows(rows+rowBound(p)) <= MaxRecordRows
	}
	var out []Proposal
	for p := 0; len(out) < MaxPushesPerRecord; p++ {
		// THE WIDEST PUSH THAT FITS, by bisection over its command count.
		lo, hi := 0, MaxCommandsPerPush
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if _, ok := fits(push(p, mid)); ok {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo == 0 {
			break
		}
		next := push(p, lo)
		n, _ := fits(next)
		bytes, rows = bytes+n, rows+rowBound(next)
		out = append(out, next)
	}
	return out
}
