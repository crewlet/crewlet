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

	"github.com/crewlet/crewlet/internal/code"
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

	"usage.measured.encoded": "a usage page's rows as Fill measured them: the serving node " +
		"writes them AS the page's Rows (usage.Page.AppendJSON), and the asker decodes Rows",

	"pages.Summary.savedID": "named by the listing and spent by the same read (attachSaves); " +
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
	"knowledge.linked_from": "reads the links the lexical index derives from the " +
		"bodies it has read, which no log position describes",
	"estate.ping": "asks whether the seat copy admits a seat, its own stricter " +
		"gate, not whether it has reached the asker's writes",
	"code.git_read": "reads a git node's cache, not a domain's copy (GitCopy): the asker's " +
		"floor travels in the query, as the cache's sync level, and an object id names " +
		"the same object at every position",
	"code.git_write": "builds over a git node's cache, not a domain's copy (GitCopy): the " +
		"asker's floor travels in the write, as the cache's sync level, and the push it " +
		"lands is decided by the repository's committer, whose proposal carries the floor " +
		"its builder read",

	"usage.spend": "reads the usage domain, whose log no write of the asker's lands " +
		"on: every node publishes its own days, so there is no position to see",
	"usage.seat_turns":        "reads the usage domain, as usage.spend does",
	"usage.person_spend":      "reads the usage domain, as usage.spend does",
	"usage.seat_days":         "reads the usage domain, as usage.spend does",
	"usage.schedule_runs":     "reads the usage domain, as usage.spend does",
	"usage.page_reads":        "reads the usage domain, as usage.spend does",
	"usage.reads_elided":      "reads the usage domain, as usage.spend does",
	"usage.repo_reads":        "reads the usage domain, as usage.spend does",
	"usage.repo_reads_elided": "reads the usage domain, as usage.spend does",
}

// EVERY OPERATION SAYS WHICH LOG ITS FLOORS ARE ON: an operation that carries a
// floor names its domain, the domain is one whose log a floor can be on, and
// the stream it waits on is that domain's own log — the tracker's
// CREWLET_TRACKER_LOG, the knowledge base's CREWLET_PAGES_LOG, the code host's
// CREWLET_CODE_LOG, the identity directory's CREWLET_IAM_LOG or the artifacts
// estate's and its datastore's, each named as its domain declares it. One
// that waited on none would carry no floor at all, and one that named another
// domain's would wait on an applier it does not depend on ([ready]).
func TestEveryOperationSaysWhichLogItsFloorsAreOn(t *testing.T) {
	t.Parallel()
	logs := map[string]string{
		trackerDomain:      "CREWLET_TRACKER_LOG",
		pagesDomain:        "CREWLET_PAGES_LOG",
		codeDomain:         "CREWLET_CODE_LOG",
		identityDomain:     "CREWLET_IAM_LOG",
		artifactsDomain:    "CREWLET_ARTIFACTS_LOG",
		artifactDataDomain: "CREWLET_ARTIFACT_DATA_LOG",
	}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		_, listed := floorless[name]
		switch {
		case spec.floorStream == "" && !listed:
			t.Errorf("%s carries no floor: declare the domain whose log it reads, or "+
				"name why no floor holds it in floorless", name)
		case spec.floorStream != "" && listed:
			t.Errorf("floorless lists %s, which carries a floor on %s", name, spec.floorStream)
		case listed:
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

// seatCopyOps is every operation that asks about the copy a SEAT is served by
// ([SeatCopy]) rather than one domain's, and why.
//
// TWO-SIDED, like [floorless]: an operation naming the seat copy that is not
// listed fails — it would run on a copy whose own domain the verdict never
// asked about — and a listed one that names a domain fails too.
var seatCopyOps = map[string]string{
	"estate.ping": "asks whether the copy a seat is served by admits one — every " +
		"domain that gates seat admission, not one domain's",
}

// EVERY OPERATION NAMES THE COPY IT READS, because that is the verdict the
// router and the server ask before running it here: a domain's copy that is
// wrong runs none of that domain's operations and every other domain's
// (ADR-0049). An operation naming no domain — the search ops once did, since
// they carry no floor — would be asked of no domain's verdict at all: run from
// a copy out of service, or held to one it does not read. The one copy that
// is no domain's is a git node's git half ([GitCopy]), which a git operation
// alone names — one keyed on the git nodes and answered on a git lane — and no
// data operation may.
func TestEveryOperationNamesTheCopyItReads(t *testing.T) {
	t.Parallel()
	domains := []string{trackerDomain, pagesDomain, usageDomain, codeDomain,
		identityDomain, artifactsDomain, artifactDataDomain}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		_, seat := seatCopyOps[name]
		switch {
		case seat && spec.domain != SeatCopy:
			t.Errorf("seatCopyOps lists %s, which reads the %q domain's copy", name, spec.domain)
		case seat:
		case spec.domain == SeatCopy:
			t.Errorf("%s names the seat copy: declare the domain whose copy it reads, "+
				"or name why it asks about the seat's in seatCopyOps", name)
		case spec.domain == GitCopy && (!spec.git || !slices.Contains(gitLanes, spec.lane)):
			t.Errorf("%s names the git copy and is no git operation answered on a git lane: "+
				"declare it keyedOnGit and answeredOn the git lane, or name a domain", name)
		case spec.domain == GitCopy:
		case !slices.Contains(domains, spec.domain):
			t.Errorf("%s reads the %q domain's copy, want one of %v", name, spec.domain, domains)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(seatCopyOps)) {
		if _, known := registry[name]; !known {
			t.Errorf("seatCopyOps lists %s, which is no operation", name)
		}
	}
}

// readsSeveral is every operation that reads another domain's rows beside its
// own ([op.alsoReads]), with the domains it reads and why.
//
// TWO-SIDED, like [floorless]: an operation that declares a second domain and
// is not listed fails, and so does a listed one that no longer declares what
// the list says — because which copies an operation reads is what decides
// where it may run, and a second domain an operation reads and does not
// declare is one whose wrong copy it answers from.
var readsSeveral = map[string][]string{
	// The links come from both corpora's bodies, and a task names the page
	// as a linked page in the tracker's own relation rows.
	"knowledge.linked_from": {pagesDomain, trackerDomain},
}

// AN OPERATION THAT READS SEVERAL DOMAINS DECLARES EACH ONE, and is routed by
// all of them: a node whose copy of any is out of service runs none of it
// (ADR-0049), and the router's memory of where it stays is kept for that set
// of copies rather than for one domain's.
func TestAnOperationThatReadsSeveralDomainsDeclaresThem(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		got := append([]string{spec.domain}, spec.alsoReads...)
		want, listed := readsSeveral[name]
		switch {
		case len(spec.alsoReads) > 0 && !listed:
			t.Errorf("%s reads %v: name it and why in readsSeveral", name, got)
		case listed && !slices.Equal(got, want):
			t.Errorf("%s reads %v, and readsSeveral says %v", name, got, want)
		case listed && spec.copies != strings.Join(want, "+"):
			t.Errorf("%s is routed by %q, want every copy it reads", name, spec.copies)
		case !listed && spec.copies != spec.domain:
			t.Errorf("%s is routed by %q, want its domain %q", name, spec.copies, spec.domain)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(readsSeveral)) {
		if _, known := registry[name]; !known {
			t.Errorf("readsSeveral lists %s, which is no operation", name)
		}
	}
}

// THE ENVELOPE IS PINNED, BYTE FOR BYTE: one read request, one asked again
// after a node refused it, a reply that answers it, admission's ping and a
// copy out of service. A field added to the
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
		"a read asked after a node refused it": {
			value: request{ID: "r-4", Op: opTasks.spec.name, Args: json.RawMessage(`{"Query":{}}`),
				Deadline: envelopeInstant,
				Spent: &statelog.Spent{
					Index:    []statelog.Position{{Stream: trackerStream, Generation: 1, Seq: 9}},
					Waited:   []statelog.Position{{Stream: trackerStream, Generation: 1, Seq: 9}},
					NoQuorum: []string{pagesStream},
				}},
			want: `{"id":"r-4","op":"tracker.tasks","args":{"Query":{}},` +
				`"deadline":"2026-10-04T12:00:00Z","spent":{` +
				`"index":[{"stream":"CREWLET_TRACKER_LOG","generation":1,"seq":9}],` +
				`"waited":[{"stream":"CREWLET_TRACKER_LOG","generation":1,"seq":9}],` +
				`"no_quorum":["CREWLET_PAGES_LOG"]}}`,
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
		"a copy out of service": {
			value: reply{Node: "data-a", Request: "r-1", Unserved: unservedOutOfService, Detail: "wrong"},
			want:  `{"node":"data-a","request":"r-1","unserved":"out_of_service","detail":"wrong"}`,
		},
		"a git node that cannot answer": {
			value: reply{Node: "git-a", Request: "r-5", Unserved: unservedBehind, Detail: "syncing",
				Unready: code.UnreadySyncing},
			want: `{"node":"git-a","request":"r-5","unserved":"behind","detail":"syncing","unready":"syncing"}`,
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
		Result: json.RawMessage(`{"Tracker":true,"Pages":true}`)}}
	r, err := NewRouter(RouterOptions{
		Self: "agent-1", Queue: asker, Placement: &fakePlacement{nodes: []string{"data-a"}},
		Git: &fakePlacement{}, Session: NewSession(), Timing: Production(),
		Envelopes: fleetEnvelopes,
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

// selfEncoded is an answer that holds its own encoding, as a usage page does.
type selfEncoded struct{ held string }

func (s selfEncoded) AppendJSON(dst []byte) ([]byte, error) {
	return append(dst, `{"held":"`+s.held+`"}`...), nil
}

// AN ANSWER THAT HOLDS ITS OWN ENCODING IS SENT AS IT IS. A usage page's rows
// were encoded once already, to cut the page at its budget, and a reply that
// encoded the page again with json.Marshal encoded every row twice; so the
// serving half writes an answer's own encoding where it holds one, and the
// ordinary encoding of everything else.
func TestAnAnswerThatHoldsItsOwnEncodingIsSentAsItIs(t *testing.T) {
	t.Parallel()
	got, err := encodeResult(selfEncoded{held: "rows"})
	if err != nil || string(got) != `{"held":"rows"}` {
		t.Fatalf("an answer holding its encoding was sent as (%s, %v), want its own bytes", got, err)
	}
	got, err = encodeResult(scheduleRunsPage{More: true})
	if err != nil || string(got) != `{"Runs":null,"More":true}` {
		t.Fatalf("an ordinary answer was sent as (%s, %v), want its json encoding", got, err)
	}
}
