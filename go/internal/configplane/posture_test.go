package configplane

import (
	"testing"
	"time"
)

// TestDecidePosture is the whole rule as a table. Every row names the
// situation it protects against, because each branch here is a decision
// somebody got backwards once.
func TestDecidePosture(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		view FleetView
		want Posture
	}{
		{
			name: "caught up",
			view: FleetView{TargetEpoch: 7, AppliedEpoch: 7, SelfStatus: StatusOK},
			want: PostureServe,
		},
		{
			name: "ahead of the pointer still serves",
			// A node that applied an epoch the pointer has not caught
			// up to is not behind. Reading this as lag would shed on
			// the winner of a race it just won.
			view: FleetView{TargetEpoch: 6, AppliedEpoch: 7, SelfStatus: StatusOK},
			want: PostureServe,
		},
		{
			name: "ordinary propagation never sheds",
			// THE rule. Every successful rollout produces lag; shedding
			// on it makes the fastest node cause a fleet-wide outage.
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, TicksBehind: 1, PeersOK: 3},
			want: PostureWait,
		},
		{
			name: "still propagation one tick before the grace expires",
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, TicksBehind: LagGraceTicks - 1, PeersOK: 3},
			want: PostureWait,
		},
		{
			name: "confirmed by time with healthy peers sheds",
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, TicksBehind: LagGraceTicks, PeersOK: 2},
			want: PostureShed,
		},
		{
			name: "confirmed by own failure sheds immediately",
			// A node that TRIED and failed does not need to wait out the
			// grace window: it already has the evidence the window exists
			// to gather.
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, SelfStatus: StatusError, PeersOK: 2},
			want: PostureShed,
		},
		{
			name: "exhausted retries with healthy peers is stuck",
			view: FleetView{
				TargetEpoch: 8, AppliedEpoch: 7, SelfStatus: StatusError,
				Attempts: MaxApplyAttempts, PeersOK: 1,
			},
			want: PostureStuck,
		},
		{
			name: "nobody managed the epoch is isolated, not shed",
			// With no healthy peer, shedding is not shedding — it is
			// stopping. Every node in a fleet that cannot apply a
			// revision reaches this at the same moment.
			view: FleetView{
				TargetEpoch: 8, AppliedEpoch: 7, SelfStatus: StatusError,
				PeersReported: 2, PeersOK: 0,
			},
			want: PostureIsolated,
		},
		{
			name: "isolated outranks exhausted retries",
			// The ordering that took a whole company dark ~45s after a
			// bad activation when it was the other way round.
			view: FleetView{
				TargetEpoch: 8, AppliedEpoch: 7, SelfStatus: StatusError,
				Attempts: MaxApplyAttempts, PeersReported: 3, PeersOK: 0,
			},
			want: PostureIsolated,
		},
		{
			name: "a lone node reaches isolated on its own failure",
			// No peer will ever report anything, so its own failure is
			// the only evidence there will be.
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, SelfStatus: StatusError},
			want: PostureIsolated,
		},
		{
			name: "degraded counts as tried-and-failed",
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, SelfStatus: StatusDegraded},
			want: PostureIsolated,
		},
		{
			name: "silence is not evidence",
			// Behind longer than propagation explains, but this node
			// never attempted the epoch and no peer reported either way.
			view: FleetView{TargetEpoch: 8, AppliedEpoch: 7, TicksBehind: 10},
			want: PostureWait,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DecidePosture(tc.view); got != tc.want {
				t.Errorf("DecidePosture(%+v) = %s, want %s", tc.view, got, tc.want)
			}
		})
	}
}

// TestLagAloneNeverSheds states the central rule directly rather than
// leaving it implicit in the table: no amount of pure lag, with any healthy
// peer count, may produce a shed while the node has not tried and failed.
func TestLagAloneNeverSheds(t *testing.T) {
	t.Parallel()
	for ticks := range LagGraceTicks {
		for peers := range 5 {
			v := FleetView{TargetEpoch: 2, AppliedEpoch: 1, TicksBehind: ticks, PeersOK: peers}
			if got := DecidePosture(v); got != PostureWait {
				t.Errorf("ticks=%d peers=%d gave %s, want wait", ticks, peers, got)
			}
		}
	}
}

// TestReadinessKeepsLaggingAndIsolatedNodesReady pins the split: failing
// readiness on ordinary lag makes the fastest node cause a fleet outage, and
// an isolated node is serving a rolled-back config correctly.
func TestReadinessKeepsLaggingAndIsolatedNodesReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		p     Posture
		ready bool
	}{
		{PostureServe, true},
		{PostureWait, true},
		{PostureIsolated, true},
		{PostureShed, false},
		{PostureStuck, false},
	} {
		if got := tc.p.Ready(); got != tc.ready {
			t.Errorf("%s.Ready() = %v, want %v", tc.p, got, tc.ready)
		}
		if got := tc.p.ServesTraffic(); got != tc.ready {
			t.Errorf("%s.ServesTraffic() = %v, want %v", tc.p, got, tc.ready)
		}
	}
}

// TestDegradedIsNeverConverged guards the three-valued status: a node whose
// apply failed after mutating a restart-required subsystem is running, but
// it is not serving the epoch it reported.
func TestDegradedIsNeverConverged(t *testing.T) {
	t.Parallel()
	if StatusDegraded.Converged() {
		t.Error("degraded must never count as converged")
	}
	if StatusError.Converged() {
		t.Error("error must never count as converged")
	}
	if !StatusOK.Converged() {
		t.Error("ok must count as converged")
	}
}

func TestReconcileDelayStaysInJitterBand(t *testing.T) {
	t.Parallel()
	spread := time.Duration(float64(ReconcileInterval) * ReconcileJitter)
	lo, hi := ReconcileInterval-spread, ReconcileInterval+spread
	for range 200 {
		d := ReconcileDelay()
		if d < lo || d > hi {
			t.Fatalf("ReconcileDelay() = %v, want within [%v, %v]", d, lo, hi)
		}
	}
}
