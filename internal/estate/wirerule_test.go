package estate

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/worklet"
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

// EVERY KEY A TYPE DOES NOT DECLARE IS NAMED, AT EVERY DEPTH, AND ONCE: inside
// a nested struct, in every element of a list (named once for the list, not
// once per element), behind a pointer, and a `-` field's name, which the wire
// never carries; while a promoted field, a name in another case, a map's keys
// and a raw document's inside are read as the standard library reads them —
// declared, or data.
//
// Mutation: decode with json.Unmarshal, as define did, and no key is named;
// strike a member only from the element the decoder pointed at, and the list's
// is named twice.
func TestEveryKeyATypeDoesNotDeclareIsNamedAtEveryDepth(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"name":"another case","Zone":"promoted",` +
		`"Inner":{"note":"n","went_on":true},` +
		`"List":[{"note":"a"},{"note":"b","went_on":1},{"went_on":2}],` +
		`"Ptr":{"later":"x"},` +
		`"ByName":{"any key at all":{"note":"data"}},` +
		`"Raw":{"anything":{"nested":true}},` +
		`"Hidden":"h","top":1}`)
	var into wireArgs
	unknown, err := decodeInput(raw, &into)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"Hidden", "Inner.went_on", "List[].went_on", "Ptr.later", "top"}
	if !slices.Equal(unknown, want) {
		t.Errorf("the keys named are %q, want %q", unknown, want)
	}

	whole := []byte(`{"name":"another case","Zone":"promoted","Inner":{"note":"n"},` +
		`"List":[{"note":"a"}],"ByName":{"any key at all":{"note":"data"}},"Raw":{"anything":1}}`)
	into = wireArgs{}
	if unknown, err = decodeInput(whole, &into); err != nil || len(unknown) != 0 {
		t.Fatalf("a document holding only what the type declares named (%q, %v), want nothing", unknown, err)
	}
	if into.Name != "another case" || into.Zone != "promoted" || into.ByName["any key at all"].Note != "data" ||
		string(into.Raw) != `{"anything":1}` {
		t.Errorf("a document read whole decoded as %+v", into)
	}
	if _, err = decodeInput([]byte(`{"Name":7}`), &into); err == nil {
		t.Error("a value of the wrong type decoded, want the decoder's own failure")
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
	args, unknown, err := opAccessTell.spec.decode(raw)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("the serving node read the told record as (%q, %v)", unknown, err)
	}
	if got := args.(accessTellArgs).Told; got.Changes != nil || !got.Baseline {
		t.Errorf("a baseline crossed carrying changes %q, want none — the serving node refuses a "+
			"baseline that tells one", got.Changes)
	}
}

// A DATA NODE RUNS NOTHING OF A REQUEST ITS BUILD CANNOT READ WHOLE: a request
// whose arguments or actor hold keys its types do not declare is refused,
// naming every one under the member of the request that held it and the node
// that refused, as its own error — not a decode failure, and never run without
// them — before the tracker is asked anything. The same request read whole runs.
//
// Mutation: decode the inputs leniently, as define did (RejectUnknownMembers
// off), and the task is created without the keys it was sent.
func TestADataNodeRunsNothingOfARequestItsBuildCannotReadWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	q := f.start(t)
	newer := request{Op: opCreateTask.spec.name,
		Actor: json.RawMessage(`{"handle":"swe","kind":"agent","provenance":{"Zone":"Europe/Paris"}}`),
		Args:  json.RawMessage(`{"OpID":"op-1","Task":{"Project":"ENG","went_on":true},"Shelved":1}`)}
	rep := askSigned(t, q, "data-a", newer)
	if rep.Err == nil || rep.Unserved != "" || rep.Result != nil {
		t.Fatalf("a request this build cannot read whole was answered %+v, want an error and nothing run", rep)
	}
	var refused *ErrUnknownKeys
	if err := decodeError(rep.Err); !errors.As(err, &refused) {
		t.Fatalf("the refusal crossed as %v, want an ErrUnknownKeys", err)
	}
	if want := []string{"actor.provenance.Zone", "args.Shelved", "args.Task.went_on"}; refused.Op !=
		opCreateTask.spec.name || !slices.Equal(refused.Keys, want) || !slices.Equal(refused.Nodes, []string{"data-a"}) {
		t.Errorf("the refusal names %s, %q and %q, want %s, %q and the node", refused.Op, refused.Keys,
			refused.Nodes, opCreateTask.spec.name, want)
	}
	if node.askedFor("create") || len(node.actors) != 0 {
		t.Fatal("the tracker was asked to run a request its build could not read whole")
	}

	whole := request{Op: newer.Op, Actor: mustInput(t, swe),
		Args: json.RawMessage(`{"OpID":"op-2","Task":{"Project":"ENG"}}`)}
	if rep = askSigned(t, q, "data-a", whole); rep.Err != nil || rep.Unserved != "" {
		t.Fatalf("the request read whole was answered %+v, want it run", rep)
	}
	if !node.askedFor("create") {
		t.Fatal("the request read whole did not run")
	}
}

// A REQUEST ONE DATA NODE'S BUILD CANNOT READ IS SERVED BY ANOTHER'S: the node
// that refused it ran nothing, so the walk asks the next — a data node of the
// writer's build — under the same operation, and the node that refused is not
// the one asked first next time.
//
// Mutation: drop the refusal's arm from Router.settle and the write fails with
// the first node's refusal.
func TestARequestOneBuildCannotReadIsServedByAnother(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.olderBuild = declaresLess("went_on", "Task") })
	second := f.other(first)
	written, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1", worklet.Task{Project: "ENG"}, nil)
	if err != nil || written.Key != "ENG-1" {
		t.Fatalf("create = (%+v, %v), want the second node's answer", written, err)
	}
	if first.askedFor("create") {
		t.Fatal("the node whose build could not read the write ran it")
	}
	if got := second.ops(); !slices.Equal(got, []string{"op-1"}) {
		t.Fatalf("the second node ran %v, want the write under its own operation", got)
	}
	if _, err = f.client.WriterAs(swe).CreateTask(t.Context(), "op-2", worklet.Task{Project: "ENG"},
		nil); err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	asked := first.requests
	first.mu.Unlock()
	if asked != 1 {
		t.Errorf("the node that refused was asked %d requests, want only the first: the node that "+
			"served it is asked first after", asked)
	}
}

// A REQUEST NO DATA NODE CAN READ WHOLE SAYS WHAT WOULD SERVE IT: the caller is
// told the operation, every key and every node that refused — a data node of
// the writer's build is needed — as the error every caller of an estate nobody
// served also reads, since nothing ran; and never that its outcome is unknown.
//
// Mutation: end the walk with the bare ErrUnserved (drop walk.refusedOr) and
// errors.As finds no ErrUnknownKeys.
func TestARequestNoDataNodeCanReadWholeSaysWhatWouldServeIt(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	order := f.client.order(opCreateTask.spec, []string{"data-a", "data-b"})
	for _, node := range f.nodes {
		node.set(func(n *fakeNode) { n.olderBuild = declaresLess("went_on", "Task") })
	}
	_, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1", worklet.Task{Project: "ENG"}, nil)
	var refused *ErrUnknownKeys
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if refused.Op != opCreateTask.spec.name || !slices.Equal(refused.Keys, []string{"args.Task.went_on"}) ||
		!slices.Equal(refused.Nodes, order) {
		t.Errorf("the refusal names %s, %q and %q, want %s, the key and %q", refused.Op, refused.Keys,
			refused.Nodes, opCreateTask.spec.name, order)
	}
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || unserved.Silent() || errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("err = %v, want an estate nobody served, in which nothing ran", err)
	}
	if !strings.Contains(err.Error(), "finish the upgrade") {
		t.Errorf("the refusal says %q, which does not tell an operator what serves it", err)
	}
	for _, node := range f.nodes {
		if node.askedFor("create") {
			t.Errorf("%s ran a write its build could not read whole", node.name)
		}
	}
}

// A REFUSAL NEVER HIDES A WRITE THAT MAY HAVE LANDED: a repeatable write one
// node refused and another never answered may have run on the second, so its
// outcome is unknown and the same operation is the retry — not a refusal
// saying nothing ran, which invites a second write under a fresh id.
//
// Mutation: put the walk's refusal ahead of its unanswered nodes in
// walk.unserved and the caller is told nothing ran.
func TestARefusalNeverHidesAWriteThatMayHaveLanded(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.olderBuild = declaresLess("went_on", "Task") })
	f.other(first).set(func(n *fakeNode) { n.silent = true })
	_, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1", worklet.Task{Project: "ENG"}, nil)
	if !errors.Is(err, ErrOutcomeUnknown) || errors.As(err, new(*ErrUnknownKeys)) {
		t.Fatalf("err = %v, want the outcome unknown: the silent node may have run it", err)
	}
}

// A ONCE-WRITE A NODE REFUSED UNREAD IS ASKED OF THE NEXT: a page comment with
// no key of its caller's is never asked twice where a node MAY have run it, and
// a node that refused it for keys its build does not declare ran nothing of it.
//
// Mutation: drop the refusal's arm from Router.settle and the comment fails
// with the first node's refusal.
func TestAOnceWriteANodeRefusedUnreadIsAskedOfTheNext(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.olderBuild = declaresLess("went_on") })
	if _, _, err := f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p1",
		doclet.NewComment{Body: "hi"}); err != nil {
		t.Fatalf("a comment the first node refused unread failed: %v", err)
	}
	if first.askedFor("comment") || !f.other(first).askedFor("comment") {
		t.Fatal("the comment did not run on the node that could read it, and only there")
	}
}

// A KEYED WRITE MOVES OFF A COMMITTER WHOSE BUILD CANNOT READ IT: the push goes
// to the repository's runner-up with its push id, nothing decided by the
// committer that refused, and the next push carrying what it refused asks that
// committer last — a keyed order has no sticky node to move the walk off it,
// and its build outlasts one push.
//
// Mutation: drop markUnread from the refusal's arm and the second push is asked
// of the committer that refused again.
func TestAKeyedWriteMovesOffACommitterThatCannotReadIt(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b"}
	f := newFleet(t, holders...)
	const repo = "repo-newer-push"
	o := rendezvous.Order(repo, holders)
	committer := f.nodes[o[0]]
	committer.set(func(n *fakeNode) { n.undeclared = []string{"args.commands[].new"} })
	for _, push := range []string{"push-1", "push-2"} {
		got, err := f.client.Code().Propose(t.Context(), proposalTo(repo, push))
		if err != nil || got.OpID != o[1] {
			t.Fatalf("%s = (%+v, %v), want the runner-up %s's answer", push, got, err, o[1])
		}
	}
	committer.mu.Lock()
	asked, decided := committer.requests, len(committer.proposals)
	committer.mu.Unlock()
	if asked != 1 || decided != 0 {
		t.Fatalf("the committer that refused was asked %d pushes and decided %d, want asked once and "+
			"deciding none", asked, decided)
	}
}

// A GIT OPERATION NO GITLET NODE CAN READ WHOLE IS REFUSED SO TOO: the walk over
// the Gitlet nodes ends in the refusal, its account the git walk's own, and the
// node's git half was never asked.
//
// Mutation: end routeGit with the bare ErrUnserved and errors.As finds no
// ErrUnknownKeys.
func TestAGitOperationNoGitletNodeCanReadWholeIsRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitletNodes(t, []string{"git-a"})
	f.nodes["git-a"].set(func(n *fakeNode) { n.olderBuild = declaresLess("went_on", "tree") })
	_, err := f.client.Code().GitRead(t.Context(), treeOf(repoID(1)))
	var refused *ErrUnknownKeys
	if !errors.As(err, &refused) || !slices.Equal(refused.Keys, []string{"args.tree.went_on"}) ||
		refused.Unserved == nil || !refused.Unserved.Git {
		t.Fatalf("err = %v, want the refusal over the git walk's account", err)
	}
	if reads := halves["git-a"].reads(); len(reads) != 0 {
		t.Fatalf("the git half was asked %d reads it could not read whole", len(reads))
	}
}

// AN ANSWER A CALLER WRITES BACK IS READ WHOLE OR REFUSED: a newer data node's
// answer holding a key this build does not declare is refused, naming it under
// the result and the node that gave it, since the caller would drop it and
// write the rest back; an answer read whole decodes; and the serving node
// leaves its fields at their zero off it. An answer nobody writes back drops
// what this build does not declare, as it always has: showing less of it loses
// nothing.
//
// Mutation: decode a carried answer with json.Unmarshal and the prior is
// handed to the save without the key; drop answeredWhole from opViewPrior and
// the same.
func TestAnAnswerACallerWritesBackIsReadWholeOrRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	x, err := exchangesOf(opViewPrior.spec, opViewPrior, &swe, viewPriorArgs{ID: "v1"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, final, err := f.client.settle(t.Context(), opViewPrior.spec, x, "data-a",
		reply{Node: "data-a", Result: json.RawMessage(`{"Held":true,"Owner":"ana","Version":3}`)}, &walk{})
	var refused *ErrUnknownKeys
	if !final || !errors.As(err, &refused) || !refused.Answer ||
		!slices.Equal(refused.Keys, []string{"result.Version"}) || !slices.Equal(refused.Nodes, []string{"data-a"}) {
		t.Fatalf("a carried answer holding a key this build does not declare settled as (%v, %v), "+
			"want the answer refused, naming it", final, err)
	}
	value, final, err := f.client.settle(t.Context(), opViewPrior.spec, x, "data-a",
		reply{Node: "data-a", Result: json.RawMessage(`{"Held":true,"Owner":"ana"}`)}, &walk{})
	if !final || err != nil || value != (worklet.ViewPrior{Held: true, Owner: "ana"}) {
		t.Fatalf("a carried answer read whole settled as (%+v, %v, %v)", value, final, err)
	}
	if got, err := encodeResult(opViewPrior.spec, worklet.ViewPrior{Held: true}); err != nil ||
		string(got) != `{"Held":true}` {
		t.Errorf("a carried answer is sent as (%s, %v), want its fields at their zero left off", got, err)
	}

	shown, err := exchangesOf(opTasks.spec, opTasks, nil, tasksArgs{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	answer := mustJSON(t, worklet.Answer{TotalHint: 7})
	answer = append(answer[:len(answer)-1], `,"later":true}`...)
	value, _, err = f.client.settle(t.Context(), opTasks.spec, shown, "data-a",
		reply{Node: "data-a", Result: answer}, &walk{})
	if err != nil || value.(worklet.Answer).TotalHint != 7 {
		t.Errorf("an answer nobody writes back settled as (%+v, %v), want what this build declares of it",
			value, err)
	}
}

// A REQUEST NO BUILD COULD READ WHOLE CROSSES AS ITSELF, AND AS AN ESTATE NOBODY
// SERVED: a node that answers on another's behalf — a Gitlet node proposing a
// push its data nodes refused — hands its asker the refusal with every field,
// so the asker's walk moves on from it as from any node that ran nothing, and
// every caller that reads an estate nobody served still reads one.
//
// Mutation: drop ErrUnknownKeys from typedKinds and errors.As finds nothing on
// the far side.
func TestARefusalOfUnreadKeysCrossesAsItself(t *testing.T) {
	t.Parallel()
	original := fmt.Errorf("record the end: %w", &ErrUnknownKeys{Op: "executor.end",
		Keys: []string{"args.Ending.went_on"}, Nodes: []string{"data-a", "data-b"},
		Unserved: &ErrUnserved{Detail: "data-a: its build does not declare it"}})
	got := throughWire(t, original)
	var refused *ErrUnknownKeys
	if !errors.As(got, &refused) || refused.Op != "executor.end" ||
		!slices.Equal(refused.Keys, []string{"args.Ending.went_on"}) ||
		!slices.Equal(refused.Nodes, []string{"data-a", "data-b"}) || refused.Unserved == nil {
		t.Fatalf("the refusal crossed as %#v, want every field", refused)
	}
	if !errors.As(got, new(*ErrUnserved)) || got.Error() != original.Error() {
		t.Errorf("the refusal crossed as %q, want an estate nobody served saying %q", got, original)
	}
}

// declaresLess is a data node of an older build, as the router meets one: every
// request it is sent holds key, set, in the object at path of its arguments —
// a field the asker's build sets and this node's does not declare.
func declaresLess(key string, path ...string) func(request) request {
	return func(req request) request {
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(req.Args))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			panic(fmt.Sprintf("the arguments of %s do not decode: %v", req.Op, err))
		}
		at := doc
		for _, member := range path {
			child, held := at[member].(map[string]any)
			if !held {
				child = map[string]any{}
				at[member] = child
			}
			at = child
		}
		at[key] = true
		raw, err := json.Marshal(doc)
		if err != nil {
			panic(err)
		}
		req.Args = raw
		return req
	}
}

// inputRoots is every type the wire's rule for an operation's inputs governs:
// each operation's arguments, the actor a write runs as, and the answer of
// every read a caller carries into a write ([op.answeredWhole]).
func inputRoots() map[reflect.Type]string {
	roots := map[reflect.Type]string{reflect.TypeFor[Actor](): "the actor a write runs as"}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		if _, held := roots[spec.args]; !held {
			roots[spec.args] = name + "'s arguments"
		}
		if spec.whole {
			roots[spec.result] = name + "'s answer"
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
// every operation at every depth, the actor a write runs as, and an answer a
// caller writes back: a key goes on the wire only where it carries a value, so
// a field a later build adds is refused by an older data node only for a write
// that uses it, and never for one that does not. What a field's zero means is
// [define]'s rule: what a build without the field does.
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
			t.Errorf("%s.%s (from %s) goes on the wire at its zero, as %s: an older data node "+
				"refuses every write that carries it", ty, member, types[ty], members[member])
		}
	}
	// A GATE THAT READ NOTHING PASSES — so it says what it read.
	if structs < 200 {
		t.Fatalf("the gate read %d struct types; it is not reading what the operations take", structs)
	}
}

// zeroByMethod is every type an operation's inputs — or an answer a caller
// writes back — hold that has an IsZero method of its own, which is what
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
			t.Errorf("zeroByMethod lists %s, which nothing an operation takes holds any more", name)
		}
	}
}

// zeroThroughPointer is every field an operation's inputs — or an answer a
// caller carries into another request — hold whose type is a POINTER (or an
// interface) at a type with an IsZero method of its own, as "<struct
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
// operation's inputs and every answer carried on, is named with why its readers
// read the two alike, or how none of them meets the pointer at the zero.
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

// decodesItself is every type an operation takes that decodes its own JSON —
// an UnmarshalJSON of its own, or the standard library's twin of one — and why
// it drops no member a newer build added: a method decoding an object with
// json.Unmarshal of its own would drop exactly the keys the wire's rule
// refuses, below where the rule can see.
//
// TWO-SIDED, like [carriedByServer].
var decodesItself = map[string]string{
	"time.Time": "an RFC 3339 string: it holds no member to drop",
	"jsontext.Value": "json.RawMessage: the bytes as their writer wrote them — a datastore " +
		"document, a canonical snapshot — which are data, and which nothing on the wire decodes",
}

// NO TYPE AN OPERATION TAKES DECODES A KEY AWAY BY ITSELF. The wire's rule
// refuses an undeclared member at every depth the standard library decodes,
// and a type with an UnmarshalJSON of its own is a depth it does not: such a
// method decoding an object with a plain json.Unmarshal would drop a newer
// build's key with nothing to say so. Each one is named with why it cannot —
// it decodes no object, or keeps what it does not know and writes it back.
//
// Mutation: delete time.Time from decodesItself and the gate names it.
func TestNoTypeAnOperationTakesDecodesAKeyAwayByItself(t *testing.T) {
	t.Parallel()
	found := map[string]string{}
	for ty, from := range inputTypes() {
		if ty.Kind() == reflect.Pointer {
			continue
		}
		for _, method := range []reflect.Type{reflect.TypeFor[json.Unmarshaler](),
			reflect.TypeFor[jsonv2.UnmarshalerFrom]()} {
			if ty.Implements(method) || reflect.PointerTo(ty).Implements(method) {
				found[ty.String()] = from
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, listed := decodesItself[name]; !listed {
			t.Errorf("%s (from %s) decodes its own JSON, below where the wire's rule refuses an "+
				"undeclared key: refuse one there too, or keep it and write it back — and name it "+
				"in decodesItself with which", name, found[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(decodesItself)) {
		if _, still := found[name]; !still {
			t.Errorf("decodesItself lists %s, which nothing an operation takes holds any more", name)
		}
	}
}

// answeredWholeBy is every read declared answered whole ([op.answeredWhole]),
// and the write its answer is carried into — the operation, and who carries
// it.
//
// TWO-SIDED: a read declared and not listed fails, and so does one listed and
// no longer declared — a list of which answers are written back is a claim
// about callers, and one nothing checks goes stale as they move.
var answeredWholeBy = map[string]string{
	"artifacts.version_carried": "artifacts.publish: a patch keeps every file of its base version, " +
		"and a publish carries a file of a version it names, each as the entry it read here " +
		"(internal/artifactupload)",
	"tracker.view_prior": "tracker.write_view: a save hands back the prior it read, which its " +
		"decide judges the stored view against (internal/agent/builtin's save_work_view)",
	"tracker.views_carried": "tracker.write_view: save_work_view replaces a view whole, by an id " +
		"list_work_views lists, with what its call re-sends of what the listing showed " +
		"(internal/agent/builtin)",
	"tracker.catalogue_carried": "tracker.write_types: write_work_catalogue replaces the types and " +
		"the fields (tracker.write_fields), each declaration whole, and tells a model to send back " +
		"what get_work_catalogue gave it (internal/agent/builtin)",
	"tracker.project_carried": "tracker.write_project: write_project replaces a project's field " +
		"declarations, each whole, and tells a model to read describe_project first and send the " +
		"whole set (internal/agent/builtin)",
	"artifactdata.schema_carried": "artifactdata.configure: set_artifact_schema replaces every " +
		"collection, and tells a model to read them with read_artifact_data and send them back with " +
		"its change (internal/agent/builtin)",
	"tracker.expanded_query": "tracker.tasks: the query a list's parameters expand to is the query " +
		"its read asks (internal/agent/builtin's list_work_items, internal/api/queries' work items)",
	"code.repo_state": "code.repo_state: a page's cursor (code.RepoSnapshot.Next) is the next " +
		"page's request (internal/code/cache)",
}

// EVERY ANSWER READ WHOLE IS ONE A CALLER CARRIES INTO ANOTHER REQUEST, and
// names the request: a read declared so refuses a newer data node's answer an
// older caller cannot read whole, which is right only where the caller would
// send the rest on — written back, or asked by — and wrong for one that only
// shows it.
//
// Mutation: drop answeredWhole from opViewPrior and the gate names
// tracker.view_prior as listed and no longer read whole.
func TestEveryAnswerReadWholeIsOneACallerCarriesIntoAnotherRequest(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		_, listed := answeredWholeBy[name]
		if whole := registry[name].whole; whole != listed {
			t.Errorf("%s is read whole: %v, and listed in answeredWholeBy: %v — a read whose answer a "+
				"caller carries into another request is both", name, whole, listed)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(answeredWholeBy)) {
		into, _, _ := strings.Cut(answeredWholeBy[name], ":")
		if _, ok := registry[into]; !ok {
			t.Errorf("answeredWholeBy says %s's answer is carried into %q, which is no operation", name, into)
		}
	}
}
