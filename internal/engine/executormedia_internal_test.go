package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A TAKE KEEPS ITS MEDIA APART ONLY ONCE EVERY READER READS IT, as the engine's
// policy answers a take: every node the executor log counts reads a capture
// naming media (executordomain.MediaVersion), and every node that may settle a
// run's row, a seats-only one included, says it asks its stamp again whole
// (sandbox.CaptureStampVersion), live on its presence, offline on its position
// row. A node below either or not saying keeps them inline, this node aside.
//
// Mutation: answer apart whatever the readers say, skip a node at zero, read
// only data nodes' presences or a live node's row, count no offline node, or
// read this node's own presence, and a case goes red.
func TestATakeKeepsMediaApartOnlyOnceEveryReaderReadsIt(t *testing.T) {
	t.Parallel()
	media, stamp := executordomain.MediaVersion, sandbox.CaptureStampVersion
	data, seats := placement.Roles(placement.RoleData, placement.RoleSeats), placement.Roles(placement.RoleSeats)
	type presence struct {
		node    string
		roles   placement.RoleSet
		version int
	}
	for name, tc := range map[string]struct {
		rows  []coord.NodePositions
		live  []presence
		apart bool
	}{
		"every node says both": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp),
			mediaRow("data-b", media+3, stamp)}, live: []presence{{"data-a", data, stamp},
			{"data-b", data, stamp + 1}, {"seats-c", seats, stamp}}, apart: true},
		"a seats-only node that says no stamp": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp)},
			live: []presence{{"data-a", data, stamp}, {"seats-c", seats, 0}}},
		"a seats-only node that says nothing": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp)},
			live: []presence{{"data-a", data, stamp}, {"seats-c", seats, -1}}},
		"a live data node whose presence says no stamp, its row one": {rows: []coord.NodePositions{
			mediaRow("data-a", media, stamp), mediaRow("data-b", media, stamp)},
			live: []presence{{"data-a", data, stamp}, {"data-b", data, 0}}},
		"a live data node with no position row yet": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp)},
			live: []presence{{"data-a", data, stamp}, {"data-d", data, stamp}}},
		"an offline node the log counts, its row last saying no stamp": {rows: []coord.NodePositions{
			mediaRow("data-a", media, stamp), mediaRow("data-e", media, 0)}, live: []presence{{"data-a", data, stamp}}},
		"an offline node the log counts, its row last saying the stamp": {rows: []coord.NodePositions{
			mediaRow("data-a", media, stamp), mediaRow("data-e", media, stamp)},
			live: []presence{{"data-a", data, stamp}}, apart: true},
		"a log reader below the media version": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp),
			mediaRow("data-b", media-1, stamp)}, live: []presence{{"data-a", data, stamp}, {"data-b", data, stamp}}},
		"no reader": {},
		"an offline node that applies no executor log": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp),
			{NodeID: "data-x", Domains: map[string]coord.DomainPosition{"tracker": {RecordVersion: 1}}}},
			live: []presence{{"data-a", data, stamp}}, apart: true},
		"this node's own presence not saying": {rows: []coord.NodePositions{mediaRow("data-a", media, stamp)},
			live: []presence{{"data-a", data, -1}}, apart: true},
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
			if got := mediaPolicy(t, mediaNode(t, fleet, leases)); got != tc.apart {
				t.Errorf("media kept apart %v, want %v", got, tc.apart)
			}
		})
	}
	for name, tc := range map[string]struct {
		fleet  func(*rosterFleet) coord.Fleet
		leases coord.Backend
	}{
		"a position register that cannot be read": {fleet: func(f *rosterFleet) coord.Fleet {
			return unreadablePositions{f}
		}, leases: coordmemory.New()},
		"live nodes that cannot be listed": {leases: unlistedNodes{coordmemory.New()}},
		"no coordination store":            {},
	} {
		fleet := &rosterFleet{memoryFleet: coordmemory.NewFleet()}
		e := mediaNode(t, fleet, tc.leases)
		if tc.fleet != nil {
			e.backends.Fleet = tc.fleet(fleet)
		}
		if mediaPolicy(t, e) {
			t.Errorf("%s: media kept apart, want inline", name)
		}
		if why := e.mediaInline.Load(); why == nil || *why == "" {
			t.Errorf("%s: kept inline with no why to log", name)
		}
	}
}

// READERS NOT READ WITHIN THEIR ROOM KEEP MEDIA INLINE, LOGGED ONCE: a store
// that does not answer the gate's reads holds a take no longer than
// sandbox.MediaReadersRoom, which BoxReadBudget counts beside the take, and
// every take it answers says why on one line, not one a take. Exact, bubbled.
//
// Mutation: read without the room, or log every answer, and it goes red.
func TestReadersNotReadWithinTheirRoomKeepMediaInline(t *testing.T) {
	logs := captureLogs(t)
	synctest.Test(t, func(t *testing.T) {
		e := &Engine{testNode: "data-a", backends: &Backends{
			Fleet: silentPositions{&rosterFleet{memoryFleet: coordmemory.NewFleet()}}, Coord: coordmemory.New()}}
		for range 2 {
			began := time.Now()
			if apart, _ := e.executorMediaRead(t.Context()); apart {
				t.Error("media kept apart past the readers' room, want inline")
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
	if why, _ := lines[0]["why"].(string); !strings.Contains(why, sandbox.MediaReadersRoom.String()) {
		t.Errorf("the line says %q, want it to name the room the readers were not read within", why)
	}
}

// EVERY NODE SAYS THE CAPTURE STAMP IT ASKS AGAIN WHOLE on its presence, the
// one record a node without `data` writes, read back as a peer reads it (a
// data node's position row says it too: TestTheRunningEstateIsTheDeclaredEstate).
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
// build reads, and the capture stamp's version it says.
func mediaRow(node string, record, stamp int) coord.NodePositions {
	return coord.NodePositions{NodeID: node, CaptureStampVersion: stamp,
		Domains: map[string]coord.DomainPosition{executordomain.Domain{}.Name(): {RecordVersion: record}}}
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

// mediaPolicy is whether the engine's policy has a take keep its media apart,
// failing where the policy keeps nothing to take.
func mediaPolicy(t *testing.T, e *Engine) bool {
	t.Helper()
	policy, err := executorKeep{engine: e}.Policy(t.Context())
	if err != nil || !policy.Keep {
		t.Fatalf("the policy reads (%+v, %v), want one that keeps sessions", policy, err)
	}
	return policy.MediaApart
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
