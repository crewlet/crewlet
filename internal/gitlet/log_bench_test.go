package gitlet_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
)

// BenchmarkPushLogBytes is what ONE PUSH COSTS THE CODE LOG: the stream's
// growth over the records the committer forms, divided by the pushes they
// carry — the log-side twin of BenchmarkPushHistoryBytes, which is what a push
// leaves in every data node's rows.
//
// The log holds the trim's window of records (its min_age, seven days unset),
// never the history, so this figure times a day's pushes times that window is
// what a company's pushes take of the code log's ceiling — beside the
// barriers its git clients' reads append (BarrierRecordBytes), which the
// census prices. A record names its pack bundle and never carries it, so the
// bundle's size is not in the figure at all.
//
//   - batch=1: a lone push, which is a record of its own — an idle repository,
//     and every push of a company whose pushes do not share a repository;
//   - batch=64: MaxPushesPerRecord pushes to one repository at once, as many a
//     record as group commit puts together with no linger.
//
// Each push is a person's push of one commit through the token bound to them
// — its principal naming the token, `pat:` and its id, beside the human seat
// they hold, as a person's push names it — one command moving a branch named
// in 40 bytes on from its tip, its pack bundle and one introduced id, decided
// and recorded by the committer as in production, through the harness's real
// publisher and embedded broker. And beside them:
//
//   - push=agent: what a SEAT's push of that commit takes of the log beyond a
//     person's (extra-bytes/push) — built by the engine, its principal naming
//     the seat and the run behind it at each field's usual size, a turn id (a
//     UUID), a tracker's work item (`native`, `ENG-1234`) and a model id of a
//     provider's usual length, and its way `engine`. The committer forms a
//     record's entry from its proposal (Proposal.entryWith): it copies the
//     principal and the way as the proposer stamped them, stamps a SEAT'S
//     entry alone with the company day its quota counted the push against
//     (PushEntry.Day), and decides the rest alike for both — so the
//     principal, the way and the day are all a seat's entry differs by. Each
//     entry here is formed by that same function (gitlet.EntryOf), a seat's on
//     the day committerOn's committer would count it against, and they are
//     measured as the difference between a seat's and a person's otherwise
//     identical entries over records formed here, each alone in its record
//     and published through the same publisher onto the same broker; what a
//     seat's push takes of the log, alone or in a burst, is the batch arms'
//     figure plus it.
//   - push=box: the same for that seat's CODING BOX's push of the commit,
//     received by git: its principal names the seat's run as the engine's
//     write does and beside it the executor that pushed — the launch's id,
//     a UUID — the harness the box ran (`claude-code`) and the
//     identity and the project of the work item its run was launched for (a
//     native task's UUID and its project's key, RunProvenance.WorkID and
//     .WorkProject, which the box's admission stamps and no engine-built
//     write carries), and its way is `git`
//     (gitlet.Proposal.Validate takes a seat's push git received only naming
//     its executor). It is the most a seat's push takes of the log, which
//     the code log's day prices every seat's push at.
//
// The branches are created before the stream is measured, and each iteration
// is a fresh log of a fixed number of rounds, so the figure is the same at any
// -benchtime and no iteration meets the harness's ceiling. doc.go's measured
// section records the arms.
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
		// rounds is how many batches a log is measured over: enough records
		// that one record's size is lost in the figure.
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
	// THE SEATS' ARMS: each the log's growth a push of a seat's, built as its
	// way names, beyond a person's.
	//
	// rounds is the records each principal's pushes are measured over, as many
	// as the lone push's arm takes; day the company day a seat's push is counted
	// against, as the committer stamps it: harnessAt's on UTC's calendar,
	// committerOn's.
	const rounds = 16
	day := period.At(period.Day, harnessAt, time.UTC).Label
	// grew is the log's growth a push over rounds records of one push each by
	// who, built as via names, each moving ref on from the commit the record
	// before left — ON A LOG OF ITS OWN, so every principal's records sit at
	// the same positions and every number a record carries beside its entry
	// is as wide for each.
	grew := func(b *testing.B, who gitlet.Principal, via gitlet.PushVia, ref string) float64 {
		n := newNode(b)
		repo := repoOn(b, n, "push-log")
		tip := gitlet.ZeroOID
		push := func(r int) {
			id, next := uuid.Must(uuid.NewV7()).String(), oidOf(fmt.Sprint(ref, "-", r))
			if who.Kind == gitlet.PrincipalAgent {
				who.Run.TurnID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("turn-", r))).String()
			}
			// THE ENTRY AS THE COMMITTER FORMS IT: a seat's carries its day,
			// and a person's none.
			n.push(repo, gitlet.EntryOf(gitlet.Proposal{PushID: id, RepoID: repo, Principal: who,
				Via: via, Node: n.id, Commands: []gitlet.Command{set(ref, tip, next)},
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
	// seat is the seat's principal both seat arms push as: the run behind a
	// write at each field's usual size, a turn id per push (a UUID), a
	// tracker's work item and a model id of a provider's usual length.
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "docs-writer",
		Run: gitlet.RunProvenance{WorkBackend: "native", WorkKey: "ENG-1234", Model: "claude-sonnet-4-5"}}
	// box is the same seat's coding box: its run names the executor too —
	// the launch's id, a UUID, and its harness — and the work item's
	// identity and project, a native task's UUID and its project's key,
	// which the box's admission stamps from its run's row and an
	// engine-built write never carries.
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
