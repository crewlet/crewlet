package gitlet_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
)

// BenchmarkPushLogBytes is what ONE PUSH COSTS THE CODE LOG: the stream's
// growth per push over the committer's records, the log-side twin of
// BenchmarkPushHistoryBytes. Times a day's pushes times the trim's min_age, it
// is a company's share of the code log's ceiling beside the census's barriers
// (BarrierRecordBytes); a record names its bundle, never carries it. Each push
// is a person's through their token, moving a 40-byte branch one commit on,
// decided by the real committer onto the embedded broker. batch=1 is a lone
// push, its own record; batch=64 MaxPushesPerRecord at once to one repository.
// push=agent and push=box are a seat's engine-built and coding box's pushes,
// as extra-bytes/push over a person's otherwise identical entry
// (gitlet.EntryOf), added to a batch arm's figure; push=box, the widest, is
// what the code log's day prices a seat's push at ("Measured", package doc).
func BenchmarkPushLogBytes(b *testing.B) {
	gitlet.Quiet(b)
	const branch = "refs/heads/feature/push-log-measured-%03d"
	if got := len(fmt.Sprintf(branch, 0)); got != 40 {
		b.Fatalf("the branch is named in %d bytes, not 40", got)
	}
	// person is who makes a person's push as git hands one over: the token
	// bound to them, by its name, and the human seat they hold.
	person := gitlet.Principal{Kind: gitlet.PrincipalOperator, Seat: "ada",
		OperatorID: iam.MachineTokenName(uuid.NewSHA1(uuid.NameSpaceOID, []byte("ada's token")).String())}
	for _, batch := range []int{1, gitlet.MaxPushesPerRecord} {
		// rounds is how many batches a fresh log is measured over: enough
		// that one record's size is lost in the figure, and fixed, so the
		// figure holds at any -benchtime and never meets the ceiling.
		rounds := max(16, 64/batch)
		b.Run(fmt.Sprint("batch=", batch), func(b *testing.B) {
			var perPush, perRecord float64
			for b.Loop() {
				b.StopTimer()
				n := newNode(b)
				c, _ := committerOn(b, n, n.pub)
				repo := repoOn(b, n, "push-log")
				tips := make([]string, batch)
				round := func(r int) {
					var wg sync.WaitGroup
					for k := range batch {
						next := oidOf(fmt.Sprint("tip-", r, "-", k))
						p := packed(proposal(repo, update(fmt.Sprintf(branch, k), tips[k], next)), harnessAt)
						p.Principal, p.Introduced, p.IntroducedCount = person, []string{next}, 1
						wg.Go(func() {
							res, err := c.Propose(b.Context(), p)
							if err != nil {
								b.Errorf("push %d of round %d: %v", k, r, err)
								return
							}
							if cmds := res.Answer.Commands; len(cmds) != 1 || cmds[0].Result != gitlet.ResultOK {
								b.Errorf("push %d of round %d answered %+v, want its one command ok", k, r, res.Answer)
							}
						})
						tips[k] = next
					}
					wg.Wait()
				}
				for k := range tips {
					tips[k] = gitlet.ZeroOID
				}
				// THE BRANCHES EXIST BEFORE THE LOG IS MEASURED: every
				// measured push moves one on, as a person's does.
				round(0)
				before, err := n.log.Stats(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				for r := 1; r <= rounds; r++ {
					round(r)
				}
				b.StopTimer()
				after, err := n.log.Stats(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				records := after.Messages - before.Messages
				grew := float64(after.Bytes - before.Bytes)
				perPush = grew / float64(rounds*batch)
				perRecord = float64(rounds*batch) / float64(records)
				b.StartTimer()
			}
			b.ReportMetric(perPush, "bytes/push")
			b.ReportMetric(perRecord, "pushes/record")
		})
	}
	// THE SEATS' ARMS: the log's growth a seat's push takes beyond a person's.
	// rounds matches the lone push's arm; day is the company day the committer
	// stamps a seat's push with (harnessAt on UTC, as committerOn's).
	const rounds = 16
	day := period.At(period.Day, harnessAt, time.UTC).Label
	// madeFor is whom who's push was made for: a seat's, the human seat that
	// assigned its run's work (ADR-0109), as the code log's day prices it; a
	// person's, nobody.
	madeFor := func(who gitlet.Principal) types.WrittenFor {
		if who.Kind != gitlet.PrincipalAgent {
			return types.ForNobody()
		}
		return types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"})
	}
	// grew is the log's growth a push over rounds one-push records by who via
	// via, each moving ref on — ON A LOG OF ITS OWN, so every principal's
	// records sit at the same positions and carry numbers as wide.
	grew := func(b *testing.B, who gitlet.Principal, via gitlet.PushVia, ref string) float64 {
		n := newNode(b)
		repo := repoOn(b, n, "push-log")
		tip := gitlet.ZeroOID
		push := func(r int) {
			id, next := uuid.Must(uuid.NewV7()).String(), oidOf(fmt.Sprint(ref, "-", r))
			if who.Kind == gitlet.PrincipalAgent {
				who.Run.TurnID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("turn-", r))).String()
			}
			// THE ENTRY AS THE COMMITTER FORMS IT: a seat's carries its day
			// and whom its push was made for, and a person's neither.
			n.push(repo, gitlet.EntryOf(gitlet.Proposal{PushID: id, RepoID: repo, Principal: who,
				WrittenFor: madeFor(who), Via: via, Node: n.id, Commands: []gitlet.Command{set(ref, tip, next)},
				Pack: suitePack(id), Introduced: []string{next}, IntroducedCount: 1,
				ReceivedAt: harnessAt}, day))
			tip = next
		}
		// THE BRANCH EXISTS BEFORE THE LOG IS MEASURED, as in the batch arms.
		push(0)
		before, err := n.log.Stats(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		for r := 1; r <= rounds; r++ {
			push(r)
		}
		after, err := n.log.Stats(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		if records := after.Messages - before.Messages; records != rounds {
			b.Fatalf("%d pushes made %d records, want one each", rounds, records)
		}
		return float64(after.Bytes-before.Bytes) / rounds
	}
	// seat is the principal both seat arms push as, its run's fields at their
	// usual sizes (a turn id per push, set in grew).
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "docs-writer",
		Run: gitlet.RunProvenance{WorkBackend: "native", WorkKey: "ENG-1234", Model: "claude-sonnet-4-5"}}
	// box is the seat's coding box: its run also names the executor (launch id
	// and harness) and the work item's id and project, which the box's
	// admission stamps and an engine-built write never carries
	// (gitlet.Proposal.Validate requires a git-received seat push to name its
	// executor).
	box := seat
	box.Run.RunID, box.Run.Harness = "0192f00d-0000-7000-8000-00000000c0de", "claude-code"
	box.Run.WorkID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("ENG-1234")).String()
	box.Run.WorkProject = "ENG"
	for _, arm := range []struct {
		name string
		who  gitlet.Principal
		via  gitlet.PushVia
	}{
		{"push=agent", seat, gitlet.ViaEngine},
		{"push=box", box, gitlet.ViaGit},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var extra float64
			for b.Loop() {
				b.StopTimer()
				persons := grew(b, person, gitlet.ViaGit, fmt.Sprintf(branch, 0))
				extra = grew(b, arm.who, arm.via, fmt.Sprintf(branch, 1)) - persons
				b.StartTimer()
			}
			b.ReportMetric(extra, "extra-bytes/push")
		})
	}
}
