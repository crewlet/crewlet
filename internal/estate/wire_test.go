package estate

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/signedask"
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
	"worklet.Query.Units":              "the serving node attaches its own chart (opTasks)",
	"worklet.ViewQuery.Units":          "the serving node attaches its own chart (opViews)",
	"worklet.ProjectQuery.Units":       "the serving node attaches its own chart (opProjects)",
	"worklet.ProjectDetailQuery.Units": "the serving node attaches its own chart (opProject)",
	"worklet.DetailWants.Units":        "the serving node attaches its own chart (opTask)",
	"worklet.WorkloadQuery.Units":      "the serving node attaches its own chart (opWorkload)",
	"worklet.EveryViewQuery.Units":     "the serving node attaches its own chart (opEveryView)",

	"worklet.TaskPatch.Watch":     "carried beside the patch, in updateTaskArgs",
	"worklet.TaskPatch.Relate":    "carried beside the patch, in updateTaskArgs",
	"worklet.TaskPatch.Depend":    "carried beside the patch, in updateTaskArgs",
	"worklet.TaskPatch.Checklist": "carried beside the patch, in updateTaskArgs",

	"worklet.DayClock.Zone":       "carried beside the wants by name, in taskArgs",
	"worklet.ViewQuery.Zone":      "carried beside the query by name, in viewsArgs",
	"worklet.EveryViewQuery.Zone": "carried beside the query by name, in everyViewArgs",

	"worklet.Total.instant": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"worklet.Total.rank": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"worklet.Total.ranked": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"worklet.Total.value": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",
	"worklet.Total.rankArgs": "decided when the total is compiled and spent by the scan; " +
		"an answer carries its result as At or Value",

	"worklet.PlaceResult.Unplaced": "carried beside the result as a wire error, in placedTask",

	"worklet.Provenance.Written": "the asking turn's own set: a holder that runs the write " +
		"answers what it committed to in reply.Written, and the router adds it there",

	"doclet.Summary.savedID": "named by the listing and spent by the same read (attachSaves); " +
		"an answer carries its result as SavedBy and SavedAt",
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
// CREWLET_TRACKER_LOG, the knowledge base's CREWLET_PAGES_LOG, the identity
// directory's CREWLET_IAM_LOG, chat's CREWLET_CHAT_LOG or the executor's
// CREWLET_EXECUTOR_LOG among them. One that named none would carry no floor at
// all, and one that named another domain's would wait on an applier it does not
// depend on ([ready]).
func TestEveryOperationSaysWhichLogItsFloorsAreOn(t *testing.T) {
	t.Parallel()
	logs := map[string]string{
		trackerDomain:      "CREWLET_TRACKER_LOG",
		pagesDomain:        "CREWLET_PAGES_LOG",
		identityDomain:     "CREWLET_IAM_LOG",
		artifactsDomain:    "CREWLET_ARTIFACTS_LOG",
		artifactDataDomain: "CREWLET_ARTIFACT_DATA_LOG",
		chatDomain:         "CREWLET_CHAT_LOG",
		executorDomain:     "CREWLET_EXECUTOR_LOG",
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
// a fleet reads of every other, and this is where that shows — rather than in
// a router that silently reads a field its peer stopped sending. What is pinned
// is the BODY a signature covers ([Envelopes]); the frame around it is the
// state log's own, pinned there.
func TestTheEnvelopeIsWhatThisBuildSends(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"a read request": {
			value: request{ID: "r-1", Op: opTasks.spec.name, Args: json.RawMessage(`{"Query":{}}`),
				Floors:   []statelog.Position{{Stream: trackerStream, Generation: 1, Seq: 7}},
				Deadline: envelopeInstant, From: "agent-1"},
			want: `{"id":"r-1","op":"tracker.tasks","args":{"Query":{}},` +
				`"floors":[{"stream":"CREWLET_TRACKER_LOG","generation":1,"seq":7}],` +
				`"deadline":"2026-10-04T12:00:00Z","from":"agent-1"}`,
		},
		"a write's last resort": {
			value: request{ID: "r-2", Op: opCreateTask.spec.name, Actor: &swe,
				Deadline: envelopeInstant, AcceptLagging: true},
			want: `{"id":"r-2","op":"tracker.create_task","actor":{"handle":"swe","kind":"agent",` +
				`"provenance":{"OperatorID":"","TurnID":"","Chain":null,"Origin":null}},` +
				`"deadline":"2026-10-04T12:00:00Z","accept_lagging":true}`,
		},
		"admission's ping": {
			value: pingEnvelope,
			want: `{"id":"r-3","op":"estate.ping","args":{},` +
				`"deadline":"2026-10-04T12:00:00Z","from":"agent-1"}`,
		},
		"an answer": {
			value: reply{Node: "data-a", Request: "r-1", Result: json.RawMessage(`{"TotalHint":7}`)},
			want:  `{"node":"data-a","request":"r-1","result":{"TotalHint":7}}`,
		},
		// THE HALVES A COPY SERVES, as admission reads them off every data
		// node it asks: a half renamed here is a half every peer reads as
		// not served, and no seat of that company is admitted anywhere.
		"admission's answer": {
			value: reply{Node: "data-a", Request: "r-3",
				Result: mustJSON(t, Served{Tracker: true, Pages: true, Chat: true})},
			want: `{"node":"data-a","request":"r-3",` +
				`"result":{"Tracker":true,"Pages":true,"Chat":true}}`,
		},
		"a copy out of service": {
			value: reply{Node: "data-a", Request: "r-1", Unserved: unservedOutOfService, Detail: "wrong"},
			want:  `{"node":"data-a","request":"r-1","unserved":"out_of_service","detail":"wrong"}`,
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

// mustJSON is v encoded, for an envelope whose result is a type's own value.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// envelopeInstant is the deadline the pinned envelopes carry.
var envelopeInstant = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// pingEnvelope is admission's ping as the envelope golden pins it.
var pingEnvelope = request{ID: "r-3", Op: opPing.spec.name, Args: json.RawMessage(`{}`),
	Deadline: envelopeInstant, From: "agent-1"}

// WHAT THE ROUTER ACTUALLY SENDS FOR ADMISSION'S PING is the golden above, read
// off the wire rather than built by hand: the envelope test pins a request a
// test spelled, and only this one fails when [Router.Serves] starts sending
// arguments a data node would read differently — a partition, a layout,
// anything at all beyond the empty object.
func TestAdmissionsPingIsTheOneTheEnvelopePins(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{answer: reply{Node: "data-a",
		Result: json.RawMessage(`{"Tracker":true,"Pages":true,"Chat":true}`)}}
	r, err := NewRouter(RouterOptions{
		Self: "agent-1", Queue: asker, Placement: &fakePlacement{nodes: []string{"data-a"}},
		Session: NewSession(), Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	served, err := r.Serves(t.Context())
	if err != nil || served != (Served{Tracker: true, Pages: true, Chat: true}) {
		t.Fatalf("Serves = (%+v, %v), want every half the answer named served", served, err)
	}
	sent := asker.requests()
	if len(sent) != 1 {
		t.Fatalf("admission sent %d requests, want exactly one", len(sent))
	}
	if sent[0].subject != Subject("data-a") {
		t.Errorf("admission asked %q, want the data node's own subject %q",
			sent[0].subject, Subject("data-a"))
	}
	admitted := fleetEnvelopes.admit(sent[0].payload, signedask.NewGuard(), time.Now())
	if admitted.Disposition != signedask.Admitted {
		t.Fatalf("the ping does not open as a request the fleet signed: %s (%s)",
			admitted.Disposition, admitted.Why)
	}
	req := admitted.Request
	// THE ID AND THE DEADLINE ARE THE ATTEMPT'S, which move with every ask
	// and the clock: each must be there, and everything else is compared
	// byte for byte against the golden.
	if req.ID == "" {
		t.Error("the ping carries no id, so its reply could be anybody's")
	}
	if req.Deadline.IsZero() {
		t.Error("the ping carries no deadline, so a data node keeps working on an " +
			"answer nobody is waiting for")
	}
	req.ID, req.Deadline = pingEnvelope.ID, pingEnvelope.Deadline
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(pingEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Errorf("the router's ping encodes as\n%s\nwant the envelope's\n%s", raw, want)
	}
}

// recordingAsker answers every request with one reply — signed by the fleet
// and bound to the request it answers — and keeps what it was sent, and where.
type recordingAsker struct {
	answer reply

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
	req := opened(payload)
	if req.ID == "" {
		return nil, errors.New("a request that does not open as the fleet's")
	}
	answer := a.answer
	answer.Request = req.ID
	return [][]byte{fleetEnvelopes.sealReply(answer)}, nil
}

func (a *recordingAsker) requests() []sentRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}
