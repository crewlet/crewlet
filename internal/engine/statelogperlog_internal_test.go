package engine

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE RUNNING ESTATE IS THE DECLARED ESTATE: its streams, its keys and its
// coordination records are the ones the domain declarations, the topics
// constants and the register name, byte for byte.
//
// Each surface here is named in more than one place — the domain's own
// declaration, the topics constants, the positions register, the floors — and
// a node whose answer drifted from the declarations provisions a second, empty
// stream beside the real one, publishes a register row nobody else's trim
// reads, or writes a floor under a key the fleet's fences never look at. So
// this boots a node the way `crewlet run` does and reads each surface back:
// the logs it runs and their order, each log's stream on the broker, the
// consumer each is applied through, the row its heartbeat writes and the floor
// its trim publishes.
func TestTheRunningEstateIsTheDeclaredEstate(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.native.Load().log

	declared := map[string][3]string{
		tracker.Domain{}.Name(): {topics.TrackerLogStream, topics.TrackerLogPrefix, topics.TrackerLogWildcard},
		search.Domain{}.Name():  {topics.TrackerVectorsStream, topics.TrackerVectorsPrefix, topics.TrackerVectorsWildcard},
		pages.Domain{}.Name():   {topics.PagesLogStream, topics.PagesLogPrefix, topics.PagesLogWildcard},
		usage.Domain{}.Name():   {topics.UsageLogStream, topics.UsageLogPrefix, topics.UsageLogWildcard},
	}
	var registered []string
	for _, d := range registeredDomains() {
		registered = append(registered, d.Name())
	}
	if got := s.held().order; !slices.Equal(got, registered) {
		t.Fatalf("the node keys its logs %v, and the register keys them %v", got, registered)
	}
	for _, running := range s.running() {
		name := running.domain.Name()
		want := declared[name]
		if running.spec.Name != want[0] || running.spec.SubjectPrefix != want[1] ||
			!slices.Equal(running.spec.Subjects, []string{want[2]}) {
			t.Errorf("the %s log runs on (%q, %q, %v); the declared stream is (%q, %q, %q)",
				name, running.spec.Name, running.spec.SubjectPrefix, running.spec.Subjects,
				want[0], want[1], want[2])
		}
		// THE CEILING TIER A SIZED THE DOMAIN AT, whole: one log carries
		// the domain's whole budget.
		if budget := s.ceilings[name].Bytes; running.spec.MaxBytes != budget {
			t.Errorf("the %s log's ceiling is %d, and Tier A sized its domain at %d",
				name, running.spec.MaxBytes, budget)
		}
		stream, err := js.Stream(t.Context(), want[0])
		if err != nil {
			t.Fatalf("the declared %s stream is not on the broker: %v", name, err)
		}
		info, err := stream.Info(t.Context())
		if err != nil {
			t.Fatalf("read %s: %v", want[0], err)
		}
		if info.Config.MaxBytes != running.spec.MaxBytes {
			t.Errorf("%s was created at %d bytes and its spec says %d",
				want[0], info.Config.MaxBytes, running.spec.MaxBytes)
		}
		if _, err := stream.Consumer(t.Context(), running.consumer.Name()); err != nil {
			t.Errorf("the %s log is applied through the consumer %q, which is not on "+
				"its stream: %v", name, running.consumer.Name(), err)
		}
	}
	// NO OTHER STREAM OF A DOMAIN'S: every stream on the broker that names
	// one of the state logs' families is one of the four, so nothing beside
	// them holds a second copy of a domain's records.
	logStreams := []string{}
	for _, names := range declared {
		logStreams = append(logStreams, names[0])
	}
	names := js.StreamNames(t.Context())
	for name := range names.Name() {
		family := strings.Contains(name, "TRACKER") || strings.Contains(name, "PAGES") ||
			strings.Contains(name, "USAGE")
		if family && !slices.Contains(logStreams, name) {
			t.Errorf("the node created %s beside the state logs %v", name, logStreams)
		}
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the broker's streams: %v", err)
	}

	// THE REGISTER ROW, keyed by the domains: as written, the row's fields
	// are exactly the fields listed here, which a peer build reads.
	s.publishPositions(t.Context())
	rows, err := e.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	var mine *coord.NodePositions
	for i := range rows {
		if rows[i].NodeID == e.id {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatalf("the heartbeat published no row for %s: %+v", e.id, rows)
	}
	rowFields := []string{"at", "domains", "engine_version", "node_id",
		"snapshot_bytes", "snapshot_skip"}
	for _, key := range jsonKeys(t, *mine) {
		if !slices.Contains(rowFields, key) {
			t.Errorf("the node's row carries %q, which the listed fields do not: %v",
				key, rowFields)
		}
	}
	// AND ITS DUTIES AND ITS ARTEFACTS ARE WHERE THEY ARE DECLARED: the trim
	// and the embedding are one fleet singleton each, on the lease named
	// here, which the next build must claim too — so two builds sharing a
	// fleet in a rolling upgrade contend for ONE lease rather than both
	// running the duty — and the snapshots are in their directory.
	for duty, resource := range map[string]string{
		retentionDutyName: "worker:retention", embedDutyName: "worker:embeddings",
	} {
		claim := e.workerDuty(duty, retentionDutyTTL)
		if claim == nil {
			t.Fatalf("the %s duty has no claim on a node running workers in a fleet", duty)
		}
		if mine, err := claim(t.Context()); err != nil || !mine {
			t.Fatalf("the lone node claimed the %s duty = (%v, %v), want it held", duty, mine, err)
		}
		lease, err := e.backends.Coord.Get(t.Context(), resource)
		if err != nil || lease == nil || lease.Owner != e.node.Owner() {
			t.Errorf("the %s duty holds no lease of this node's at %s: (%+v, %v)",
				duty, resource, lease, err)
		}
	}
	root := e.boot.Store.SnapshotDirFor()
	deps := s.donorDeps(root, nil)
	if got, want := deps.Path(statelog.Manifest{Artifact: "snapshot-1-1.db"}),
		filepath.Join(root, "snapshot-1-1.db"); got != want {
		t.Errorf("the donor streams its artefacts from %s, and they are in %s", got, want)
	}
	var keys []string
	for key := range mine.Domains {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	sortedRegistered := slices.Sorted(slices.Values(registered))
	if !slices.Equal(keys, sortedRegistered) {
		t.Errorf("the row names the logs %v, and the register declares %v", keys, sortedRegistered)
	}

	// THE FLOOR THE TRIM PUBLISHES: under the domain's key, carrying the
	// fields listed here and nothing else.
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: e.id,
		cfg: config.TrackerRetention{MinAgeRaw: "1ns"}}
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	running := s.Domain(tracker.Domain{}.Name())
	if err := r.domain(t.Context(), running, shared); err != nil {
		t.Fatalf("the tracker's tick: %v", err)
	}
	floors, err := e.backends.Fleet.Floors(t.Context())
	if err != nil {
		t.Fatalf("read the floors: %v", err)
	}
	var found bool
	for _, f := range floors {
		if f.Domain == (tracker.Domain{}).Name() {
			found = true
			floorFields := []string{"at", "blocked_by", "blocked_since", "by", "domain",
				"floor", "generation", "terms", "trim_to"}
			for _, key := range jsonKeys(t, f) {
				if !slices.Contains(floorFields, key) {
					t.Errorf("the tracker's floor carries %q, which a floor does not: %v",
						key, floorFields)
				}
			}
		}
	}
	if !found {
		t.Errorf("the tick published no floor under the key %q: %+v", tracker.Domain{}.Name(), floors)
	}
}

// jsonKeys is the top-level keys v is written with, sorted.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return slices.Sorted(maps.Keys(fields))
}

// lastSeq is a stream's last sequence on the broker.
func lastSeq(t *testing.T, js natsjs.JetStream, stream string) uint64 {
	t.Helper()
	st, err := js.Stream(t.Context(), stream)
	if err != nil {
		t.Fatalf("the stream %s: %v", stream, err)
	}
	info, err := st.Info(t.Context())
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	return info.State.LastSeq
}

// runsLogs makes running every log s runs, in the order given — for a case
// that builds a state log by hand rather than booting one.
func runsLogs(s *stateLog, running ...*runningLog) *stateLog {
	s.logs = newLogSet(running)
	return s
}
