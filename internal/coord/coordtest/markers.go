package coordtest

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the removal markers ----------------------------------------------- //

// sweepAll sweeps every marker the backend holds: a cutoff an hour in the
// FUTURE is later than any marker a case can have written, so nothing is spared
// for being young. A horizon of zero is the strongest form of the contract's
// promise — if sweeping the newest marker changes no answer, sweeping one
// [coord.MarkerRetention] old cannot.
func (h *fleetHarness) sweepAll() int64 {
	h.t.Helper()
	n, err := h.f.SweepMarkers(h.ctx, time.Now().Add(time.Hour))
	if err != nil {
		h.t.Fatalf("SweepMarkers: %v", err)
	}
	return n
}

func (h *fleetHarness) holdOwners() []string {
	h.t.Helper()
	holds, err := h.f.Holds(h.ctx)
	if err != nil {
		h.t.Fatalf("Holds: %v", err)
	}
	owners := make([]string, 0, len(holds))
	for _, hold := range holds {
		owners = append(owners, hold.Owner)
	}
	slices.Sort(owners)
	return owners
}

func (h *fleetHarness) putHold(owner string) {
	h.t.Helper()
	err := h.f.PutHold(h.ctx, coord.TrimHold{
		Owner: owner, At: h.now(),
		Streams: map[string]coord.Position{"LOG": {Stream: "LOG", Generation: 1, Seq: 5}},
	})
	if err != nil {
		h.t.Fatalf("PutHold(%s): %v", owner, err)
	}
}

var markerCases = []fleetCase{{
	// THE CONTRACT, held on every family a KV backend keeps markers in. A
	// removal leaves a marker that a bucket nobody ages keeps for ever, and
	// the sweep that removes them is only safe because nothing reads one as
	// an answer: a listing asks the leader about a marker its pass met, and a
	// create asks before it conditions on one. So after a sweep of EVERY
	// marker, each family answers exactly what it answered before — and a
	// key whose marker went is created again as if it had never existed,
	// which is the case a create that conditioned on the marker's revision
	// got wrong.
	//
	// The same failure INSIDE one create — a marker swept between the
	// create's read of it and its write over it — opens in a window this
	// contract cannot reach, and a stress loop around it measured no hit in
	// four hundred rounds. It is staged at the seam instead:
	// internal/coord/kv's
	// TestACreateOverAMarkerTheSweepRemovesMidWayStillCreates.
	name: "sweeping every marker changes no answer",
	fn: func(h *fleetHarness) {
		// Runs: one removed, one live, one removed and created again.
		h.createRun("turn-gone", `{"status":"done"}`)
		gone, _ := h.run("turn-gone")
		h.createRun("turn-live", `{"status":"running"}`)
		h.createRun("turn-again", `{"status":"done"}`)
		first, _ := h.run("turn-again")
		for _, r := range []coord.Record{{Key: "turn-gone", Version: gone.Version}, {Key: "turn-again", Version: first.Version}} {
			if ok, err := h.f.DeleteSandboxRun(h.ctx, r.Key, r.Version); err != nil || !ok {
				h.t.Fatalf("DeleteSandboxRun(%s) = (%v, %v)", r.Key, ok, err)
			}
		}
		h.createRun("turn-again", `{"status":"running"}`)
		// Mailboxes, secrets, integration statuses and trim holds: one
		// removed and one kept in each.
		for _, handle := range []string{"cto", "ceo"} {
			h.createMailbox(coord.MailboxRecord{Handle: handle})
		}
		cto, _ := h.mailbox("cto")
		if !h.deleteMailbox("cto", cto.Version) {
			h.t.Fatal("the cto's mailbox record could not be deleted at the version just read")
		}
		h.putSecret("GONE_TOKEN", "sealed-1", "k1")
		h.putSecret("KEPT_TOKEN", "sealed-2", "k1")
		if removed, err := h.f.DeleteSecret(h.ctx, "GONE_TOKEN"); err != nil || !removed {
			h.t.Fatalf("DeleteSecret = (%v, %v)", removed, err)
		}
		h.putIntegration("slack", `{"phase":"ready"}`)
		h.putIntegration("jira", `{"phase":"ready"}`)
		if err := h.f.DeleteIntegrationStatus(h.ctx, "slack"); err != nil {
			h.t.Fatalf("DeleteIntegrationStatus: %v", err)
		}
		h.putHold("node-a/backup")
		h.putHold("node-b/snapshot")
		if err := h.f.ReleaseHold(h.ctx, "node-a/backup"); err != nil {
			h.t.Fatalf("ReleaseHold: %v", err)
		}

		h.sweepAll()
		if n := h.sweepAll(); n != 0 {
			h.t.Errorf("a second sweep removed %d markers, want 0: the first "+
				"left markers behind that a cutoff after every one of them "+
				"should have taken", n)
		}

		runs, err := h.f.SandboxRuns(h.ctx)
		if err != nil {
			h.t.Fatalf("SandboxRuns: %v", err)
		}
		var keys []string
		for _, r := range runs {
			keys = append(keys, r.Key)
		}
		if want := []string{"turn-again", "turn-live"}; !slices.Equal(keys, want) {
			h.t.Errorf("runs after the sweep = %v, want %v", keys, want)
		}
		if _, found := h.run("turn-gone"); found {
			h.t.Error("a run removed before the sweep reads as present after it")
		}
		if h.updateRun("turn-again", `{"status":"stale"}`, first.Version) {
			h.t.Error("a version from the removed incarnation overwrote its successor once the marker between them was swept")
		}
		if !h.createRun("turn-gone", `{"status":"running"}`) {
			h.t.Error("a run whose marker was swept could not be created again: the store reported a removed key as held")
		}
		if _, found := h.mailbox("cto"); found {
			h.t.Error("a mailbox record removed before the sweep reads as present after it")
		}
		if _, created := h.createMailbox(coord.MailboxRecord{Handle: "cto"}); !created {
			h.t.Error("a mailbox record whose marker was swept could not be created again")
		}
		if got := h.secretNames(); !slices.Equal(got, []string{"KEPT_TOKEN"}) {
			h.t.Errorf("secrets after the sweep = %v, want [KEPT_TOKEN]", got)
		}
		if _, found := h.secret("GONE_TOKEN"); found {
			h.t.Error("a secret removed before the sweep reads as present after it")
		}
		if got := h.integrationNames(); !slices.Equal(got, []string{"jira"}) {
			h.t.Errorf("integration statuses after the sweep = %v, want [jira]", got)
		}
		if got := h.holdOwners(); !slices.Equal(got, []string{"node-b/snapshot"}) {
			h.t.Errorf("trim holds after the sweep = %v, want [node-b/snapshot]", got)
		}
	},
}}
