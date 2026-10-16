package coordtest

import (
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- backup points ------------------------------------------------------ //

// The streams the cases acknowledge, named as the engine's own logs are: what
// is under test is that each is a row of its own, not what they are called.
const (
	trackerLog = "CREWLET_TRACKER_LOG"
	pagesLog   = "CREWLET_PAGES_LOG"
	vectorsLog = "CREWLET_TRACKER_VECTORS"
)

// putBackup writes one point, failing the case on any refusal.
func (h *fleetHarness) putBackup(p coord.BackupPoint) {
	h.t.Helper()
	if err := h.f.PutBackupPoint(h.ctx, p); err != nil {
		h.t.Fatalf("PutBackupPoint(%s): %v", p.Owner, err)
	}
}

// backups reads the register.
func (h *fleetHarness) backups() []coord.BackupPoint {
	h.t.Helper()
	points, err := h.f.BackupPoints(h.ctx)
	if err != nil {
		h.t.Fatalf("BackupPoints: %v", err)
	}
	return points
}

// acknowledge is `crewlet retention ack -stream <stream> -position <seq>` as
// the route writes it.
func (h *fleetHarness) acknowledge(stream string, seq uint64, at time.Time) {
	h.t.Helper()
	h.putBackup(coord.BackupPoint{
		Owner: coord.OperatorBackupOwner, At: at, Verified: true,
		Streams: map[string]coord.Position{
			stream: {Stream: stream, Generation: 1, Seq: seq},
		},
	})
}

// reach is how far the newest point covering stream reaches, and when it was
// taken — the two facts the trim's backup term reads off it.
func (h *fleetHarness) reach(stream string) (uint64, time.Time, bool) {
	h.t.Helper()
	newest, ok := coord.NewestBackupOf(h.backups(), stream)
	if !ok {
		return 0, time.Time{}, false
	}
	return newest.Streams[stream].Seq, newest.At, true
}

var backupCases = []fleetCase{{
	// THE BUG THIS KEYING EXISTS TO PREVENT. Under `backup_floor: operator`
	// every log is trimmed only as far as an acknowledgement of THAT log
	// says, so an operator acknowledges each one in turn. Keyed by the
	// owner alone, each acknowledgement replaced the last, and following the
	// procedure left exactly one log trimmable — whichever was acknowledged
	// last — with the alarm green beside the others.
	name: "an acknowledgement of one log leaves the others standing",
	fn: func(h *fleetHarness) {
		base := h.now()
		h.acknowledge(trackerLog, 900, base)
		h.acknowledge(pagesLog, 400, base.Add(time.Minute))
		h.acknowledge(vectorsLog, 700, base.Add(2*time.Minute))

		for _, want := range []struct {
			stream string
			seq    uint64
			at     time.Time
		}{
			{trackerLog, 900, base},
			{pagesLog, 400, base.Add(time.Minute)},
			{vectorsLog, 700, base.Add(2 * time.Minute)},
		} {
			seq, at, ok := h.reach(want.stream)
			if !ok {
				h.t.Fatalf("%s: no acknowledgement covers it after three in turn — "+
					"a later acknowledgement of another log replaced it (register %+v)",
					want.stream, h.backups())
			}
			if seq != want.seq || !at.Equal(want.at) {
				h.t.Errorf("%s: reaches %d at %v, want %d at %v — each log is aged "+
					"by its own acknowledgement, never by a neighbour's",
					want.stream, seq, at, want.seq, want.at)
			}
		}
	},
}, {
	// AND A SECOND ACKNOWLEDGEMENT OF ONE LOG DOES REPLACE THE FIRST, which
	// is the other half of keying by the log: a row per gesture would grow
	// for ever, and the reader would still take the newest.
	name: "a second acknowledgement of one log replaces the first",
	fn: func(h *fleetHarness) {
		base := h.now()
		h.acknowledge(trackerLog, 900, base)
		h.acknowledge(trackerLog, 1200, base.Add(time.Hour))
		var rows int
		for _, p := range h.backups() {
			if p.Owner == coord.OperatorBackupOwner {
				rows++
			}
		}
		if rows != 1 {
			h.t.Errorf("two acknowledgements of one log left %d rows, want 1: %+v",
				rows, h.backups())
		}
		if seq, at, _ := h.reach(trackerLog); seq != 1200 || !at.Equal(base.Add(time.Hour)) {
			h.t.Errorf("the log reaches %d at %v, want the second acknowledgement's 1200",
				seq, at)
		}
	},
}, {
	// A NODE'S BACKUP IS ONE ARTEFACT, so its next one replaces its last
	// whole — a stream the new copy no longer reaches is not left covered by
	// the old one — and another node's point is untouched by either.
	name: "a node's next backup replaces its last and no other owner's",
	fn: func(h *fleetHarness) {
		base := h.now()
		h.putBackup(coord.BackupPoint{
			Owner: "node-a", At: base, Verified: true, Dir: "/backups/a/1", Bytes: 4096,
			Streams: map[string]coord.Position{
				trackerLog: {Stream: trackerLog, Generation: 1, Seq: 100},
				pagesLog:   {Stream: pagesLog, Generation: 1, Seq: 50},
			},
		})
		h.putBackup(coord.BackupPoint{
			Owner: "node-b", At: base, Verified: true,
			Streams: map[string]coord.Position{
				pagesLog: {Stream: pagesLog, Generation: 1, Seq: 60},
			},
		})
		h.putBackup(coord.BackupPoint{
			Owner: "node-a", At: base.Add(time.Hour), Verified: true, Dir: "/backups/a/2",
			Streams: map[string]coord.Position{
				trackerLog: {Stream: trackerLog, Generation: 1, Seq: 300},
			},
		})
		points := h.backups()
		if len(points) != 2 {
			h.t.Fatalf("register holds %d rows, want node-a's newest and node-b's: %+v",
				len(points), points)
		}
		byOwner := map[string]coord.BackupPoint{}
		for _, p := range points {
			byOwner[p.Owner] = p
		}
		a := byOwner["node-a"]
		if a.Dir != "/backups/a/2" || a.Bytes != 0 || len(a.Streams) != 1 ||
			a.Streams[trackerLog].Seq != 300 {
			h.t.Errorf("node-a = %+v, want its second backup whole", a)
		}
		if b := byOwner["node-b"]; b.Streams[pagesLog].Seq != 60 {
			h.t.Errorf("node-b = %+v, want its own point untouched", b)
		}
		// THE PAGES LOG IS NOW COVERED BY node-b ALONE: node-a's newer
		// copy does not reach it, and the reader takes the newest point
		// that DOES rather than the newest point overall.
		if seq, _, ok := h.reach(pagesLog); !ok || seq != 60 {
			h.t.Errorf("pages reaches %d (covered %v), want node-b's 60", seq, ok)
		}
	},
}, {
	// A POINT ROUND-TRIPS WHOLE: every field the trim, the alarm and the
	// backups screen read comes back as it was written.
	name: "a backup point round-trips whole",
	fn: func(h *fleetHarness) {
		want := coord.BackupPoint{
			Owner: "node-a", At: h.now(), Verified: true, Dir: "/backups/a", Bytes: 1 << 20,
			Streams: map[string]coord.Position{
				trackerLog: {Stream: trackerLog, Generation: 3, Seq: 918},
			},
		}
		h.putBackup(want)
		points := h.backups()
		if len(points) != 1 {
			h.t.Fatalf("register holds %+v, want the one point", points)
		}
		got := points[0]
		if got.Owner != want.Owner || !got.At.Equal(want.At) || got.Dir != want.Dir ||
			got.Bytes != want.Bytes || got.Verified != want.Verified ||
			got.Streams[trackerLog] != want.Streams[trackerLog] || len(got.Streams) != 1 {
			h.t.Errorf("read back %+v, want %+v", got, want)
		}
	},
}, {
	// AN ACKNOWLEDGEMENT REACHING TWO LOGS IS REFUSED: it is filed under
	// the one log its key names, so it would replace that log's own
	// acknowledgement with a claim about another.
	name: "an acknowledgement reaching two logs is refused",
	fn: func(h *fleetHarness) {
		err := h.f.PutBackupPoint(h.ctx, coord.BackupPoint{
			Owner: coord.OperatorBackupOwner, At: h.now(), Verified: true,
			Streams: map[string]coord.Position{
				trackerLog: {Stream: trackerLog, Generation: 1, Seq: 900},
				pagesLog:   {Stream: pagesLog, Generation: 1, Seq: 400},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			h.t.Fatalf("PutBackupPoint = %v, want the refusal naming one log", err)
		}
		if points := h.backups(); len(points) != 0 {
			h.t.Errorf("a refused acknowledgement left %+v", points)
		}
	},
}, {
	// FORGETTING AN OWNER REMOVES EVERY ROW IT HOLDS — for the operator,
	// its acknowledgement of every log — and nobody else's.
	name: "forgetting an owner removes every row it holds and no other",
	fn: func(h *fleetHarness) {
		base := h.now()
		h.acknowledge(trackerLog, 900, base)
		h.acknowledge(vectorsLog, 700, base)
		h.putBackup(coord.BackupPoint{
			Owner: "node-a", At: base, Verified: true,
			Streams: map[string]coord.Position{
				trackerLog: {Stream: trackerLog, Generation: 1, Seq: 100},
			},
		})
		if err := h.f.ForgetBackupPoint(h.ctx, coord.OperatorBackupOwner); err != nil {
			h.t.Fatalf("ForgetBackupPoint: %v", err)
		}
		var owners []string
		for _, p := range h.backups() {
			owners = append(owners, p.Owner)
		}
		if !slices.Equal(owners, []string{"node-a"}) {
			h.t.Errorf("after forgetting the operator the register holds %v, "+
				"want node-a's point alone", owners)
		}
		if err := h.f.ForgetBackupPoint(h.ctx, "node-a"); err != nil {
			h.t.Fatalf("ForgetBackupPoint(node-a): %v", err)
		}
		if points := h.backups(); len(points) != 0 {
			h.t.Errorf("after forgetting both the register holds %+v", points)
		}
	},
}}
