package coordtest

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- model retries and device sign-ins ---------------------------------- //

func (h *fleetHarness) retry(entry string, mode coord.ModelRetryMode, at time.Time, by string) coord.ModelRetry {
	h.t.Helper()
	r, err := h.f.PutModelRetry(h.ctx, coord.ModelRetry{
		Key: entry, Mode: mode, At: at, By: by, ByKind: iam.ActorHuman, OperatorID: sessionOf(by),
	})
	if err != nil {
		h.t.Fatalf("PutModelRetry(%s): %v", entry, err)
	}
	return r
}

func (h *fleetHarness) signIn(entry, node string) coord.DeviceSignIn {
	return coord.DeviceSignIn{
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(entry+node)), Entry: entry, Node: node,
		Status: coord.DeviceStarting, StartedAt: h.now(), ExpiresAt: h.now().Add(15 * time.Minute),
		By: "jane-doe", ByKind: iam.ActorHuman, OperatorID: sessionOf("jane-doe"),
	}
}

// next reads one update off a watch, failing the case if none arrives.
func next[U any](h *fleetHarness, updates <-chan U, what string) U {
	h.t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			h.t.Fatalf("the %s watch closed while the store was healthy", what)
		}
		return u
	case <-time.After(stallBudget):
		h.t.Fatalf("the %s watch delivered nothing", what)
	}
	var zero U
	return zero
}

var modelRetryCases = []fleetCase{{
	// THE LATER STAMP WINS, whichever write lands last: every node clears
	// the windows reported before the stamp, so a later request clears a
	// superset of what an earlier one does, and an earlier one landing
	// second must not take that back.
	name: "a model retry keeps the later stamp",
	fn: func(h *fleetHarness) {
		later := h.retry("default", coord.RetryClear, h.now().Add(time.Minute), "jane-doe")
		if later.Version == 0 || later.Key != "default" || later.ByKind != iam.ActorHuman ||
			later.OperatorID != sessionOf("jane-doe") || !later.At.Equal(h.now().Add(time.Minute)) {
			h.t.Fatalf("stored %+v, want jane's retry with a version", later)
		}
		kept := h.retry("default", coord.RetryProbe, h.now(), "omar-ali")
		if kept.By != "jane-doe" || kept.Version != later.Version {
			h.t.Fatalf("an earlier request landing second returned %+v, want jane's later one kept", kept)
		}
		newer := h.retry("default", coord.RetryProbe, h.now().Add(2*time.Minute), "omar-ali")
		if newer.By != "omar-ali" || newer.Version == later.Version || newer.Mode != coord.RetryProbe {
			h.t.Fatalf("a later request returned %+v, want omar's stored over jane's", newer)
		}
		read, found, err := h.f.ModelRetry(h.ctx, "default")
		if err != nil || !found || read.Version != newer.Version || read.By != "omar-ali" {
			h.t.Fatalf("ModelRetry = %+v (found %v, %v), want omar's", read, found, err)
		}
		// Removal is at the version read, so a remove that read jane's
		// record cannot take omar's away.
		if gone, err := h.f.DeleteModelRetry(h.ctx, "default", later.Version); err != nil || gone {
			h.t.Fatalf("a remove at a superseded version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteModelRetry(h.ctx, "default", 0); err != nil || gone {
			h.t.Fatalf("a remove with no version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteModelRetry(h.ctx, "default", newer.Version); err != nil || !gone {
			h.t.Fatalf("a remove at the current version = (%v, %v)", gone, err)
		}
		if _, found, _ := h.f.ModelRetry(h.ctx, "default"); found {
			h.t.Fatal("a removed retry still reads")
		}
		// And the entry can be retried again over the purge.
		h.retry("default", coord.RetryClear, h.now(), "jane-doe")
	},
}, {
	name: "a model retry names its entry, mode, instant and author",
	fn: func(h *fleetHarness) {
		for _, bad := range []coord.ModelRetry{
			{Mode: coord.RetryClear, At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			{Key: "default", At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			{Key: "default", Mode: "force", At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			{Key: "default", Mode: coord.RetryClear, By: "jane-doe", ByKind: iam.ActorHuman},
			{Key: "default", Mode: coord.RetryClear, At: h.now(), ByKind: iam.ActorHuman},
			{Key: "default", Mode: coord.RetryClear, At: h.now(), By: "jane-doe"},
		} {
			if _, err := h.f.PutModelRetry(h.ctx, bad); err == nil {
				h.t.Errorf("PutModelRetry(%+v) was accepted", bad)
			}
		}
		if all, _ := h.f.ListModelRetries(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused retry left %+v behind", all)
		}
	},
}, {
	// Every node builds its copy from this: the records that exist, the
	// marker, then each change, a removal included.
	name: "a model retry watch sees a retry and its removal, and lists by entry",
	fn: func(h *fleetHarness) {
		h.retry("sonnet", coord.RetryClear, h.now(), "jane-doe")
		h.retry("aux", coord.RetryClear, h.now(), "jane-doe")
		all, err := h.f.ListModelRetries(h.ctx)
		if err != nil || len(all) != 2 || all[0].Key != "aux" || all[1].Key != "sonnet" {
			h.t.Fatalf("ListModelRetries = %+v, %v; want aux then sonnet", all, err)
		}

		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates, err := h.f.WatchModelRetries(ctx)
		if err != nil {
			h.t.Fatalf("WatchModelRetries: %v", err)
		}
		for _, want := range []string{"aux", "sonnet"} {
			if u := next(h, updates, "retry"); u.Key != want || u.Retry == nil || u.Current {
				h.t.Fatalf("update = %+v, want %s's existing retry", u, want)
			}
		}
		if u := next(h, updates, "retry"); !u.Current {
			h.t.Fatalf("update = %+v, want the marker", u)
		}
		stored := h.retry("default", coord.RetryProbe, h.now(), "omar-ali")
		u := next(h, updates, "retry")
		if u.Key != "default" || u.Retry == nil || u.Retry.Version != stored.Version || u.Retry.Mode != coord.RetryProbe {
			h.t.Fatalf("update after a retry = %+v, want default's as stored", u)
		}
		if gone, err := h.f.DeleteModelRetry(h.ctx, "default", stored.Version); err != nil || !gone {
			h.t.Fatalf("DeleteModelRetry = (%v, %v)", gone, err)
		}
		if u = next(h, updates, "retry"); u.Key != "default" || u.Retry != nil {
			h.t.Fatalf("update after a removal = %+v, want default's removed", u)
		}
	},
}}

var deviceSignInCases = []fleetCase{{
	// ONE SESSION PER ENTRY across the fleet is the store's own create: a
	// second start is told one is running, and only an update at the version
	// read can replace it, which is how a start over an ended session wins
	// and a start racing another loses.
	name: "a device sign-in is one per entry, compare and set",
	fn: func(h *fleetHarness) {
		first, created, err := h.f.CreateDeviceSignIn(h.ctx, h.signIn("codex-review", "node-1"))
		if err != nil || !created || first.Version == 0 {
			h.t.Fatalf("CreateDeviceSignIn = %+v (%v, %v), want the first session stored", first, created, err)
		}
		if _, again, againErr := h.f.CreateDeviceSignIn(h.ctx, h.signIn("codex-review", "node-2")); againErr != nil || again {
			h.t.Fatalf("a second start for the entry = (%v, %v), want it refused", again, againErr)
		}
		pending := first
		pending.Status, pending.URL, pending.Code = coord.DevicePending, "https://example.com/device", "QXJM-7KPD"
		moved, ok, err := h.f.UpdateDeviceSignIn(h.ctx, pending)
		if err != nil || !ok || moved.Version == first.Version {
			h.t.Fatalf("UpdateDeviceSignIn at the version read = %+v (%v, %v)", moved, ok, err)
		}
		if _, ok, _ := h.f.UpdateDeviceSignIn(h.ctx, first); ok {
			h.t.Fatal("an update at a superseded version won, overwriting the code the person is typing")
		}
		read, found, err := h.f.DeviceSignIn(h.ctx, "codex-review")
		if err != nil || !found || read.Code != "QXJM-7KPD" || read.Node != "node-1" ||
			read.ID != first.ID || !read.ExpiresAt.Equal(first.ExpiresAt) || read.ByKind != iam.ActorHuman {
			h.t.Fatalf("DeviceSignIn = %+v (found %v, %v), want node-1's pending session", read, found, err)
		}
		if gone, _ := h.f.DeleteDeviceSignIn(h.ctx, "codex-review", first.Version); gone {
			h.t.Fatal("a removal at a superseded version won")
		}
		if gone, err := h.f.DeleteDeviceSignIn(h.ctx, "codex-review", moved.Version); err != nil || !gone {
			h.t.Fatalf("DeleteDeviceSignIn at the current version = (%v, %v)", gone, err)
		}
		if _, created, _ := h.f.CreateDeviceSignIn(h.ctx, h.signIn("codex-review", "node-2")); !created {
			h.t.Fatal("an entry whose session was removed could not start another")
		}
	},
}, {
	name: "a device sign-in names its session, entry, node, status, lifetime and author",
	fn: func(h *fleetHarness) {
		good := h.signIn("codex-review", "node-1")
		for name, mutate := range map[string]func(*coord.DeviceSignIn){
			"no id":      func(s *coord.DeviceSignIn) { s.ID = uuid.Nil },
			"no entry":   func(s *coord.DeviceSignIn) { s.Entry = "" },
			"no node":    func(s *coord.DeviceSignIn) { s.Node = "" },
			"no status":  func(s *coord.DeviceSignIn) { s.Status = "" },
			"bad status": func(s *coord.DeviceSignIn) { s.Status = "waiting" },
			"no expiry":  func(s *coord.DeviceSignIn) { s.ExpiresAt = time.Time{} },
			"no author":  func(s *coord.DeviceSignIn) { s.By = "" },
			"no kind":    func(s *coord.DeviceSignIn) { s.ByKind = "" },
		} {
			bad := good
			mutate(&bad)
			if _, created, err := h.f.CreateDeviceSignIn(h.ctx, bad); err == nil || created {
				h.t.Errorf("%s: CreateDeviceSignIn = (%v, %v), want a refusal", name, created, err)
			}
		}
		if all, _ := h.f.ListDeviceSignIns(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused session left %+v behind", all)
		}
	},
}, {
	// The node running a session hears a cancel through this, and every
	// other node hears the ending a status read answers with.
	name: "a device sign-in watch sees a session move and its removal",
	fn: func(h *fleetHarness) {
		started, _, err := h.f.CreateDeviceSignIn(h.ctx, h.signIn("codex-review", "node-1"))
		if err != nil {
			h.t.Fatalf("CreateDeviceSignIn: %v", err)
		}
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates, err := h.f.WatchDeviceSignIns(ctx)
		if err != nil {
			h.t.Fatalf("WatchDeviceSignIns: %v", err)
		}
		if u := next(h, updates, "sign-in"); u.Entry != "codex-review" || u.SignIn == nil || u.SignIn.Status != coord.DeviceStarting {
			h.t.Fatalf("update = %+v, want the existing session", u)
		}
		if u := next(h, updates, "sign-in"); !u.Current {
			h.t.Fatalf("update = %+v, want the marker", u)
		}
		asked := started
		asked.Status = coord.DeviceCancelRequested
		moved, ok, err := h.f.UpdateDeviceSignIn(h.ctx, asked)
		if err != nil || !ok {
			h.t.Fatalf("UpdateDeviceSignIn = (%v, %v)", ok, err)
		}
		if u := next(h, updates, "sign-in"); u.SignIn == nil || u.SignIn.Status != coord.DeviceCancelRequested || u.SignIn.Version != moved.Version {
			h.t.Fatalf("update after a cancel request = %+v, want it as stored", u)
		}
		if gone, err := h.f.DeleteDeviceSignIn(h.ctx, "codex-review", moved.Version); err != nil || !gone {
			h.t.Fatalf("DeleteDeviceSignIn = (%v, %v)", gone, err)
		}
		if u := next(h, updates, "sign-in"); u.Entry != "codex-review" || u.SignIn != nil {
			h.t.Fatalf("update after a removal = %+v, want the session removed", u)
		}
	},
}}
