package code

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// BenchmarkDecideABatch is the push decide of ONE BATCH over a repository of
// ten thousand refs and [MaxProtectionsPerRepo] rules, in its two parts: the
// FORMATION of the protection verdicts before the publish
// ([verdicts.formBefore] — a read of the rules and every command against
// every rule, holding no snapshot), and a ROUND inside the publisher's
// snapshot — the two reads, and every command against the verdicts.
//
// The batches and their rules:
//
//   - maximal: three pushes of [MaxCommandsPerPush] ref creations against
//     rules of the shape a team's convention
//     writes (refs/heads/teamNN/release/**), each rule asked about each
//     command because none matches — the batch a burst of wide pushes makes;
//   - group: [MaxPushesPerRecord] pushes of one command each against the same
//     rules, the batch a burst of small pushes makes;
//   - worst: the costliest batch a repository's rules can make — every rule
//     of gitref's costliest shape (a run of four globs between two **s, three
//     matching every component of the ref and a fourth failing, so the run is
//     tried at every place and fails at its last part each time), asked about
//     as many refs of the widest shape that costs it most (255 bytes of
//     six-byte components) as one record carries ([filledRecord]), none
//     matching;
//   - worst-refused: the same, with the last rule in the rules' order matching
//     every ref with direct_push none, so every command is refused, and the
//     sentence naming the rule that refused it is formed too;
//   - deepest: as many creates as one record carries ([filledRecord]) of the
//     deepest refs ([deepestRef]), each making 118 directories — the most
//     seeks [RefusalRefConflict] asks of a round;
//   - buried: one push creating every directory of the deepest chain a ref
//     can sit under ([buriedChain]: refs/heads/a, refs/heads/a/a, … 118 of
//     them) at a repository holding [MaxTombstonesPerRepo] tombstones under
//     the chain's deepest directory, so every create's range under it spans
//     every tombstone; the first create lands and the rest are refused
//     ref_conflict, each having asked whether a live ref lies under it;
//   - fanned: the same 118 creates of the chain, then as many creates under
//     its deepest directory as the rest of a record carries
//     ([filledRecord]: 2 492 more), so every create of the chain holds every
//     name the record spells below it — the most gaps a record's ranges can
//     be cut into, and the most names a nested create's own walk passes;
//   - changes: one push moving the heads of [MaxCommandsPerPush] live changes
//     by a pusher already their author ([estate.seedLiveChanges]), then
//     [MaxPushesPerRecord] − 1 pushes of one create each that move none — the
//     round whose change book ([changeBook]) holds the most changes beside
//     the most entries it tries that reach none of them.
//
// And what each arm times:
//
//   - "round": a round as the committer runs it, with the verdicts the
//     formation formed — a first round and every later one alike, since a
//     round under the rules the formation read forms nothing;
//   - "formation": the formation alone, on the committer goroutine;
//   - "reformed": a round whose snapshot holds rules the formation did not
//     read — an activation changing this repository's protections between
//     the two — which forms every verdict inside the snapshot: the longest a
//     round can hold its reader.
//
// A round runs on the repository's ONE committer goroutine inside one read
// transaction, which holds a connection of the replicated estate's reader
// pool for its length, and again on every round a peer's record forces, up to
// the publisher's sixteen; doc.go's measured section records the figures, and
// [CommitBudget] is held against the worst.
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
		team = append(team, Protection{
			Pattern: fmt.Sprintf("refs/heads/team%02d/release/**", i), DirectPush: DirectNone,
		})
	}
	repository(typical, team)

	const worst, worstRefused = "0190f3c4-0000-7000-8000-00000000be02",
		"0190f3c4-0000-7000-8000-00000000be03"
	costliest := costliestRules(MaxProtectionsPerRepo)
	repository(worst, costliest)
	// THE MATCHING RULE SORTS LAST ('a' after '*'), so the sentence's walk
	// meets every costly rule before it, as the decision's walk did.
	refusing := append(slices.Clone(costliest[:MaxProtectionsPerRepo-1]),
		Protection{Pattern: "refs/heads/" + widestComponent + "/**", DirectPush: DirectNone})
	repository(worstRefused, refusing)

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
	deepest := filledRecord(b, typical, deepestRef)

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
	// THE FIRST CREATE TAKES THE CHAIN and every later one meets it — each
	// decided all the same, past every read the rule asks of it.
	firstLands := func(ref string) Refusal {
		if ref == chain[0] {
			return ""
		}
		return RefusalRefConflict
	}

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
		// formation is the batch's verdicts formed as the committer forms
		// them, every command's, each as the arm says — so a formation cut
		// short, or one a round would form again, is never what is measured.
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
				// EVERY COMMAND DECIDED AS THE ARM SAYS, so a round cut
				// short by a refusal nobody meant is never what is
				// measured.
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

// buriedChain is every directory of the deepest chain a tombstone of the
// buried arm sits under, shallowest first: refs/heads/a, then one "/a" more
// each, to the deepest that leaves [buriedLeaf] room in [MaxRefNameBytes] —
// 118 refs, each an ancestor of every tombstone.
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

// buriedLeaf is the spelling of the last component of each of the buried
// arm's tombstones, as the seeding statement prints its number.
const buriedLeaf = "/t000000"

// costliestRules are n distinct rules of gitref's costliest shape: a run of
// four globs between two **s, the first three matching every
// [widestComponent] at every place they are tried, a piece a byte, and the
// fourth failing on its last piece — a byte no component of [widestRef] holds,
// one for each rule. Sorted, as a repository's rules are stored.
func costliestRules(n int) []Protection {
	const last = "bcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_+"
	if n > len(last) {
		panic(fmt.Sprintf("costliestRules: %d rules asked for and %d distinct bytes to end them with", n, len(last)))
	}
	glob := "*" + strings.Repeat("a*", len(widestComponent))
	out := make([]Protection, n)
	for i := range n {
		out[i] = Protection{
			Pattern:    "refs/heads/**/" + strings.Repeat(glob+"/", 3) + glob + last[i:i+1] + "*/**",
			DirectPush: DirectPersons,
		}
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
