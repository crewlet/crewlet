package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A TAKE KEEPS ITS MEDIA APART ONLY ONCE EVERY READER READS IT: every node the
// executor log counts at executordomain.MediaCaptureVersion, every node that
// may settle a run's row at sandbox.MediaStampVersion, live on its presence,
// offline on its record; one that recorded nothing is not looked for. A no
// names the first node below; a standing retirement uncounts an offline node.
// ONE THAT HAS NOT SAID IS UNKNOWN, NEVER BELOW: a presence with no status
// stands on its record, or with none is unsaid, as is a log reader with no row.
//
// Mutation: answer apart whatever, skip a node at zero, count no record or a
// node with none, read a record over a live presence, read a presence with no
// status or a log reader at zero as below, honour a retirement a later record
// voids, read this node's own presence, or name a node out of order.
func TestATakeKeepsMediaApartOnlyOnceEveryReaderReadsIt(t *testing.T) {
	t.Parallel()
	media, stamp := executordomain.MediaCaptureVersion, sandbox.MediaStampVersion
	data, seats := placement.Roles(placement.RoleData, placement.RoleSeats), placement.Roles(placement.RoleSeats)
	recorded := time.Now().UTC().Add(-2 * time.Hour)
	type presence struct {
		node    string
		roles   placement.RoleSet
		version int
	}
	selfRow, self := mediaRow("data-a", media), presence{"data-a", data, stamp}
	for name, tc := range map[string]struct {
		rows   []coord.NodePositions
		live   []presence
		gone   []string       // held presence, released it
		stamps map[string]int // recorded two hours ago
		// retired stand from an hour after the records, retiredBefore from
		// an hour before them, so a record voids it.
		retired, retiredBefore []string
		apart                  bool
		below                  string // the node a definite no names
		unread                 bool
	}{
		"every node says both": {rows: []coord.NodePositions{selfRow, mediaRow("data-b", media+3)},
			live: []presence{self, {"data-b", data, stamp + 1}, {"seats-c", seats, stamp}}, apart: true},
		"a live seats-only node that says no stamp": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-c", seats, 0}}, below: "seats-c"},
		"a live node saying no status this beat, its record at the stamp": {rows: []coord.NodePositions{
			selfRow}, live: []presence{self, {"seats-c", seats, -1}}, stamps: map[string]int{"seats-c": stamp},
			apart: true},
		"a live node saying no status this beat, its record below": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-c", seats, -1}}, stamps: map[string]int{"seats-c": 0},
			below: "seats-c"},
		"a live node saying no status this beat, with no record": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-c", seats, -1}}, unread: true},
		"a live data node with no position row": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"data-b", data, stamp}}, unread: true},
		"a node below beside one unsaid": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-c", seats, -1}, {"seats-d", seats, 0}}, below: "seats-d"},
		"a live node whose presence says no stamp, its record one": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-c", seats, 0}}, stamps: map[string]int{"seats-c": stamp},
			below: "seats-c"},
		"an offline seats-only node whose record says zero": {rows: []coord.NodePositions{selfRow},
			live: []presence{self}, gone: []string{"seats-x"}, stamps: map[string]int{"seats-x": 0},
			below: "seats-x"},
		"an offline node that recorded nothing": {rows: []coord.NodePositions{selfRow},
			live: []presence{self}, gone: []string{"seats-x"}, apart: true},
		"an offline node recorded at the stamp": {rows: []coord.NodePositions{selfRow},
			live: []presence{self}, gone: []string{"seats-x"}, stamps: map[string]int{"seats-x": stamp},
			apart: true},
		"a recorded node at zero this store never saw present": {rows: []coord.NodePositions{selfRow},
			live: []presence{self}, stamps: map[string]int{"seats-z": 0}, below: "seats-z"},
		"an offline node whose retirement stands": {rows: []coord.NodePositions{selfRow},
			live: []presence{self}, gone: []string{"seats-x"}, stamps: map[string]int{"seats-x": 0},
			retired: []string{"seats-x"}, apart: true},
		"a retired node that recorded since": {rows: []coord.NodePositions{selfRow},
			live: []presence{self}, gone: []string{"seats-x"}, stamps: map[string]int{"seats-x": 0},
			retiredBefore: []string{"seats-x"}, below: "seats-x"},
		"a retired node live again": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-x", seats, 0}}, retired: []string{"seats-x"}, below: "seats-x"},
		"an offline data node the log counts, recorded at the stamp": {rows: []coord.NodePositions{selfRow,
			mediaRow("data-e", media)}, live: []presence{self}, gone: []string{"data-e"},
			stamps: map[string]int{"data-e": stamp}, apart: true},
		"an offline data node the log counts, recorded nowhere": {rows: []coord.NodePositions{selfRow,
			mediaRow("data-e", media)}, live: []presence{self}, apart: true},
		"a log reader below the media version": {rows: []coord.NodePositions{selfRow,
			mediaRow("data-b", media-1)}, live: []presence{self, {"data-b", data, stamp}}, below: "data-b"},
		"the first node below in node order": {rows: []coord.NodePositions{selfRow},
			live: []presence{self, {"seats-d", seats, 0}, {"seats-b", seats, 0}}, gone: []string{"seats-c"},
			below: "seats-b"},
		"an offline node that applies no executor log, recorded at the stamp": {rows: []coord.NodePositions{
			selfRow, {NodeID: "data-x", Domains: map[string]coord.DomainPosition{"tracker": {RecordVersion: 1}}}},
			live: []presence{self}, stamps: map[string]int{"data-x": stamp}, apart: true},
		"an offline node that applies no executor log, recorded nowhere": {rows: []coord.NodePositions{
			selfRow, {NodeID: "data-x", Domains: map[string]coord.DomainPosition{"tracker": {RecordVersion: 1}}}},
			live: []presence{self}, apart: true},
		"this node's own presence not saying": {rows: []coord.NodePositions{selfRow},
			live: []presence{{"data-a", data, -1}}, apart: true},
		"no node the log counts": {unread: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet, leases := &rosterFleet{memoryFleet: coordmemory.NewFleet()}, coordmemory.New()
			for _, row := range tc.rows {
				if err := fleet.PutPositions(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range tc.live {
				sayStamp(t, leases, p.node, p.roles, p.version)
			}
			for _, node := range tc.gone {
				leaveFleet(t, leases, node)
			}
			read := mediaFleet{rosterFleet: fleet}
			for node, version := range tc.stamps {
				read.stamps = append(read.stamps, coord.CaptureStampRecord{NodeID: node,
					CaptureStampVersion: version, StoredAt: recorded})
			}
			for at, nodes := range map[time.Time][]string{recorded.Add(time.Hour): tc.retired,
				recorded.Add(-time.Hour): tc.retiredBefore} {
				for _, node := range nodes {
					read.retired = append(read.retired, coord.NodeRetirement{NodeID: node, StoredAt: at})
				}
			}
			e := mediaNode(t, fleet, leases)
			e.backends.Fleet = read
			got := mediaPolicy(t, e)
			switch {
			case got.MediaApart != tc.apart:
				t.Errorf("media kept apart %v, want %v (%+v)", got.MediaApart, tc.apart, got)
			case (got.MediaUnread != "") != tc.unread:
				t.Errorf("the readers unread says %q, want unread %v", got.MediaUnread, tc.unread)
			case tc.below != "" && !strings.HasPrefix(got.MediaBelow, "node "+tc.below+" "):
				t.Errorf("the node below reads %q, want it to name %s", got.MediaBelow, tc.below)
			}
		})
	}
}

// THE MEDIA GATE'S READS FIT ITS ROOM AT THE FLOOR: every read the policy makes
// across a link ([executorKeep.Policy]) of a fleet at its widest
// (coord.MaxMaintenanceParticipants nodes): the activation pointer at its bound
// ([Engine.servedFleetRevision]), and the gate's ([Engine.readMediaReaders]):
// each node's presence at its class's bound, its position row, record and
// retirement at their widest, each reply whole at the floor pace (jsapi.Crossing
// at objstore.MiBPace), a read term for each read's own round trip. The log's
// evictions are this node's own rows. The figure is MediaReadersRoom's doc's.
//
// Mutation: drop a listing or the pointer, or raise the fleet past the room.
func TestTheMediaGatesReadsFitItsRoomAtTheFloor(t *testing.T) {
	t.Parallel()
	widest := map[string]int{}
	for _, w := range coordtest.WidestPositionsRecords() {
		widest[w.Class] = max(widest[w.Class], w.Stored())
	}
	cross := func(n int) time.Duration { return jsapi.Crossing(n, objstore.MiBPace) }
	node := cross(coord.SmallValueBytes) + cross(widest[coord.PositionClass]) +
		cross(widest[coord.CaptureStampClass]) + cross(widest[coord.RetirementClass])
	const listings = 4
	pointer := cross(coord.MaxPointerBytes) + jsprovision.ReadTerm
	reads := pointer + time.Duration(coord.MaxMaintenanceParticipants)*node + listings*jsprovision.ReadTerm
	if reads > sandbox.MediaReadersRoom {
		t.Errorf("the gate's reads take %v at the floor, past its room, %v", reads, sandbox.MediaReadersRoom)
	}
	if got := reads.Round(100 * time.Millisecond).String(); got != "24.7s" {
		t.Errorf("the gate's reads take %s at the floor, and sandbox.MediaReadersRoom's doc states 24.7s: "+
			"restate it", got)
	}
}

// EVERY UNKNOWN KEEPS MEDIA INLINE AS UNKNOWN, NEVER AS A NODE BELOW: a read
// that failed or a log nobody counted is why the readers went unread, and the
// policy names no node.
//
// Mutation: answer a failed read or an empty set as a node below, or as apart,
// and a case goes red.
func TestEveryUnknownKeepsMediaInlineAsUnknown(t *testing.T) {
	t.Parallel()
	failed := errors.New("the store did not answer")
	for name, tc := range map[string]struct {
		fleet  func(*rosterFleet) coord.Fleet
		leases func() coord.Backend
	}{
		"a position register that cannot be read": {fleet: func(f *rosterFleet) coord.Fleet {
			return unreadablePositions{f}
		}},
		"live nodes that cannot be listed": {leases: func() coord.Backend {
			return unlistedNodes{coordmemory.New()}
		}},
		"capture stamp records that cannot be read": {fleet: func(f *rosterFleet) coord.Fleet {
			return mediaFleet{rosterFleet: f, stampsErr: failed}
		}},
		"retirements that cannot be read": {fleet: func(f *rosterFleet) coord.Fleet {
			return mediaFleet{rosterFleet: f, retiredErr: failed}
		}},
		"no node the log counts": {},
		"no coordination store":  {leases: func() coord.Backend { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet, leases := &rosterFleet{memoryFleet: coordmemory.NewFleet()}, coord.Backend(coordmemory.New())
			if tc.leases != nil {
				leases = tc.leases()
			}
			row := mediaRow("data-a", executordomain.MediaCaptureVersion)
			if name != "no node the log counts" {
				if err := fleet.PutPositions(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			}
			e := mediaNode(t, fleet, leases)
			if tc.fleet != nil {
				e.backends.Fleet = tc.fleet(fleet)
			}
			got := mediaPolicy(t, e)
			if got.MediaUnread == "" || got.MediaBelow != "" {
				t.Errorf("the policy reads %+v, want the readers unread and no node below", got)
			}
		})
	}
}

// READERS NOT READ WITHIN THEIR ROOM KEEP MEDIA INLINE, LOGGED ONCE: a store
// that does not answer the gate's reads holds a take no longer than
// sandbox.MediaReadersRoom, which BoxReadBudget counts beside the take, and
// every take it answers says why on one line of the unread kind, not one a
// take. Exact, bubbled.
//
// Mutation: read without the room, log every answer, or log no kind, and it
// goes red.
func TestReadersNotReadWithinTheirRoomKeepMediaInline(t *testing.T) {
	logs := captureLogs(t)
	synctest.Test(t, func(t *testing.T) {
		e := &Engine{testNode: "data-a", backends: &Backends{
			Fleet: silentPositions{&rosterFleet{memoryFleet: coordmemory.NewFleet()}}, Coord: coordmemory.New()}}
		for range 2 {
			began := time.Now()
			if media := e.executorMediaRead(t.Context()); media.apart || media.unread == "" {
				t.Errorf("past the readers' room the gate answers %+v, want the readers unread", media)
			}
			if took := time.Since(began); took != sandbox.MediaReadersRoom {
				t.Errorf("the readers were read for %v, want the room's %v", took, sandbox.MediaReadersRoom)
			}
		}
	})
	var lines []map[string]any
	for _, record := range logs.records(t) {
		if record["msg"] == "executor_media_kept_inline" {
			lines = append(lines, record)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines for two takes kept inline for one reason, want one", len(lines))
	}
	if why, _ := lines[0]["why"].(string); !strings.Contains(why, sandbox.MediaReadersRoom.String()) ||
		lines[0]["kind"] != "unread" {
		t.Errorf("the line says %q (%v), want an unread kind naming the room the readers were not read "+
			"within", why, lines[0]["kind"])
	}
}

// A NODE BELOW KEEPS MEDIA INLINE, LOGGED ONCE NAMING IT: a definite no is
// logged as an unknown one is, once each time its sentence changes, of the
// below kind, so an operator reads which node to upgrade or retire.
//
// Mutation: log only an unknown answer, log every answer, or log no kind, and
// it goes red.
func TestANodeBelowKeepsMediaInlineLoggedOnceNamingIt(t *testing.T) {
	logs := captureLogs(t)
	fleet, leases := &rosterFleet{memoryFleet: coordmemory.NewFleet()}, coordmemory.New()
	if err := fleet.PutPositions(t.Context(), mediaRow("data-a", executordomain.MediaCaptureVersion)); err != nil {
		t.Fatal(err)
	}
	sayStamp(t, leases, "seats-c", placement.Roles(placement.RoleSeats), 0)
	e := mediaNode(t, fleet, leases)
	for range 2 {
		if got := mediaPolicy(t, e); !strings.HasPrefix(got.MediaBelow, "node seats-c ") {
			t.Errorf("the policy reads %+v, want seats-c named below", got)
		}
	}
	var lines []map[string]any
	for _, record := range logs.records(t) {
		if record["msg"] == "executor_media_kept_inline" {
			lines = append(lines, record)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines for two takes kept inline for one node below, want one", len(lines))
	}
	if why, _ := lines[0]["why"].(string); lines[0]["kind"] != "below" || !strings.Contains(why, "seats-c") {
		t.Errorf("the line says %q (%v), want the below kind naming seats-c", why, lines[0]["kind"])
	}
}

// EVERY NODE SAYS THE CAPTURE STAMP IT ASKS AGAIN WHOLE on its presence, read
// back as a peer reads it (offline, its record says it:
// TestEveryNodeRecordsItsCaptureStampBeforeItClaimsPresence).
//
// Mutation: publish no version on the status, and it goes red.
func TestEveryNodeSaysTheCaptureStampItAsksAgainWhole(t *testing.T) {
	t.Parallel()
	meta := map[string]any{coord.StatusKey: statusOf(t.Context(), t, &Engine{}).Meta()}
	if status, ok := coord.StatusFromMeta(meta); !ok || status.CaptureStampVersion != sandbox.CaptureStampVersion {
		t.Errorf("a peer reads the node's presence as saying capture stamp %d (%v), want this build's %d",
			status.CaptureStampVersion, ok, sandbox.CaptureStampVersion)
	}
}

// mediaRow is node's position row: the executor log's record version its
// build reads.
func mediaRow(node string, record int) coord.NodePositions {
	return coord.NodePositions{NodeID: node,
		Domains: map[string]coord.DomainPosition{executordomain.Domain{}.Name(): {RecordVersion: record}}}
}

// leaveFleet has node hold its presence and give it back, as a node that
// stopped: it held presence once, and holds none now.
func leaveFleet(t *testing.T, leases coord.Backend, node string) {
	t.Helper()
	lease, _, err := leases.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":1", TTL: time.Hour, Ungated: true})
	if err != nil || lease == nil {
		t.Fatalf("claim %s's presence: %v", node, err)
	}
	if _, err := leases.Release(t.Context(), lease.Resource, lease.Owner, lease.Epoch); err != nil {
		t.Fatal(err)
	}
}

// sayStamp has node hold its presence with roles, its status saying version
// of the capture stamp, or no status at all for -1.
func sayStamp(t *testing.T, leases coord.Backend, node string, roles placement.RoleSet, version int) {
	t.Helper()
	meta := placement.NodeProfile{ID: node, Roles: roles}.Meta()
	if version >= 0 {
		meta[coord.StatusKey] = coord.NodeStatus{CaptureStampVersion: version}.Meta()
	}
	if _, _, err := leases.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":1", TTL: time.Hour, Preferred: node, Ungated: true, Meta: meta}); err != nil {
		t.Fatal(err)
	}
}

// mediaNode is node data-a serving the fleet's activation of a company that
// keeps sessions, with an object store to keep them in, reading the media's
// readers off fleet and leases: an engine whose policy reaches the gate.
func mediaNode(t *testing.T, fleet *rosterFleet, leases coord.Backend) *Engine {
	t.Helper()
	objects, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{testNode: "data-a", objects: &objectStore{store: objects},
		backends: &Backends{Fleet: fleet, Coord: leases}}
	e.epoch.current.Store(&Company{ActivatedAt: activate(t, fleet, "rev-1").At})
	return e
}

// mediaPolicy is the engine's policy for a take, failing where it keeps
// nothing to take or says other than exactly one of apart, unread and below.
func mediaPolicy(t *testing.T, e *Engine) sandbox.SessionPolicy {
	t.Helper()
	policy, err := executorKeep{engine: e}.Policy(t.Context())
	if err != nil || !policy.Keep {
		t.Fatalf("the policy reads (%+v, %v), want one that keeps sessions", policy, err)
	}
	if said := btoi(policy.MediaApart) + btoi(policy.MediaUnread != "") + btoi(policy.MediaBelow != ""); said != 1 {
		t.Fatalf("the policy reads %+v, want exactly one of apart, unread and below", policy)
	}
	return policy
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mediaFleet is a fleet whose capture stamp records and retirements are the
// case's, each at the instant it names, or fail.
type mediaFleet struct {
	*rosterFleet
	stamps                []coord.CaptureStampRecord
	retired               []coord.NodeRetirement
	stampsErr, retiredErr error
}

func (f mediaFleet) CaptureStamps(context.Context) ([]coord.CaptureStampRecord, error) {
	return f.stamps, f.stampsErr
}

func (f mediaFleet) NodeRetirements(context.Context) ([]coord.NodeRetirement, error) {
	return f.retired, f.retiredErr
}

// unreadablePositions is a fleet whose position register cannot be read.
type unreadablePositions struct{ *rosterFleet }

func (unreadablePositions) Positions(context.Context) ([]coord.NodePositions, error) {
	return nil, errors.New("the store did not answer")
}

// silentPositions is a fleet whose position register answers no read before
// its reader stops waiting.
type silentPositions struct{ *rosterFleet }

func (silentPositions) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// unlistedNodes is a coordination store whose live nodes cannot be listed.
type unlistedNodes struct{ coord.Backend }

func (unlistedNodes) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, errors.New("the store did not answer")
}
