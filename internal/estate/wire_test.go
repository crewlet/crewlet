package estate

import (
	"encoding"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

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
		if spec.part != nil {
			walk(spec.part)
		}
	}
	// AND THE ENVELOPE EVERY ONE OF THEM TRAVELS IN — a gather's per-
	// partition reply included — since a field of the request or the reply
	// that does not cross loses every operation's answer at once.
	for _, envelope := range []reflect.Type{
		reflect.TypeFor[request](), reflect.TypeFor[reply](), reflect.TypeFor[partReply](),
	} {
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
		for _, ty := range []reflect.Type{spec.args, spec.result, spec.part} {
			if ty == nil {
				continue
			}
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

// AN OLDER ASKER'S REQUEST IS RESOLVED, NEVER REFUSED AS MALFORMED.
//
// A rolling upgrade puts a build from before partitions in front of this one,
// on the one layout the two can share — layout 0; the protocol version fences
// a divided one — and such a build names no partition and sends its own
// arguments, which lack every field this build added. The server resolves that
// request by the operation's arguments under its own layout, so each operation
// must resolve arguments that carry NOTHING, the least an older build's can
// carry, to the one partition: an operation whose partition function requires
// a field of its own refuses every older asker, and an older asker takes the
// refusal as final. Admission's ping did exactly that, and withheld every seat
// claim on every stateless node not yet upgraded.
func TestEveryOperationResolvesAnOlderAskersRequest(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		if spec.partitions == nil {
			continue
		}
		parts, err := spec.partitions(t.Context(), layoutZero, layoutResolver{layout: layoutZero},
			json.RawMessage(`{}`))
		if err != nil || !slices.Equal(parts, []statelog.PartitionID{statelog.EstatePartition}) {
			t.Errorf("%s: a request with none of this build's fields resolves to (%v, %v), "+
				"want %s", name, parts, err, statelog.EstatePartition)
		}
	}
}

// EVERY OPERATION SAYS WHICH LOG ITS FLOORS ARE ON: an operation that
// addresses a partition and carries a floor names its domain, and the domain
// is one a partition's logs belong to. One that named none would carry no
// floor at all — reading from before the asker's own writes with nothing to
// show for it — and one that named another domain's would wait on an applier
// it does not depend on ([address]).
func TestEveryOperationSaysWhichLogItsFloorsAreOn(t *testing.T) {
	t.Parallel()
	domains := []string{trackerDomain, pagesDomain, vectorsDomain}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		if spec.partitions == nil || spec.floorless {
			continue
		}
		if !slices.Contains(domains, spec.domain) {
			t.Errorf("%s carries a floor and names the domain %q, want one of %v",
				name, spec.domain, domains)
			continue
		}
		if streams := spec.floorStreams(layoutZero, statelog.EstatePartition); len(streams) != 1 {
			t.Errorf("%s's floors at layout 0 are on %v, want its own domain's one log", name, streams)
		}
	}
}
