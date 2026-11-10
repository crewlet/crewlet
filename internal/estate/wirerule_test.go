package estate

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
)

// wireInner, wirePromoted and wireArgs are an operation's arguments as the
// cases below need them: a nested struct, a list of them, a pointer to one, a
// struct embedded for its promoted fields, a map whose keys are data, a raw
// document that is data, and a field the wire never carries.
type (
	wireInner struct {
		Note  string `json:"note"`
		Count int
	}
	wirePromoted struct{ Zone string }
	wireArgs     struct {
		wirePromoted
		Name   string
		Inner  wireInner
		List   []wireInner
		Ptr    *wireInner
		ByName map[string]wireInner
		Raw    json.RawMessage
		Hidden string `json:"-"`
	}
)

// AN INPUT CROSSES WITH EVERY FIELD AT ITS ZERO LEFT OFF, and is otherwise
// json.Marshal's, byte for byte: a field a later build adds then reaches a data
// node of an earlier build only where a write sets it, and a write that does
// not is served by either build exactly as it was before the field existed.
//
// Mutation: drop OmitZeroStructFields from inputEncoding and the zero value
// goes on the wire as every one of its keys.
func TestAnInputLeavesEveryFieldAtItsZeroOffTheWire(t *testing.T) {
	t.Parallel()
	zero, err := encodeInput(wireArgs{})
	if err != nil || string(zero) != `{}` {
		t.Fatalf("a zero input crosses as (%s, %v), want {} — no key at all", zero, err)
	}
	set := wireArgs{wirePromoted: wirePromoted{Zone: "z"}, Name: "n", Inner: wireInner{Note: "<b>&"},
		List: []wireInner{{Count: 2}}, ByName: map[string]wireInner{"k": {}}, Raw: json.RawMessage(`[1]`)}
	got, err := encodeInput(set)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Zone":"z","Name":"n","Inner":{"note":"\u003cb\u003e\u0026"},"List":[{"Count":2}],` +
		`"ByName":{"k":{}},"Raw":[1]}`
	if string(got) != want {
		t.Errorf("an input crosses as\n%s\nwant json.Marshal's encoding of every value it holds\n%s", got, want)
	}
}

// A BASELINE TOLD RECORD CROSSES THE WIRE AS ITSELF. A seat's first told record
// carries no changes, and its validation refuses a baseline that tells one. Sent
// as json.Marshal writes it, a nil json.RawMessage crossed as `null`, which the
// serving node decoded as the four bytes "null" — so every baseline a node
// without data recorded was refused there as a baseline that tells a change,
// and the seat was told its whole access afresh on every turn. Left off the
// wire at its zero, it arrives nil, as it left.
//
// Mutation: drop OmitZeroStructFields from inputEncoding and Changes arrives
// as "null".
func TestABaselineToldRecordCrossesTheWireAsItself(t *testing.T) {
	t.Parallel()
	told := accessdomain.NewTold{Handle: "swe", Turn: "019a0c2e-0000-7000-8000-000000000001",
		Digest: "sha256:" + strings.Repeat("0", 64), Snapshot: json.RawMessage(`{"v":1}`), Baseline: true}
	raw, err := encodeInput(accessTellArgs{Told: told})
	if err != nil {
		t.Fatal(err)
	}
	// AS THE SERVING NODE DECODES AN OPERATION'S ARGUMENTS ([define]).
	var args accessTellArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("the serving node read the told record as %v", err)
	}
	if got := args.Told; got.Changes != nil || !got.Baseline {
		t.Errorf("a baseline crossed carrying changes %q, want none — the serving node refuses a "+
			"baseline that tells one", got.Changes)
	}
}

// inputRoots is every type the wire's rule for an operation's arguments
// governs: each operation's arguments.
func inputRoots() map[reflect.Type]string {
	roots := map[reflect.Type]string{}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		if _, held := roots[spec.args]; !held {
			roots[spec.args] = name + "'s arguments"
		}
	}
	return roots
}

// inputTypes is every type reachable from [inputRoots] the way the encoder
// reaches one — through a pointer, a slice, an array and a map's values, and
// every field it carries — with the root it was reached from. A type that
// writes its own encoding is reached, and nothing inside it.
func inputTypes() map[reflect.Type]string {
	found := map[reflect.Type]string{}
	var walk func(ty reflect.Type, from string)
	walk = func(ty reflect.Type, from string) {
		if _, seen := found[ty]; seen {
			return
		}
		found[ty] = from
		if ty.Implements(reflect.TypeFor[json.Marshaler]()) ||
			reflect.PointerTo(ty).Implements(reflect.TypeFor[json.Marshaler]()) {
			return
		}
		switch ty.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(ty.Elem(), from)
		case reflect.Map:
			walk(ty.Elem(), from)
		case reflect.Struct:
			for field := range ty.Fields() {
				if (field.IsExported() || field.Anonymous) && field.Tag.Get("json") != "-" {
					walk(field.Type, from)
				}
			}
		}
	}
	roots := inputRoots()
	for _, root := range slices.SortedFunc(maps.Keys(roots), func(a, b reflect.Type) int {
		return strings.Compare(a.String(), b.String())
	}) {
		walk(root, roots[root])
	}
	return found
}

// EVERY FIELD AN OPERATION TAKES LEAVES THE WIRE AT ITS ZERO — the arguments of
// every operation, at every depth: a key goes on the wire only where it carries
// a value, so a field a later build adds reaches a data node of an earlier
// build only in a write that sets it. What a field's zero means is [define]'s
// rule: what a build without the field does.
//
// Mutation: drop OmitZeroStructFields from inputEncoding and every field of
// every type is named.
func TestEveryFieldAnOperationTakesLeavesTheWireAtItsZero(t *testing.T) {
	t.Parallel()
	types := inputTypes()
	structs := 0
	for _, ty := range slices.SortedFunc(maps.Keys(types), func(a, b reflect.Type) int {
		return strings.Compare(a.String(), b.String())
	}) {
		if ty.Kind() != reflect.Struct || ty.Implements(reflect.TypeFor[json.Marshaler]()) ||
			reflect.PointerTo(ty).Implements(reflect.TypeFor[json.Marshaler]()) {
			continue
		}
		structs++
		raw, err := encodeInput(reflect.New(ty).Elem().Interface())
		if err != nil {
			t.Errorf("%s (from %s) does not encode at its zero: %v", ty, types[ty], err)
			continue
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			t.Errorf("%s (from %s) encodes as %s, which is not an object", ty, types[ty], raw)
			continue
		}
		for _, member := range slices.Sorted(maps.Keys(members)) {
			t.Errorf("%s.%s (from %s) goes on the wire at its zero, as %s: every write of a build "+
				"that adds it carries it to a data node that may not declare it", ty, member, types[ty],
				members[member])
		}
	}
	// A GATE THAT READ NOTHING PASSES — so it says what it read.
	if structs < 200 {
		t.Fatalf("the gate read %d struct types; it is not reading what the operations take", structs)
	}
}

// zeroByMethod is every type an operation's arguments hold that has an IsZero
// method of its own, which is what
// decides when the wire leaves it out ([inputEncoding]), and why a reader of
// its absence, which decodes Go's zero, loses nothing; and, as
// "<type>.<field>", every field such a method calls zero while it holds
// something, and why that something is nothing a reader misses.
//
// TWO-SIDED, like [carriedByServer]: a type or a field found and not listed
// fails, and one listed that nothing carries fails too.
var zeroByMethod = map[string]string{
	"time.Time": "the zero instant: a VALUE field's absence decodes as the same instant, which every " +
		"reader asks IsZero of or compares as an instant (a POINTER at it decodes as nil instead, " +
		"which zeroThroughPointer holds to a reason of its own)",
	"statelog.Position": "a position that names nothing (statelog's own definition): its absence " +
		"decodes as Go's zero, which names nothing too",
	"statelog.Position.Stream": "a zero position names nothing, so no reader judges its stream: " +
		"every reader of a position an operation takes asks IsZero, Seq or statelog's On before " +
		"its stream (accessdomain's told check by On, code's decides by IsZero, the executor log's " +
		"by Seq), and On takes a zero position on any log",
	"code.RunProvenance": "Go's own zero: its method compares the value with RunProvenance{}",
	"code.RevisionClasses": "no path classed and the revision not complete: the decide reads the " +
		"lists' lengths, so their absence is the same answer",
	"code.RevisionClasses.CommentOnly": "an empty list and none are one answer to the decide, " +
		"which reads its length",
	"code.RevisionClasses.WhitespaceOnly": "an empty list and none are one answer to the decide, " +
		"which reads its length",
	"code.HumanPathMatch": "no place held or waived: the decide reads the lists' lengths, so " +
		"their absence is the same answer",
	"code.HumanPathMatch.Held":   "an empty list and none are one answer to the decide, which reads its length",
	"code.HumanPathMatch.Waived": "an empty list and none are one answer to the decide, which reads its length",
	"objstore.Key":               "Go's own zero: its method compares the key with Key{}",
	"artifacts.ID":               "Go's own zero: its method compares the id with ID{}",
}

// isZeroer is a type with an IsZero method of its own, which the encoder asks
// in place of comparing with Go's zero.
type isZeroer interface{ IsZero() bool }

// EVERY ZERO A TYPE'S OWN METHOD DECIDES DROPS NOTHING A READER MISSES. The
// wire leaves a field out where its type's IsZero says so, and its reader
// decodes Go's zero in its place: a method that calls a value carrying
// something zero would drop that something on every routed write — so every
// such type is named with why its reader loses nothing, and every field its
// method ignores is found by setting each alone and named with why too.
//
// Mutation: delete statelog.Position.Stream from zeroByMethod and the gate
// names the stream a zero position drops.
func TestEveryZeroAMethodDecidesDropsNothingAReaderMisses(t *testing.T) {
	t.Parallel()
	found := map[string]string{}
	for ty, from := range inputTypes() {
		if !ty.Implements(reflect.TypeFor[isZeroer]()) && !reflect.PointerTo(ty).Implements(reflect.TypeFor[isZeroer]()) {
			continue
		}
		if ty.Kind() == reflect.Pointer {
			// The type it points at is found by the walk itself.
			continue
		}
		found[ty.String()] = from
		if ty.Kind() != reflect.Struct {
			continue
		}
		for i, field := range slices.Collect(ty.Fields()) {
			if !field.IsExported() {
				continue
			}
			for _, sample := range samplesOf(field.Type) {
				v := reflect.New(ty).Elem()
				v.Field(i).Set(sample)
				if v.Addr().Interface().(isZeroer).IsZero() {
					found[ty.String()+"."+field.Name] = from
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, listed := zeroByMethod[name]; !listed {
			t.Errorf("%s (from %s) is left off the wire by its own IsZero while it may hold something: "+
				"name in zeroByMethod why its reader loses nothing, or give the type a zero that "+
				"holds nothing", name, found[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(zeroByMethod)) {
		if _, still := found[name]; !still {
			t.Errorf("zeroByMethod lists %s, which no operation's arguments hold any more", name)
		}
	}
}

// zeroThroughPointer is every field an operation's arguments hold, at every
// depth, whose type is a POINTER (or an interface) at a type with an IsZero
// method of its own, as "<struct
// type>.<field>", and why no reader of it reads a pointer at what that method
// calls zero otherwise than nil: encoding/json/v2 asks the method THROUGH the
// pointer ([inputEncoding]), so such a pointer crosses as no value at all and
// the serving node decodes nil.
//
// EACH REASON NAMES EVERY READER, and is true of each — or says how no reader
// meets the pointer at the zero: a field whose readers tell the two apart is
// never listed as one they do not, but reshaped until none meets it — a clear
// spelled as a flag of its own (worklet.TaskPatch.ClearDue), a bound held as a
// value whose zero means unset (worklet.ShowClosed.Since), a value no write
// may carry refused before it is asked ([op.refusedBeforeAsking]).
//
// TWO-SIDED, like [zeroByMethod]: a field found and not listed fails, and one
// listed that nothing carries fails too.
var zeroThroughPointer = map[string]string{
	"worklet.TaskPatch.StartAt": "a date a patch sets, never the zero instant: tracker.update_task " +
		"refuses one before it is asked (worklet.TaskPatch.CheckDates) and the writer's UpdateTask " +
		"where it runs, so the applier (applyPatch), its one reader, meets nil or a real instant; " +
		"taking the date off is ClearStart, a flag that crosses as itself",
	"worklet.TaskPatch.DueAt": "a date a patch sets, never the zero instant: tracker.update_task " +
		"refuses one before it is asked (worklet.TaskPatch.CheckDates) and the writer's UpdateTask " +
		"where it runs, so the applier (applyPatch), its one reader, meets nil or a real instant; " +
		"taking the date off is ClearDue, a flag that crosses as itself",
	"worklet.Task.StartAt": "never the zero instant in a create: tracker.create_task and " +
		"tracker.create_task_asking refuse one before they are asked (worklet.Task.CheckInstants) " +
		"and the writer's createTask where it runs, so the stored task and every schedule " +
		"predicate that reads it meet nil or a real instant",
	"worklet.Task.DueAt": "never the zero instant in a create: tracker.create_task and " +
		"tracker.create_task_asking refuse one before they are asked (worklet.Task.CheckInstants) " +
		"and the writer's createTask where it runs, so the stored task and every due and overdue " +
		"predicate that reads it meet nil or a real instant",
	"worklet.Task.DoneAt": "never the zero instant in a create: tracker.create_task and " +
		"tracker.create_task_asking refuse one before they are asked (worklet.Task.CheckInstants) " +
		"and the writer's createTask where it runs, so the stored task and every finished-work " +
		"predicate that reads it meet nil or a real instant",
	"worklet.Task.ClosedAt": "never the zero instant in a create: tracker.create_task and " +
		"tracker.create_task_asking refuse one before they are asked (worklet.Task.CheckInstants) " +
		"and the writer's createTask where it runs, so the stored task and every finished-work " +
		"predicate that reads it meet nil or a real instant",
	"worklet.Task.ArchivedAt": "never the zero instant in a create: tracker.create_task and " +
		"tracker.create_task_asking refuse one before they are asked (worklet.Task.CheckInstants) " +
		"and the writer's createTask where it runs, so the stored task and every archive " +
		"predicate that reads it meet nil or a real instant",
	"worklet.Comment.ResolvedAt": "never the zero instant on a comment a write posts: " +
		"tracker.create_task_asking (worklet.Comment.CheckInstants) and tracker.update_task " +
		"(worklet.TaskPatch.CheckDates, which checks its comment) refuse one before they are " +
		"asked, and the writer where it runs, so the stored comment and every reader of whether it " +
		"is resolved meet nil or a real instant",
	"inbox.Gesture[github.com/crewlet/crewlet/internal/worklet.Reason].ReadThrough": "a position " +
		"that names nothing is no position to every reader of a gesture — Gesture.Empty, " +
		"CheckGesture and Apply, in the tracker's MarkInbox decide — each asking IsZero first " +
		"(Gesture.readThrough), so nil, which is what crosses, reads the same",
	"code.MarkGesture.ReadThrough": "read only as inbox's gesture (MarkGesture.Gesture), by " +
		"Gesture.Empty, CheckGesture and Apply in code's mark decide and its marks record's apply, " +
		"each asking IsZero first (inbox's Gesture.readThrough): a position that names nothing is no " +
		"position, so nil, which is what crosses, reads the same",
}

// NO POINTER AT A ZERO CROSSES WHERE A READER WOULD MISS IT. The wire leaves a
// pointer whose type's own IsZero calls what it points at zero off, exactly as
// it leaves nil off, and the serving node decodes nil: a field whose readers
// told a pointer at the zero from nil — a patch's date at the zero instant,
// once its clear — lost that meaning on every routed request while the same
// request in process kept it, and the zero gate below it, reading a type's
// IsZero on values alone, passed. So every such field, at every depth of every
// operation's arguments, is named with why its readers read the two alike, or
// how none of them meets the pointer at the zero.
//
// Mutation: delete worklet.TaskPatch.DueAt from zeroThroughPointer and the gate
// names it.
func TestNoPointerAtAZeroCrossesWhereAReaderWouldMissIt(t *testing.T) {
	t.Parallel()
	found := map[string]string{}
	for ty, from := range inputTypes() {
		if ty.Kind() != reflect.Struct {
			continue
		}
		for field := range ty.Fields() {
			if (!field.IsExported() && !field.Anonymous) || field.Tag.Get("json") == "-" {
				continue
			}
			// THE ENCODER'S OWN TEST (encoding/json/v2's fields.go): a pointer
			// or an interface whose type has an IsZero is asked it through
			// what it holds.
			if kind := field.Type.Kind(); (kind == reflect.Pointer || kind == reflect.Interface) &&
				field.Type.Implements(reflect.TypeFor[isZeroer]()) {
				found[ty.String()+"."+field.Name] = from
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, listed := zeroThroughPointer[name]; !listed {
			t.Errorf("%s (from %s) points at a type whose own IsZero the wire asks through the pointer, "+
				"so a pointer at that zero crosses as nil: name in zeroThroughPointer why every reader "+
				"reads the two alike, or reshape it until none meets the pointer at the zero", name, found[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(zeroThroughPointer)) {
		if _, still := found[name]; !still {
			t.Errorf("zeroThroughPointer lists %s, which nothing an operation takes holds any more", name)
		}
	}
}

// samplesOf is values of ty that are not Go's zero — an empty list and a list
// of one for a slice, since a method may call either zero — or none for a type
// with nothing a field could be set to.
func samplesOf(ty reflect.Type) []reflect.Value {
	v := reflect.New(ty).Elem()
	switch ty.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		return []reflect.Value{reflect.MakeSlice(ty, 0, 0), reflect.MakeSlice(ty, 1, 1)}
	case reflect.Map:
		v.Set(reflect.MakeMap(ty))
	case reflect.Pointer:
		v.Set(reflect.New(ty.Elem()))
	case reflect.Array:
		inner := samplesOf(ty.Elem())
		if ty.Len() == 0 || len(inner) == 0 {
			return nil
		}
		v.Index(0).Set(inner[len(inner)-1])
	case reflect.Struct:
		if ty == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
			break
		}
		set := false
		for i, field := range slices.Collect(ty.Fields()) {
			if inner := samplesOf(field.Type); field.IsExported() && len(inner) > 0 {
				v.Field(i).Set(inner[len(inner)-1])
				set = true
				break
			}
		}
		if !set {
			return nil
		}
	default:
		return nil
	}
	return []reflect.Value{v}
}
