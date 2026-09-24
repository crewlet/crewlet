package builtin

import (
	"encoding/json"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// What a turn's tracker writes are named, and why each name is what it is.
//
// # One name per write, re-derived by a redelivery
//
// Every write a tool here makes carries an OPERATION ID, and the tracker's
// operation ledger answers a write under an id it has already applied
// `applied`, at the first write's position, without writing anything. That is
// what makes a redelivered turn safe — it re-issues the same calls, and each
// collapses into the write its first attempt landed — and it is exactly as
// dangerous as it is useful: a SECOND write in the same turn under the same id
// is collapsed the same way, for as long as the ledger keeps the row
// ([statelog.OpsRetention]), and its change is dropped while the call reports
// success.
//
// So an id names the turn AND the write's place in it. The turn is its seed
// ([Actor.OperationSeed]): the work key, which a redelivery reproduces, or the
// run where there is none. The place is counted within a BASE — the same verb
// on the same object, `<seed>-update-<task>` — over [turnctx.Turn.Calls],
// which is every closed round's calls and every call before this one in the
// phase that is running: the first write under a base is named the base
// itself, and the n-th the base with `#n` after it. Strictly, each write takes
// the first of those names no earlier write in the turn holds, which is the
// same thing whenever nothing else holds one.
//
// A turn making the same calls in the same order therefore derives the same
// ids, and two writes to one object in one turn are two writes.
//
// # The names are read off the receipts
//
// A write's base holds the object it resolved — a task's ID, never the key the
// model typed, which a cross-project move changes — so the names cannot be
// re-derived from the arguments. Every write therefore says what it was
// written under, in the `operations` of its answer, and the next write reads
// those answers back. A write that named its operations and then did not land
// says so too, in the same field of a failed answer, and is counted with the
// rest: a redelivery whose earlier attempt DID land that write must still name
// every later write the way the earlier attempt did.
//
// # A create has no base to repeat
//
// Its object is the task it files, whose id is DERIVED from the seed, the
// author and how many creates came before it in the turn, so a redelivery
// files the same task rather than a second one.
//
// # Between builds
//
// A rolling upgrade puts this build on one ledger with builds that name every
// write a seat makes in a turn under its base — the name this build gives the
// FIRST write — and draw a create's task id and a project setting's operation
// id at random. For every write but those two, a redelivery that crosses such
// builds lands nothing twice in either direction: the first write to an
// object is named alike by both, and each later one either never landed under
// the other build's name, whose ledger collapsed it into the first, or lands
// once under this build's. A create or a project setting such a build made is
// one no later attempt can re-derive, and a redelivery that crosses builds
// makes it again.
//
// # Outside a turn
//
// There is no seed, and every id is fresh: an operator's client made one
// call, and nothing will redeliver it.

// operationsField is the answer field every write here reports its operation
// ids in, and the one [operationsBefore] reads back.
const operationsField = "operations"

// sequencedTools are the tools whose answers carry [operationsField], and
// therefore the only answers [operationsBefore] reads. Each implements
// [tools.Sequenced], so a surface never runs two of them at once;
// operation_test.go holds this list against the registered tools that do.
var sequencedTools = []string{
	CreateWorkItemTool, UpdateWorkItemTool, CommentOnWorkTool,
	tracker.MergeWorkItemTool, tracker.RemoveWorkItemTool,
	tracker.RestoreWorkItemTool, tracker.WriteProjectTool,
	tracker.SaveWorkViewTool, tracker.WriteWorkGoalTool,
	tracker.SetPrioritiesTool, tracker.SetPinsTool, tracker.MarkInboxTool,
}

// operations are the ids the writes before a call in its turn were made under.
type operations map[string]bool

// operationsBefore reads them off the answers of every call before this one in
// the turn. Empty outside a turn, where nothing is counted.
func operationsBefore(turn *turnctx.Turn) operations {
	out := operations{}
	for _, call := range turn.Calls() {
		if !slices.Contains(sequencedTools, call.Name) {
			continue
		}
		var answer struct {
			Operations []string `json:"operations"`
		}
		// AN ANSWER THAT IS NOT A RECEIPT NAMES NO OPERATION: a refusal made
		// before anything was named is prose.
		if json.Unmarshal([]byte(call.Result), &answer) != nil {
			continue
		}
		for _, id := range answer.Operations {
			out[id] = true
		}
	}
	return out
}

// next names the next write under base: the first of base, base#2, base#3 …
// that no earlier write in the turn holds.
func (o operations) next(base string) string {
	for n := 1; ; n++ {
		if id := nth(base, n); !o[id] {
			return id
		}
	}
}

// nth is the n-th name under base: base itself for the first.
func nth(base string, n int) string {
	if n <= 1 {
		return base
	}
	return base + "#" + strconv.Itoa(n)
}

// opIDFor is the operation id one write is made under: see the file head.
//
// OUTSIDE A TURN IT IS FRESH PER CALL. It keeps the verb and the object ahead
// of the part that makes it unique, so a stuck operation in the ledger still
// says what it was.
func opIDFor(actor Actor, before operations, verb, object string) string {
	seed := actor.OperationSeed()
	if seed == "" {
		return verb + "-" + object + "-" + uuid.NewString()
	}
	return before.next(seed + "-" + verb + "-" + object)
}

// createFor is the id a create files its task under, and the operation it
// files it in.
//
// DERIVED IN A TURN — from the seed, the author and the create's place among
// the turn's creates — so a redelivered turn files the same task under the
// same operation rather than a second one. The author is in it because a work
// key is derived from events alone, and nothing makes an event one seat's.
//
// Fresh outside a turn, where nothing redelivers.
func createFor(actor Actor, before operations) (taskID, opID string) {
	seed := actor.OperationSeed()
	if seed == "" {
		taskID = uuid.NewString()
		return taskID, opIDFor(actor, before, "create", taskID)
	}
	for k := 1; ; k++ {
		taskID = uuid.NewSHA1(taskNamespace,
			[]byte(seed+"\x00"+actor.Handle+"\x00"+strconv.Itoa(k))).String()
		if opID = seed + "-create-" + taskID; !before[opID] {
			return taskID, opID
		}
	}
}

// taskNamespace is the uuid namespace a turn's task ids are derived under.
// Fixed for the life of the format: a new one would make every redelivered
// create file its task a second time.
var taskNamespace = uuid.MustParse("3b2f0c5e-8d4a-5e71-9c36-0f1d2a7b6e48")

// commentFor is a comment's own id and the operation it is written under.
//
// The id is a PRIMARY KEY on every node — two nodes applying one record must
// write one row, so an id generated at apply time would produce two — and it
// is derived for [opIDFor]'s reason. The first comment a seat makes on a task
// in a turn is derived from the seed, the task and the seat alone, which is
// the id a build naming every write under its base gives every such comment;
// each later one is derived from its place as well, and its operation is named
// under the first comment's.
func commentFor(actor Actor, before operations, taskID string) (id, opID string) {
	seed := actor.OperationSeed()
	if seed == "" {
		id = uuid.NewString()
		return id, opIDFor(actor, before, "comment", id)
	}
	name := seed + "\x00" + taskID + "\x00" + actor.Handle
	first := uuid.NewSHA1(commentNamespace, []byte(name)).String()
	base := seed + "-comment-" + first
	for n := 1; ; n++ {
		if opID = nth(base, n); before[opID] {
			continue
		}
		if n == 1 {
			return first, opID
		}
		return uuid.NewSHA1(commentNamespace,
			[]byte(name+"\x00"+strconv.Itoa(n))).String(), opID
	}
}

// commentNamespace is the uuid namespace comment ids are derived under. Fixed
// for the life of the format: it is durable in every comment row.
var commentNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// callKey is the operation id of ONE call that writes a person's own record:
// prefix, then the turn's seed with the call's place after it in a turn, or a
// fresh value outside one.
//
// AN OPERATOR HAS NO TURN and no redelivery: their client made one call, so
// there is nothing to deduplicate against and two calls in one session are two
// writes, which is what the caller meant. In a turn, two calls are two writes
// as well, and a redelivered turn names both again.
func callKey(actor Actor, before operations, prefix string) string {
	seed := actor.OperationSeed()
	if seed == "" {
		return prefix + "operator-" + uuid.NewString()
	}
	return before.next(prefix + seed)
}

// receipt is a write's answer, carrying the operations it was made under.
func receipt(answer map[string]any, ops ...string) (tools.Result, error) {
	answer[operationsField] = ops
	return jsonResult(answer)
}

// refusedAfter is a write that named its operations and then did not land: the
// refusal the model reads, with the operations a later write in the turn is
// named after. See the file head for why those count. With none named it is
// an ordinary refusal.
func refusedAfter(refusal string, ops ...string) tools.Result {
	if len(ops) == 0 {
		return failed(refusal)
	}
	data, err := json.MarshalIndent(map[string]any{
		"error": refusal, operationsField: ops,
	}, "", "  ")
	if err != nil {
		return failed(refusal)
	}
	return tools.Result{Output: string(data), Failed: true}
}
