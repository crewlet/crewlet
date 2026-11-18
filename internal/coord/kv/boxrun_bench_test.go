package kv

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkABoxRequestOnItsRun measures what one request of a coding box costs
// the coordination store: three members at three replicas, from a member NOT
// leading the runs bucket, as two of three are for any bucket. Its arms: read,
// host.Runs' admission read of every /box/git/ request (PendingStore.Get);
// raise, a higher code floor each time, so every one writes (RaiseCodeFloor);
// append, a call that cost the engine nothing, so only the log is written
// (AppendBridgeCall); append-paid, one that moved the job's spend, so the record
// is read and written too; begin, the entry every bridged call waits on before
// it runs (sandbox.AppendBegin; mcpbridge's "A call is on its run's record
// before it runs"), withdrawn after timing; record, an entry completed in one
// write (sandbox.AppendRecord). Reports ms-p50, ms-p99, row-bytes, log-bytes;
// figures: docs/concepts/gitlet.md, "What a box's request costs its run's record".
func BenchmarkABoxRequestOnItsRun(b *testing.B) {
	ctx := context.Background()
	fleet := boxRunFleet(b)
	store := sandbox.NewCoordStore(fleet)
	turn, launch := seedBoxRun(b, store)

	row := func() int {
		record, found, err := fleet.SandboxRun(ctx, turn)
		if err != nil || !found {
			b.Fatalf("read the run's record: (%v, %v)", found, err)
		}
		return len(record.Value)
	}
	logBytes := func() int {
		record, found, err := fleet.SandboxRunPart(ctx, turn, coord.RunLog)
		if err != nil || !found {
			b.Fatalf("read the run's log: (%v, %v)", found, err)
		}
		return len(record.Value)
	}

	// measure times op, with before and after — either nil — run around each
	// operation and kept out of its time.
	measured := func(b *testing.B, before, op, after func(i int) error) {
		took := make([]time.Duration, 0, b.N)
		b.ResetTimer()
		for i := range b.N {
			if before != nil {
				if err := before(i); err != nil {
					b.Fatal(err)
				}
			}
			start := time.Now()
			if err := op(i); err != nil {
				b.Fatal(err)
			}
			took = append(took, time.Since(start))
			if after != nil {
				if err := after(i); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StopTimer()
		slices.Sort(took)
		at := func(q float64) time.Duration { return took[int(q*float64(len(took)-1))] }
		b.ReportMetric(float64(at(0.5).Microseconds())/1000, "ms-p50")
		b.ReportMetric(float64(at(0.99).Microseconds())/1000, "ms-p99")
		b.ReportMetric(float64(row()), "row-bytes")
		b.ReportMetric(float64(logBytes()), "log-bytes")
	}
	measure := func(b *testing.B, op func(i int) error) { measured(b, nil, op, nil) }

	b.Run("read", func(b *testing.B) {
		measure(b, func(int) error {
			run, found, err := store.Get(ctx, turn)
			switch {
			case err != nil:
				return fmt.Errorf("read the run: %w", err)
			case !found || run.LaunchID != launch:
				return fmt.Errorf("the run read as (%v, launch %q), want its launch %q", found, run.LaunchID, launch)
			}
			return nil
		})
	})
	floor := 1
	b.Run("raise", func(b *testing.B) {
		measure(b, func(int) error {
			floor++
			next := fmt.Sprintf("CREWLET_CODE_LOG@1:%d", floor)
			wrote, err := store.RaiseCodeFloor(ctx, turn, launch, func(string) (string, bool) { return next, true })
			switch {
			case err != nil:
				return fmt.Errorf("raise the floor: %w", err)
			case !wrote:
				return fmt.Errorf("the raise to %s wrote nothing", next)
			}
			return nil
		})
	})
	output := strings.Repeat("o", 2<<10)
	appendCall := func(i int, spent sandbox.EngineSpend) error {
		recorded, err := store.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
			Call: sandbox.BridgeCall{Name: "read_page",
				Args: fmt.Sprintf(`{"id":%d,"q":"%s"}`, i, strings.Repeat("q", 240)), Output: output},
			Spent: spent})
		switch {
		case err != nil:
			return fmt.Errorf("append a call: %w", err)
		case !recorded:
			return fmt.Errorf("the append of call %d recorded nothing", i)
		}
		return nil
	}
	b.Run("append", func(b *testing.B) {
		measure(b, func(i int) error { return appendCall(i, sandbox.EngineSpend{}) })
	})
	// THE SPEND GROWS ON EVERY CALL, from past anything an earlier run of
	// the arm wrote, so every append moves the record's total and writes it.
	paid := 0
	b.Run("append-paid", func(b *testing.B) {
		measure(b, func(i int) error {
			paid++
			return appendCall(i, sandbox.EngineSpend{Workers: paid, WorkerInput: 1000 * paid,
				WorkerOutput: 200 * paid})
		})
	})
	// A CALL'S STEPS, each at a place and an instant no earlier arm's call
	// took, so every one is a call of its own.
	started := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	step := func(name string, at sandbox.AppendStep, i int) error {
		call := sandbox.BridgeCall{Name: "read_page", Seq: sandbox.MaxBridgeCalls + 1 + i,
			StartedAt: started.Add(time.Duration(i) * time.Millisecond)}
		if at == sandbox.AppendRecord {
			call.Args, call.Output = fmt.Sprintf(`{"id":%d,"q":"%s"}`, i, strings.Repeat("q", 240)), output
		}
		recorded, err := store.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Step: at, Launch: launch, Call: call})
		switch {
		case err != nil:
			return fmt.Errorf("%s call %d: %w", name, i, err)
		case !recorded:
			return fmt.Errorf("the %s of call %d took nothing", name, i)
		}
		return nil
	}
	// EVERY STEP'S CALL IS A NEW ONE, across every run the framework makes of
	// an arm and across both arms.
	calls, at := 0, 0
	next := func(int) error { calls++; at = calls; return nil }
	b.Run("begin", func(b *testing.B) {
		measured(b, next, func(int) error { return step("begin", sandbox.AppendBegin, at) },
			func(int) error { return step("withdraw", sandbox.AppendWithdraw, at) })
	})
	b.Run("record", func(b *testing.B) {
		measured(b, func(i int) error {
			_ = next(i)
			return step("begin", sandbox.AppendBegin, at)
		}, func(int) error { return step("record", sandbox.AppendRecord, at) }, nil)
	})
}

// boxRunFleet is the fleet's coordination store on a three-member cluster at
// three replicas, opened through a member that does not lead the runs bucket.
func boxRunFleet(b *testing.B) *FleetStore {
	b.Helper()
	c := jetstreamtest.StartCluster(b, 3, js.Config{})
	client := func(i int) jetstream.JetStream {
		nc, err := c.Servers[i].Conn()
		if err != nil {
			b.Fatalf("member %d: connect: %v", i, err)
		}
		b.Cleanup(nc.Close)
		client, err := jsapi.Embedded().Client(nc)
		if err != nil {
			b.Fatalf("member %d: client: %v", i, err)
		}
		return client
	}
	cfg := FleetConfig{
		Reach:         InProcess,
		BucketPrefix:  fmt.Sprintf("f%d", bucketSeq.Add(1)),
		MaxRateWindow: time.Minute, MaxClaimTTL: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, RebaseRetention: 10 * time.Minute,
		BudgetRetention: time.Hour, StatusFreshness: 10 * time.Minute,
		CustodyRetention: 10 * time.Minute, Replicas: 3, Clustered: true,
	}
	first, err := OpenFleet(b.Context(), client(0), cfg)
	if err != nil {
		b.Fatalf("OpenFleet: %v", err)
	}
	stream, err := first.js.Stream(b.Context(), bucketStream(first.runs))
	if err != nil {
		b.Fatalf("the runs bucket's stream: %v", err)
	}
	info, err := stream.Info(b.Context())
	if err != nil || info.Cluster == nil {
		b.Fatalf("the runs bucket's placement: (%v, %v)", info, err)
	}
	for i, member := range c.Configs {
		if member.ServerName == info.Cluster.Leader {
			continue
		}
		fleet, err := OpenFleet(b.Context(), client(i), cfg)
		if err != nil {
			b.Fatalf("member %d: OpenFleet: %v", i, err)
		}
		return fleet
	}
	b.Fatalf("every member leads the runs bucket: %+v", info.Cluster)
	return nil
}

// seedBoxRun launches the benchmark's run, parks its turn and fills its bridge
// log, answering its turn and its launch. THE RUN IS A REALISTIC ONE, mid-job:
// one repository, a 32 KiB frozen context, and the log, kept beside the record
// ([sandbox.BridgeLog]), full at sandbox.MaxBridgeCalls calls of 256 bytes of
// arguments and 2 KiB of output each.
func seedBoxRun(b *testing.B, store *sandbox.CoordStore) (turn, launch string) {
	b.Helper()
	ctx := b.Context()
	text := func(r *rand.Rand, n int) string {
		const letters = "abcdefghijklmnopqrstuvwxyz      "
		out := make([]byte, n)
		for i := range out {
			out[i] = letters[r.IntN(len(letters))]
		}
		return string(out)
	}
	r := rand.New(rand.NewPCG(1, 2))
	turn = uuid.NewString()
	opened, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turn, AgentHandle: "swe", AgentID: uuid.NewString(), Role: "engineer",
		Harness: "claude-code", TaskDescription: text(r, 2<<10),
		Repos: []sandbox.RunRepo{{Repo: "web", RepoID: uuid.NewString(),
			Base: strings.Repeat("a", 40), Branch: "seats/swe/web-1", Access: sandbox.RepoWrite}},
		CodeFloor: "CREWLET_CODE_LOG@1:1",
	}, sandbox.Fence{})
	if err != nil {
		b.Fatalf("BeginLaunch: %v", err)
	}
	// THE PARKED TURN'S STATE, as the executor's launch writes it: the
	// turn's bookkeeping and the context it froze at its start, 32 KiB of
	// it. No conversation: the executor is its harness's own run, and
	// nothing a model said is the engine's to keep.
	state, err := json.Marshal(map[string]any{
		"version": 1, "iteration": 1, "task_description": text(r, 2<<10),
		"frozen_context": map[string]string{"thread_context": text(r, 32<<10)},
	})
	if err != nil {
		b.Fatal(err)
	}
	if ok, err := store.MarkSuspended(ctx, turn, sandbox.Suspension{
		State: state, Iteration: 1,
	}); err != nil || !ok {
		b.Fatalf("MarkSuspended = (%v, %v)", ok, err)
	}
	for i := range sandbox.MaxBridgeCalls {
		if ok, err := store.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{
			Launch: opened.LaunchID,
			Call: sandbox.BridgeCall{Name: "read_page",
				Args:   fmt.Sprintf(`{"id":%d,"q":"%s"}`, i, text(r, 240)),
				Output: text(r, 2<<10)},
		}); err != nil || !ok {
			b.Fatalf("AppendBridgeCall %d = (%v, %v)", i, ok, err)
		}
	}
	return turn, opened.LaunchID
}
