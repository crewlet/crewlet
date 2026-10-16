package code_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
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
// Each push is a person's push of one commit: one command moving a branch
// named in 40 bytes on from its tip, its pack bundle and one introduced id,
// decided and recorded by the committer as in production, through the
// harness's real publisher and embedded broker. The branches are created
// before the stream is measured, and each iteration is a fresh log of a fixed
// number of rounds, so the figure is the same at any -benchtime and no
// iteration meets the harness's ceiling. doc.go's measured section records
// both arms.
func BenchmarkPushLogBytes(b *testing.B) {
	code.Quiet(b)
	const branch = "refs/heads/feature/push-log-measured-%03d"
	if got := len(fmt.Sprintf(branch, 0)); got != 40 {
		b.Fatalf("the branch is named in %d bytes, not 40", got)
	}
	for _, batch := range []int{1, code.MaxPushesPerRecord} {
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
						p.Introduced, p.IntroducedCount = []string{next}, 1
						wg.Go(func() {
							res, err := c.Propose(b.Context(), p)
							if err != nil {
								b.Errorf("push %d of round %d: %v", k, r, err)
								return
							}
							if cmds := res.Answer.Commands; len(cmds) != 1 || cmds[0].Result != code.ResultOK {
								b.Errorf("push %d of round %d answered %+v, want its one command ok", k, r, res.Answer)
							}
						})
						tips[k] = next
					}
					wg.Wait()
				}
				for k := range tips {
					tips[k] = code.ZeroOID
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
}
