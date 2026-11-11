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
// the coordination store, on an embedded three-member broker whose buckets are
// replicated three times, from a member that does NOT lead the runs bucket —
// where two of a fleet's three members stand for any one bucket:
//
//   - read: the read of the box's run every /box/git/ request makes before
//     anything else (internal/gitlet/host's admission, through the engine's
//     host.Runs over sandbox.PendingStore.Get);
//   - raise: the raise of the run's code floor every ref answer past it and
//     every applied push makes before it answers (host.Runs.RaiseFloor, through
//     sandbox.PendingStore.RaiseCodeFloor), each operation a higher floor, so
//     every one writes;
//   - append: one bridged call the box makes, appended to the run's log
//     (sandbox.PendingStore.AppendBridgeCall) — where the log's size is paid
//     now that the record does not carry it — that cost the engine nothing
//     (a call answered by a builtin), so the record is not written;
//   - append-paid: the same call where it moved what the job's bridged calls
//     cost (a delegated worker's model call, say), so the append also reads
//     and writes the run's record with the new total — the path every call
//     that costs the engine anything takes.
//
// THE RUN IS A REALISTIC ONE, mid-job: launched with one repository, its turn
// parked (its bookkeeping and a 32 KiB frozen context), and the bridge's log
// full at sandbox.MaxBridgeCalls calls of 256 bytes of arguments and 2 KiB of
// output each, kept beside the record ([sandbox.BridgeLog]). Reported beside
// the time are its spread (ms-p50, ms-p99), the bytes of the run's own record
// (row-bytes) — what each read moves, and what each raise reads and writes
// back — and of its log (log-bytes), what each append reads and writes back.
// A request that fails fails the benchmark.
//
// Measured on 2026-11-03, thirty operations an arm and one count, on two
// E-cores of an Intel Core Ultra 7 265U, with the log beside the record: see
// docs/concepts/gitlet.md ("What a box's request costs its run's record")
// for the figures, both layouts', and for the size of the sealed set of
// secrets kept beside the record, which no arm moves. With the log on the
// record, as main's run record kept it (measured 2026-11-01 on the same
// machine), the record was 516 173 bytes, nearly all of it the log, a read
// took 5.8 ms at the median (7.9 at the 99th percentile) and a raise 11.1 ms
// (26.0): every request of a box moved the run's whole tool log, not the
// request.
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

	measure := func(b *testing.B, op func(i int) error) {
		took := make([]time.Duration, 0, b.N)
		b.ResetTimer()
		for i := range b.N {
			start := time.Now()
			if err := op(i); err != nil {
				b.Fatal(err)
			}
			took = append(took, time.Since(start))
		}
		b.StopTimer()
		slices.Sort(took)
		at := func(q float64) time.Duration { return took[int(q*float64(len(took)-1))] }
		b.ReportMetric(float64(at(0.5).Microseconds())/1000, "ms-p50")
		b.ReportMetric(float64(at(0.99).Microseconds())/1000, "ms-p99")
		b.ReportMetric(float64(row()), "row-bytes")
		b.ReportMetric(float64(logBytes()), "log-bytes")
	}

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
// log, answering its turn and its launch.
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
