package estate

import (
	"context"
	"encoding"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// carriedByServer is every field an operation's types hold that the wire does
// NOT carry, and why that is right — who supplies it on the far side, or why
// no value that crosses ever holds one.
//
// TWO-SIDED: a field found and not listed fails, and a listed field no
// operation carries any more fails too, so the list is never a record of a
// type that has since changed.
var carriedByServer = map[string]string{
	"tracker.Query.Units":              "the serving node attaches its own chart (opTasks)",
	"tracker.ViewQuery.Units":          "the serving node attaches its own chart (opViews)",
	"tracker.ProjectQuery.Units":       "the serving node attaches its own chart (opProjects)",
	"tracker.ProjectDetailQuery.Units": "the serving node attaches its own chart (opProject)",
	"tracker.DetailWants.Units":        "the serving node attaches its own chart (opTask)",
	"tracker.WorkloadQuery.Units":      "the serving node attaches its own chart (opWorkload)",
	"tracker.EveryViewQuery.Units":     "the serving node attaches its own chart (opEveryView)",

	"tracker.TaskPatch.Watch":     "carried beside the patch, in updateTaskArgs",
	"tracker.TaskPatch.Relate":    "carried beside the patch, in updateTaskArgs",
	"tracker.TaskPatch.Depend":    "carried beside the patch, in updateTaskArgs",
	"tracker.TaskPatch.Promote":   "carried beside the patch, in updateTaskArgs",
	"tracker.TaskPatch.Checklist": "carried beside the patch, in updateTaskArgs",

	"tracker.DayClock.Zone":       "carried beside the wants by name, in taskArgs",
	"tracker.ViewQuery.Zone":      "carried beside the query by name, in viewsArgs",
	"tracker.EveryViewQuery.Zone": "carried beside the query by name, in everyViewArgs",

	"tracker.Total.instant": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"tracker.Total.rank": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"tracker.Total.ranked": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"tracker.Total.value": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"tracker.Total.rankArgs": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",

	"tracker.PlaceResult.Unplaced": "carried beside the result as a wire error, in placedTask",

	"tracker.Provenance.Written": "the asking turn's own set: a holder that runs the write " +
		"answers what it committed to in reply.Written, and the router adds it there",
}

// extraField is the one field name every record type keeps a newer build's
// unknown fields in. It is listed once rather than per type: none of it is
// ever set on a value an operation carries — the tables have no column for
// it, so an answer read from rows holds none, and a caller building a task or
// a patch has no unknown fields to set.
const extraField = "Extra"

// uncarried is every field reachable from an operation's types that the wire
// would not carry, as "<package>.<Type>.<Field>".
func uncarried(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice ||
			ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
			if ty.Kind() == reflect.Map {
				key := ty.Key()
				if key.Kind() != reflect.String &&
					!key.Implements(reflect.TypeFor[encoding.TextMarshaler]()) {
					found[ty.String()] = "a map keyed by " + key.String()
				}
			}
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		if ty.Implements(reflect.TypeFor[json.Marshaler]()) ||
			reflect.PointerTo(ty).Implements(reflect.TypeFor[json.Marshaler]()) {
			return
		}
		for i := range ty.NumField() {
			f := ty.Field(i)
			name := ty.String() + "." + f.Name
			switch {
			case f.Anonymous && !f.IsExported():
				walk(f.Type)
			case !f.IsExported():
				found[name] = "unexported"
			case f.Tag.Get("json") == "-":
				if f.Name != extraField {
					found[name] = `json:"-"`
				}
			case f.Type.Kind() == reflect.Interface && f.Type.NumMethod() > 0:
				found[name] = "an interface"
			case f.Type.Kind() == reflect.Func || f.Type.Kind() == reflect.Chan:
				found[name] = "a " + f.Type.Kind().String()
			default:
				walk(f.Type)
			}
		}
	}
	for _, spec := range registry {
		walk(spec.args)
		walk(spec.result)
	}
	// AND THE ENVELOPE EVERY ONE OF THEM TRAVELS IN, since a field of the
	// request or the reply that does not cross loses every operation's
	// answer at once.
	for _, envelope := range []reflect.Type{reflect.TypeFor[request](), reflect.TypeFor[reply]()} {
		walk(envelope)
	}
	return found
}

// NOTHING AN OPERATION CARRIES IS SILENTLY LEFT BEHIND. A field that does not
// arrive answers as its zero value on the far side, which is a filter that
// matches everything or a flag that was never set — and neither says so.
func TestEveryFieldAnOperationCarriesArrives(t *testing.T) {
	t.Parallel()
	found := uncarried(t)
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, listed := carriedByServer[name]; !listed {
			t.Errorf("%s is %s and would not cross the wire: carry it, or name "+
				"who supplies it in carriedByServer", name, found[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(carriedByServer)) {
		if _, still := found[name]; !still {
			t.Errorf("carriedByServer lists %s, which no operation carries any more", name)
		}
	}
}

// EVERY OPERATION ROUND-TRIPS ITS OWN ZERO VALUE, so a type the decoder
// cannot fill — an interface field, a map it cannot key — fails here rather
// than on the first request that carries one.
func TestEveryOperationsTypesDecode(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		for _, ty := range []reflect.Type{spec.args, spec.result} {
			raw, err := json.Marshal(reflect.New(ty).Elem().Interface())
			if err != nil {
				t.Errorf("%s: encode a zero %s: %v", name, ty, err)
				continue
			}
			if err := json.Unmarshal(raw, reflect.New(ty).Interface()); err != nil {
				t.Errorf("%s: decode a zero %s: %v", name, ty, err)
			}
		}
		if !strings.Contains(name, ".") {
			t.Errorf("%s: an operation is named <half>.<verb>", name)
		}
	}
}

// floorless is every operation that carries no floor, and why: what it reads
// is not a log's rows at a position, so no floor could hold its answer to
// anything.
//
// TWO-SIDED, like [carriedByServer]: an operation that declares no floor and
// is not listed fails — one that forgot its domain would read from before the
// asker's own writes with nothing to show for it — and a listed one that now
// declares a floor fails too.
var floorless = map[string]string{
	"tracker.search": "reads the lexical and semantic indexes, which each node " +
		"maintains behind its applier on its own schedule",
	"knowledge.search": "reads the indexes, as tracker.search does",
	"knowledge.building": "asks whether the lexical index has caught up, which no " +
		"log position describes",
	"estate.ping": "asks whether the copy admits a seat, its own stricter gate, " +
		"not whether it has reached the asker's writes",
}

// EVERY OPERATION SAYS WHICH LOG ITS FLOORS ARE ON: an operation that carries a
// floor names its domain, the domain is one whose log a floor can be on, and
// the stream it waits on is that domain's own log — the tracker's
// CREWLET_TRACKER_LOG or the knowledge base's CREWLET_PAGES_LOG, named as they
// have always been. One that named none would carry no floor at all, and one
// that named another domain's would wait on an applier it does not depend on
// ([ready]).
func TestEveryOperationSaysWhichLogItsFloorsAreOn(t *testing.T) {
	t.Parallel()
	logs := map[string]string{
		trackerDomain: "CREWLET_TRACKER_LOG",
		pagesDomain:   "CREWLET_PAGES_LOG",
	}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		_, listed := floorless[name]
		switch {
		case spec.domain == "" && !listed:
			t.Errorf("%s carries no floor: declare the domain whose log it reads, or "+
				"name why no floor holds it in floorless", name)
		case spec.domain != "" && listed:
			t.Errorf("floorless lists %s, which carries a floor on the %s log", name, spec.domain)
		case spec.domain == "":
			if spec.floorStream != "" {
				t.Errorf("%s carries no floor and still names the stream %q", name, spec.floorStream)
			}
		case logs[spec.domain] == "":
			t.Errorf("%s carries a floor on the %q domain's log, want one of %v",
				name, spec.domain, slices.Sorted(maps.Keys(logs)))
		case spec.floorStream != logs[spec.domain]:
			t.Errorf("%s waits on %q, want its own domain's log %q",
				name, spec.floorStream, logs[spec.domain])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(floorless)) {
		if _, known := registry[name]; !known {
			t.Errorf("floorless lists %s, which is no operation", name)
		}
	}
}

// THE ENVELOPE IS PINNED, BYTE FOR BYTE: one read request, a reply that answers
// it, admission's ping and a copy out of service. A field added to the
// envelope, renamed or made to send its zero value changes what every node of
// a fleet mid-upgrade reads of every other, and this is where that shows —
// rather than in a router that silently reads a field its peer stopped
// sending.
func TestTheEnvelopeIsWhatThisBuildSends(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"a read request": {
			value: request{Op: opTasks.spec.name, Args: json.RawMessage(`{"Query":{}}`),
				Floors:   []statelog.Position{{Stream: trackerStream, Generation: 1, Seq: 7}},
				Deadline: at, From: "agent-1"},
			want: `{"op":"tracker.tasks","args":{"Query":{}},` +
				`"floors":[{"stream":"CREWLET_TRACKER_LOG","generation":1,"seq":7}],` +
				`"deadline":"2026-10-04T12:00:00Z","from":"agent-1"}`,
		},
		"a write's last resort": {
			value: request{Op: opCreateTask.spec.name, Actor: &swe, AcceptLagging: true},
			want: `{"op":"tracker.create_task","actor":{"handle":"swe","kind":"agent",` +
				`"provenance":{"OperatorID":"","Seat":"","TurnID":"","Chain":null,` +
				`"Origin":null}},"accept_lagging":true}`,
		},
		"admission's ping": {
			value: request{Op: opPing.spec.name, Args: json.RawMessage(`{}`), From: "agent-1"},
			want:  `{"op":"estate.ping","args":{},"from":"agent-1"}`,
		},
		"an answer": {
			value: reply{Node: "data-a", Result: json.RawMessage(`{"TotalHint":7}`)},
			want:  `{"node":"data-a","result":{"TotalHint":7}}`,
		},
		"a copy out of service": {
			value: reply{Node: "data-a", Unserved: unservedOutOfService, Detail: "wrong"},
			want:  `{"node":"data-a","unserved":"out_of_service","detail":"wrong"}`,
		},
	} {
		raw, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(raw) != tc.want {
			t.Errorf("%s encodes as\n%s\nwant\n%s", name, raw, tc.want)
		}
	}
}

// WHAT THE ROUTER ACTUALLY SENDS FOR ADMISSION'S PING is the golden above, read
// off the wire rather than built by hand: the envelope test pins a request a
// test spelled, and only this one fails when [Router.Serves] starts sending
// arguments a data node of the previous build would read differently — a
// partition, a layout, anything at all beyond the empty object.
func TestAdmissionsPingIsTheOneTheEnvelopePins(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{answer: []byte(`{"node":"data-a","result":{"Tracker":true,"Pages":true}}`)}
	r, err := NewRouter(RouterOptions{
		Self: "agent-1", Queue: asker, Placement: &fakePlacement{nodes: []string{"data-a"}},
		Session: NewSession(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker, pages, err := r.Serves(t.Context())
	if err != nil || !tracker || !pages {
		t.Fatalf("Serves = (%v, %v, %v), want both halves served", tracker, pages, err)
	}
	sent := asker.requests()
	if len(sent) != 1 {
		t.Fatalf("admission sent %d requests, want exactly one", len(sent))
	}
	if sent[0].subject != Subject("data-a") {
		t.Errorf("admission asked %q, want the data node's own subject %q",
			sent[0].subject, Subject("data-a"))
	}
	var req request
	if err := json.Unmarshal(sent[0].payload, &req); err != nil {
		t.Fatalf("the ping does not decode as a request: %v\n%s", err, sent[0].payload)
	}
	// THE DEADLINE IS THE ATTEMPT'S, which moves with the clock: everything
	// else is compared byte for byte against the golden.
	if req.Deadline.IsZero() {
		t.Error("the ping carries no deadline, so a data node keeps working on an " +
			"answer nobody is waiting for")
	}
	req.Deadline = time.Time{}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"op":"estate.ping","args":{},"from":"agent-1"}`; string(raw) != want {
		t.Errorf("the router's ping encodes as\n%s\nwant the envelope's\n%s", raw, want)
	}
}

// recordingAsker answers every request with one reply and keeps what it was
// sent, and where.
type recordingAsker struct {
	answer []byte

	mu   sync.Mutex
	sent []sentRequest
}

// sentRequest is one request a [recordingAsker] was handed.
type sentRequest struct {
	subject string
	payload []byte
}

func (a *recordingAsker) Ask(_ context.Context, subject string, payload []byte, _ int) ([][]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, sentRequest{subject: subject, payload: slices.Clone(payload)})
	return [][]byte{a.answer}, nil
}

func (a *recordingAsker) requests() []sentRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}
