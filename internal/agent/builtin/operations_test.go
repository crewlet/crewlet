package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY FAMILY OF WRITE TOOLS NAMES ITS OPERATION BY ONE RULE.
//
// # What was wrong
//
// Which operation a write tool's call is was decided in two places. The
// tracker's writes carried the rule, and the artifacts' writes — the
// datastore's with them, through the same deps — carried a copy: a turn's call
// and a request carrying its own key derive their operation from what they
// carry and are refused an `op_id`, while the operator's assistant over MCP is
// minted one for each call, or brings back the one it was answered and is held
// to the call that op_id names. The copy had already reworded every refusal
// and the argument's own description, and a copy is where the next drift is a
// CHECK rather than a word: one that took a brought-back `op_id` without
// holding it to its call would run another call under an operation promised to
// make nothing twice, and make a second write under it.
//
// # What is asserted
//
// One write of each family is made on every surface a write is made on — a
// seat's turn, a request carrying its own operation key (the act route's
// path) and the operator's assistant over MCP, with every kind of `op_id` it
// can bring — and every family accepts the same calls, derives or mints their
// operation alike, refuses the same calls in the same words before anything
// reaches its writer, and offers the argument on the same surfaces under the
// same description.
func TestEveryWriteFamilyBindsOneWay(t *testing.T) {
	t.Parallel()
	key := statelog.NewOpID(time.Now(), "request")
	families := []familyRig{trackerFamily(t, key), artifactFamily(t, key), datastoreFamily(t, key),
		codeFamily(t, key)}

	// THE OPERATOR'S ASSISTANT IS MINTED AN OPERATION FOR ITS CALL, and every
	// case below brings one of these back.
	minted := make([]string, len(families))
	firstKey := make([]string, len(families))
	for i, f := range families {
		first := f.call(t, onOperatorMCP, maps.Clone(f.one))
		op := answeredOpOf(first.got)
		if first.got.Failed || first.wrote != 1 || statelog.CheckCallerOpID(op) != nil {
			t.Fatalf("%s: the operator's first call answered %q, writing %d time(s), "+
				"with op_id %q", f.tool, first.got.Output, first.wrote, op)
		}
		minted[i], firstKey[i] = op, first.key
	}

	own := func(i int) any { return minted[i] }
	for _, c := range []struct {
		name    string
		surface bindSurface
		other   bool            // the same tool, asked for something else
		opID    func(i int) any // the op_id family i's call brings, nil for none
		want    bindWant
	}{
		{name: "a seat's turn", surface: onSeatTurn, want: bindDerived},
		{name: "a seat's turn bringing an op_id", surface: onSeatTurn, opID: own,
			want: bindRefused},
		{name: "a request carrying its own key", surface: onKeyedRequest, want: bindDerived},
		{name: "a request carrying its own key and an op_id", surface: onKeyedRequest,
			opID: own, want: bindRefused},
		{name: "the operator's assistant", surface: onOperatorMCP, want: bindMinted},
		{name: "the operator's assistant bringing an empty op_id", surface: onOperatorMCP,
			opID: func(int) any { return "" }, want: bindMinted},
		{name: "the operator's assistant bringing its op_id back", surface: onOperatorMCP,
			opID: own, want: bindResumed},
		{name: "its op_id brought back with another call", surface: onOperatorMCP,
			other: true, opID: own, want: bindRefused},
		{name: "an op_id another tool was answered", surface: onOperatorMCP,
			opID: func(i int) any { return minted[(i+1)%len(minted)] }, want: bindRefused},
		{name: "a step of its operation", surface: onOperatorMCP,
			opID: func(i int) any { return minted[i] + ".step" }, want: bindRefused},
		{name: "an op_id this engine never minted", surface: onOperatorMCP,
			opID: func(int) any { return "retry-1" }, want: bindRefused},
		{name: "an op_id that is not text", surface: onOperatorMCP,
			opID: func(int) any { return 42.0 }, want: bindRefused},
	} {
		// SEQUENTIAL, not parallel: each family's fakes count what reached
		// its writer across these cases.
		t.Run(c.name, func(t *testing.T) {
			answers := make([]tools.Result, len(families))
			for i, f := range families {
				args := maps.Clone(f.one)
				if c.other {
					args = maps.Clone(f.other)
				}
				if c.opID != nil {
					args["op_id"] = c.opID(i)
				}
				got := f.call(t, c.surface, args)
				answers[i] = got.got
				switch c.want {
				case bindRefused:
					if !got.got.Failed || got.wrote != 0 {
						t.Errorf("%s was not refused before its writer: it wrote %d "+
							"time(s) and answered %q", f.tool, got.wrote, got.got.Output)
					} else if class := tools.RefusalOf(got.got); class != tools.RefusalInvalid {
						t.Errorf("%s refused it as %q, not as a call to change: %q",
							f.tool, class, got.got.Output)
					}
				case bindDerived:
					if got.got.Failed || got.wrote != 1 || got.key == "" {
						t.Fatalf("%s answered %q, writing %d time(s) under %q", f.tool,
							got.got.Output, got.wrote, got.key)
					}
					if op := answeredOpOf(got.got); op != "" {
						t.Errorf("%s handed back op_id %q to a caller whose turn or "+
							"key already names the operation", f.tool, op)
					}
					if again := f.call(t, c.surface, maps.Clone(args)); again.key != got.key {
						t.Errorf("%s made again wrote under %q, the first under %q — "+
							"another operation for the same call", f.tool, again.key, got.key)
					}
				case bindMinted:
					op := answeredOpOf(got.got)
					if got.got.Failed || got.wrote != 1 || statelog.CheckCallerOpID(op) != nil {
						t.Fatalf("%s answered %q, writing %d time(s), with op_id %q",
							f.tool, got.got.Output, got.wrote, op)
					}
					if op == minted[i] || got.key == firstKey[i] {
						t.Errorf("%s with no op_id to bring back was answered as the "+
							"operation %s, the first call's", f.tool, minted[i])
					}
				case bindResumed:
					if got.got.Failed || got.wrote != 1 {
						t.Fatalf("%s brought back with its own op_id answered %q, "+
							"writing %d time(s)", f.tool, got.got.Output, got.wrote)
					}
					if op := answeredOpOf(got.got); op != minted[i] || got.key != firstKey[i] {
						t.Errorf("%s brought back with op_id %s answered op_id %q and "+
							"wrote under %q, the first call under %q — another operation",
							f.tool, minted[i], op, got.key, firstKey[i])
					}
				}
			}
			if c.want != bindRefused {
				return
			}
			// A SEAT'S SCHEMA DECLARES NO op_id, so its tools' own gate refuses
			// one, naming what each tool does read; every other refusal is the
			// rule's, and says one thing whichever family gives it.
			if c.surface == onSeatTurn {
				for i, f := range families {
					if !errors.Is(answers[i].Cause, builtin.ErrUndeclaredArgument) {
						t.Errorf("a seat's %s refused an op_id as %v, not as an "+
							"argument it does not declare", f.tool, answers[i].Cause)
					}
				}
				return
			}
			told := func(i int) string {
				return strings.ReplaceAll(answers[i].Output, families[i].tool, "<tool>")
			}
			for i := 1; i < len(families); i++ {
				if told(i) != told(0) {
					t.Errorf("%s refused %q where %s refused %q", families[i].tool,
						told(i), families[0].tool, told(0))
				}
			}
		})
	}

	// THE ARGUMENT IS OFFERED ON THE OPERATOR'S CATALOGUE AND NOWHERE ELSE,
	// and it means one thing whichever tool offers it.
	var described []string
	for _, f := range families {
		if _, held := propertiesOf(f.on[onSeatTurn].tool)["op_id"]; held {
			t.Errorf("a seat's %s offers op_id", f.tool)
		}
		for _, s := range []bindSurface{onKeyedRequest, onOperatorMCP} {
			prop, held := propertiesOf(f.on[s].tool)["op_id"].(map[string]any)
			if !held {
				t.Errorf("the operator's %s offers no op_id", f.tool)
				continue
			}
			described = append(described, fmt.Sprint(prop["description"]))
		}
	}
	for i := 1; i < len(described); i++ {
		if described[i] != described[0] {
			t.Errorf("op_id is described %q on one write and %q on another",
				described[i], described[0])
		}
	}
}

// bindSurface is a surface a write tool is called on, as the operation rule
// tells them apart.
type bindSurface int

const (
	// onSeatTurn is a seat's registry called in its turn: the turn names
	// the operation.
	onSeatTurn bindSurface = iota
	// onKeyedRequest is the operator's catalogue called with the request's
	// own operation key, which the act route carries in its Idempotency-Key.
	onKeyedRequest
	// onOperatorMCP is the operator's catalogue with no key: the operator's
	// assistant over MCP, which holds its operations itself.
	onOperatorMCP
)

func (s bindSurface) String() string {
	return [...]string{"a seat's turn", "a keyed request", "the operator's MCP"}[s]
}

// bindWant is what a case expects of every family's call.
type bindWant int

const (
	// bindDerived is accepted under the operation its turn or its key names:
	// no op_id is answered, and the same call again is that operation.
	bindDerived bindWant = iota
	// bindMinted is accepted as a new operation, answered as its op_id.
	bindMinted
	// bindResumed is accepted as the operation its op_id names.
	bindResumed
	// bindRefused is refused before anything reaches the writer.
	bindRefused
)

// familyRig is one family of write tools over its own fakes: one write, the
// same tool asked for something else, the tool as each surface serves it, and
// what reached the family's writer.
type familyRig struct {
	tool       string
	one, other map[string]any
	on         map[bindSurface]familyTool
	// writes is how many writes reached the family's writer so far, and the
	// operation key the last of them was named by.
	writes func() (made int, key string)
}

// familyTool is a write tool as one surface serves it: the gated tool, and who
// calls it in which turn (nil: none).
type familyTool struct {
	tool tools.SeatCallable
	ctx  context.Context
	turn *turnctx.Turn
}

// familyCall is one call's answer, and what it wrote.
type familyCall struct {
	got   tools.Result
	wrote int
	// key is the operation key its write was named by, empty where it wrote
	// nothing.
	key string
}

func (r familyRig) call(t *testing.T, s bindSurface, args map[string]any) familyCall {
	t.Helper()
	on := r.on[s]
	before, _ := r.writes()
	got, err := on.tool.CallForTurn(on.ctx, on.turn, args)
	if err != nil {
		t.Fatalf("%s on %s: %v", r.tool, s, err)
	}
	after, key := r.writes()
	if after == before {
		key = ""
	}
	return familyCall{got: got, wrote: after - before, key: key}
}

// trackerFamily is the tracker's writes, by a comment on an item.
func trackerFamily(t *testing.T, key string) familyRig {
	t.Helper()
	trk := newFakeTracker()
	operator := func(actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)) builtin.OperatorDeps {
		return builtin.OperatorDeps{
			Work:      builtin.WorkDeps{Reader: trk, Writer: trk.as, Actor: actor},
			Authorize: builtin.Decide(chartLeads),
		}
	}
	// THE ACT ROUTE'S ACTOR: the request's principal, carrying its key and
	// the instant the key was minted at.
	keyed := func(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
		actor, err := operatorActor(ctx, turn)
		actor.WorkKey = key
		actor.WorkSince, _ = statelog.OpMintedAt(key)
		return actor, err
	}
	person := personHolding(iam.AllGrants...)
	tool := builtin.CommentOnWorkTool
	return familyRig{
		tool:  tool,
		one:   map[string]any{"item": "ENG-1", "body": "noted"},
		other: map[string]any{"item": "ENG-1", "body": "noted, and the fix is up"},
		on: map[bindSurface]familyTool{
			onSeatTurn: {tool: asSeatCallable(t, registered(t, builtin.Deps{
				Work: builtin.WorkDeps{Reader: trk, Writer: trk.as}}, tool)),
				ctx: everyGrant(), turn: workTurn(t)},
			onKeyedRequest: {tool: operatorToolOver(t, operator(keyed), tool), ctx: person},
			onOperatorMCP:  {tool: operatorToolOver(t, operator(operatorActor), tool), ctx: person},
		},
		writes: func() (int, string) {
			if len(trk.opIDs) == 0 {
				return 0, ""
			}
			return len(trk.opIDs), trk.opIDs[len(trk.opIDs)-1]
		},
	}
}

// artifactFamily is the artifacts' writes, by a create.
func artifactFamily(t *testing.T, key string) familyRig {
	t.Helper()
	f := newFakeArtifacts()
	chart := artifactChart{}
	deps := artifactDeps(f, chart, requesters{})
	company := organization(t)
	deps.Org = func() *org.Organization { return company }
	keyed := deps
	keyed.CallerKey = func(context.Context) string { return key }
	person := iam.WithPrincipal(context.Background(), founderPrincipal(iam.Apps...))
	tool := builtin.CreateArtifactTool
	return familyRig{
		tool:  tool,
		one:   map[string]any{"kind": "doc", "title": "Notes", "content": "# hi"},
		other: map[string]any{"kind": "doc", "title": "Other notes", "content": "# hi"},
		on: map[bindSurface]familyTool{
			// THE GATE DERIVES THE SEAT FROM THE TURN, as it does for every
			// seat's call.
			onSeatTurn: {tool: asSeatCallable(t, registered(t, builtin.Deps{Artifacts: deps,
				Authorize: builtin.Decide(chart)}, tool)),
				ctx: context.Background(), turn: turnFor(t, "agent-cto")},
			onKeyedRequest: {tool: operatorToolOver(t, builtin.OperatorDeps{Artifacts: keyed,
				Authorize: builtin.Decide(chart)}, tool), ctx: person},
			onOperatorMCP: {tool: operatorToolOver(t, builtin.OperatorDeps{Artifacts: deps,
				Authorize: builtin.Decide(chart)}, tool), ctx: person},
		},
		writes: func() (int, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.creates) == 0 {
				return 0, ""
			}
			return len(f.creates), f.calls[len(f.calls)-1].OpKey
		},
	}
}

// datastoreFamily is the datastore's writes, by a row set on the founder's
// sheet, which is shared with the seat so the person and the seat write the
// same rows.
func datastoreFamily(t *testing.T, key string) familyRig {
	t.Helper()
	art, data := newFakeArtifacts(), newFakeData()
	chart := artifactChart{}
	sheet := art.seed("founder", iam.ActorHuman, founder,
		[]artifacts.Share{shareWith("agent-cto", artifacts.RoleEditor)},
		[2]string{"index.html", "<p>sheet</p>"})
	deps := artifactDeps(art, chart, requesters{})
	company := organization(t)
	deps.Org = func() *org.Organization { return company }
	keyed := deps
	keyed.CallerKey = func(context.Context) string { return key }
	row := func(n int) map[string]any {
		return map[string]any{"artifact": sheet.ID.String(), "ops": []any{map[string]any{
			"op": "set", "collection": "rows", "doc": "a", "fields": map[string]any{"n": n}}}}
	}
	person := iam.WithPrincipal(context.Background(), founderPrincipal(iam.Apps...))
	tool := builtin.WriteArtifactDataTool
	return familyRig{
		tool:  tool,
		one:   row(1),
		other: row(2),
		on: map[bindSurface]familyTool{
			onSeatTurn: {tool: asSeatCallable(t, registered(t, builtin.Deps{Artifacts: deps,
				ArtifactData: artifactDataDeps(data), Authorize: builtin.Decide(chart)}, tool)),
				ctx: context.Background(), turn: turnFor(t, "agent-cto")},
			onKeyedRequest: {tool: operatorToolOver(t, builtin.OperatorDeps{Artifacts: keyed,
				ArtifactData: artifactDataDeps(data), Authorize: builtin.Decide(chart)}, tool),
				ctx: person},
			onOperatorMCP: {tool: operatorToolOver(t, builtin.OperatorDeps{Artifacts: deps,
				ArtifactData: artifactDataDeps(data), Authorize: builtin.Decide(chart)}, tool),
				ctx: person},
		},
		writes: func() (int, string) {
			data.mu.Lock()
			defer data.mu.Unlock()
			if len(data.writes) == 0 {
				return 0, ""
			}
			return len(data.writes), data.calls[len(data.calls)-1].OpKey
		},
	}
}

// codeFamily is the code host's writes, by a branch created at a commit, made
// by agent-cto's turn on its own registry and by a person — through their own
// token, bound to the human seat they hold — on the operator's catalogue.
func codeFamily(t *testing.T, key string) familyRig {
	t.Helper()
	writer := &codeWrites{}
	deps := writeDeps(newCodeHost(), writer)
	operator := func(keyed bool) builtin.OperatorDeps {
		d := deps
		d.Actor = builtin.PrincipalActor
		if keyed {
			// THE ACT ROUTE'S ACTOR: the request's principal, carrying its
			// key and the instant the key was minted at.
			d.Actor = func(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
				actor, err := builtin.PrincipalActor(ctx, turn)
				actor.WorkKey = key
				actor.WorkSince, _ = statelog.OpMintedAt(key)
				return actor, err
			}
		}
		return builtin.OperatorDeps{Code: d, Authorize: builtin.Decide(chartLeads)}
	}
	tool := builtin.CodeCreateBranchTool
	return familyRig{
		tool:  tool,
		one:   map[string]any{"repo": "site", "branch": "seats/agent-cto/a", "at": mainOID},
		other: map[string]any{"repo": "site", "branch": "seats/agent-cto/b", "at": mainOID},
		on: map[bindSurface]familyTool{
			onSeatTurn: {tool: asSeatCallable(t, registered(t, builtin.Deps{Code: deps}, tool)),
				ctx: everyGrant(), turn: writerTurn(t)},
			onKeyedRequest: {tool: operatorToolOver(t, operator(true), tool), ctx: asBoundPerson()},
			onOperatorMCP:  {tool: operatorToolOver(t, operator(false), tool), ctx: asBoundPerson()},
		},
		writes: func() (int, string) {
			writes, _ := writer.asked()
			if len(writes) == 0 {
				return 0, ""
			}
			return len(writes), writes[len(writes)-1].OpID
		},
	}
}

// operatorToolOver is the operator catalogue's tool named name, over deps.
func operatorToolOver(t *testing.T, deps builtin.OperatorDeps, name string) tools.SeatCallable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(deps) {
		if tool.Name() == name {
			return asSeatCallable(t, tool)
		}
	}
	t.Fatalf("the operator catalogue serves no %s", name)
	return nil
}

// asSeatCallable is tool as a caller with a turn, or with none, calls it.
func asSeatCallable(t *testing.T, tool tools.Callable) tools.SeatCallable {
	t.Helper()
	seated, ok := tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s cannot know who called it", tool.Name())
	}
	return seated
}

// answeredOpOf is the op_id an answer carries, or "" where it carries none —
// a refusal included, which is prose.
func answeredOpOf(got tools.Result) string {
	var answer map[string]any
	if json.Unmarshal([]byte(got.Output), &answer) != nil {
		return ""
	}
	op, _ := answer["op_id"].(string)
	return op
}

// propertiesOf is the properties a tool's schema declares.
func propertiesOf(tool tools.Callable) map[string]any {
	props, _ := tool.Parameters()["properties"].(map[string]any)
	return props
}
