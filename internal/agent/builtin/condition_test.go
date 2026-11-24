package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
)

// conditionDetail is what a refusal says for whoever runs the node: a
// position, a stream setting and a subject, none of which a model or a person
// acts on.
const conditionDetail = "the write needs 3:4182 on crewlet.tracker.task.t-1; " +
	"raise stream.tracker_max_bytes"

// errTransport is a backend's own words beneath its mark: a host and a
// port, which belong in the node's log.
var errTransport = errors.New("nats: dial tcp 10.0.0.7:4222: connection refused")

// everyCondition is one error per condition [builtin.Condition] names, each
// wrapped as a domain or a backend wraps it: every write reason and read
// refusal of the state log, a bare state-log sentinel, and every other mark.
func everyCondition() map[string]error {
	out := map[string]error{
		"a bare state-log sentinel": fmt.Errorf("chatlet: %w", statelog.ErrUnavailable),
		"a coordination store": fmt.Errorf("read pins: %w: %w", coord.ErrUnavailable,
			errTransport),
		"an estate nobody served": fmt.Errorf("estate: tracker.task: %w",
			&estate.ErrUnserved{Detail: unservedDetail}),
		"a request no data node's build reads whole": fmt.Errorf("estate: tracker.create_task: %w",
			&estate.ErrUnknownKeys{Op: "tracker.create_task", Keys: []string{"args.Task.went_on"},
				Nodes: []string{"data-b"}, Unserved: &estate.ErrUnserved{Detail: unservedDetail}}),
		"an estate held closed": fmt.Errorf("worklet: %w", store.ErrNoEstate),
		"a busy store": fmt.Errorf("worklet: %w: %w", store.ErrBusy,
			errors.New("turso: error: database is locked")),
		"a queue not live": fmt.Errorf("publish: %w", queue.ErrNotLive),
		"a broker blip": fmt.Errorf("ask: %w: %w", queue.ErrUnavailable,
			errTransport),
		"a wait that ran out": fmt.Errorf("read: %w", context.DeadlineExceeded),
		"a wait abandoned":    fmt.Errorf("read: %w", context.Canceled),
		"a full file store": fmt.Errorf("objstore: store the object: natsobj: put files/x: "+
			"%w: bucket crewlet_files: %w", objstore.ErrFull, errTransport),
		"a file store that did not answer": fmt.Errorf("objstore: store the object: %w: %w",
			objstore.ErrUnanswered, errTransport),
		"a note the running node could not verify": fmt.Errorf("steer: ask: %w",
			steer.ErrUnverified),
		"a run's record naming its seat under a key this node lacks": fmt.Errorf(
			"steer: run-1's seat: %w", steer.ErrSeatKeyUnknown),
	}
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT, syscall.EROFS,
		syscall.EIO, syscall.EMFILE, syscall.ENFILE} {
		out["a host's "+errno.Error()] = fmt.Errorf("write the notes: %w",
			&os.PathError{Op: "write", Path: "/var/lib/crewlet/notes", Err: errno})
	}
	out["a store whose disk is full"] = fmt.Errorf("worklet: %w: no room", store.ErrFull)
	out["an executor's owner that could not answer"] = fmt.Errorf("query: %w",
		&sandbox.TailError{Node: "n2", TurnID: "t-1", Failure: sandbox.TailFailedUnanswered})
	for _, why := range sandbox.Whys {
		out["a sandbox "+string(why)] = fmt.Errorf("sandbox: read the box of run t-1: %w",
			sandbox.Because(why, errTransport))
	}
	for _, r := range statelog.Reasons() {
		out["write "+string(r)] = fmt.Errorf("worklet: decide: %w", &statelog.Unavailable{
			Reason: r, Detail: conditionDetail, OpID: "op-1"})
	}
	for _, c := range statelog.ReadRefusals {
		out["read "+string(c)] = fmt.Errorf("doclet: read: %w", &statelog.Refused{
			Code: c, Level: statelog.ReadLinearizable, Detail: conditionDetail,
			RetryAfter: statelog.RetryHint(c, 10, 1)})
	}
	return out
}

// EVERY CONDITION IS TOLD TO BOTH ITS READERS, AND TO NEITHER IN ITS OWN TEXT.
//
// A model reads a condition as a clause inside a tool's sentence
// ([builtin.Condition]), a person as a whole sentence ([builtin.ConditionSentence]);
// one classification, so neither names a condition the other does not. Neither
// carries the error (no package name, reason, position, setting, subject or
// host). The person's has no em dash or engine word for its parts, and asks
// for a try exactly where the answer carries a Retry-After.
//
// Mutation: drop an arm, answer a refusal's message
// ([statelog.Unavailable.Error]) or detail, add an em dash, or give advice that
// disagrees with the Retry-After, and the matching row goes red.
func TestAConditionIsToldToItsTwoReadersWithoutItsOwnText(t *testing.T) {
	t.Parallel()
	for name, err := range everyCondition() {
		clause, ok := builtin.Condition(err)
		person, said := builtin.ConditionSentence(err)
		if !ok || !said || clause == "" || person == "" {
			t.Errorf("%s is told %q (%v) to a model and %q (%v) to a person", name,
				clause, ok, person, said)
			continue
		}
		for _, leak := range []string{"statelog", "crewlet.", "3:4182", "tracker_max",
			"op-1", "10.0.0.7", "data-b", "turso", "_", "unavailable ("} {
			if strings.Contains(clause, leak) {
				t.Errorf("%s reaches a model with %q in it: %s", name, leak, clause)
			}
			if strings.Contains(person, leak) {
				t.Errorf("%s reaches a person with %q in it: %s", name, leak, person)
			}
		}
		lower := strings.ToLower(person)
		for _, internal := range []string{"—", "coordination", "broker", "queue",
			"estate", "snapshot", "store", "data node", "log", "peer"} {
			if strings.Contains(lower, internal) {
				t.Errorf("%s is told to a person with %q in it: %s", name, internal, person)
			}
		}
		// THE HINT IS THE ONE EVERY SURFACE SENDS BESIDE THESE WORDS
		// ([builtin.RetryAfter]), which reads the same classification, and
		// the words agree with its size: "in a moment" for a wait under a
		// minute, "in a few minutes" for one of a minute or more.
		hint := builtin.RetryAfter(err, time.Second)
		soon := strings.HasSuffix(person, statelog.TryAgainSoon)
		later := strings.HasSuffix(person, statelog.TryAgainLater)
		switch {
		case hint == 0 && (soon || later):
			t.Errorf("%s carries no Retry-After and tells a person %q", name, person)
		case hint > 0 && hint < time.Minute && !soon:
			t.Errorf("%s carries a Retry-After of %s and tells a person %q, want %q",
				name, hint, person, statelog.TryAgainSoon)
		case hint >= time.Minute && !later:
			t.Errorf("%s carries a Retry-After of %s and tells a person %q, want %q",
				name, hint, person, statelog.TryAgainLater)
		}
		if words, isRefusal := statelog.ConditionSentence(err); isRefusal && words != person {
			t.Errorf("%s is told to a person as %q, not in the state log's own "+
				"sentence %q", name, person, words)
		}
	}
	for _, fault := range []error{nil, errors.New("disk I/O error"),
		fmt.Errorf("worklet: read ENG-1: %w", errors.New("database is locked"))} {
		if clause, ok := builtin.Condition(fault); ok {
			t.Errorf("a fault %v is a condition to a model: %q", fault, clause)
		}
		if person, ok := builtin.ConditionSentence(fault); ok {
			t.Errorf("a fault %v is a condition to a person: %q", fault, person)
		}
	}
}

// EVERY CONDITION IS RECORDED UNDER A KIND OF ITS OWN. A failed phase or turn
// records its failure's kind off this classification ([builtin.ConditionKind]),
// and the one kind no condition may take is the unclassified one, which an
// issue report reads as the engine's own defect. A fault takes none.
//
// Mutation: leave a condition's kind empty, or spell one `error`, and its row
// goes red; give a fault a kind and the last loop does.
func TestEveryConditionIsRecordedUnderAKindOfItsOwn(t *testing.T) {
	t.Parallel()
	for name, err := range everyCondition() {
		kind, ok := builtin.ConditionKind(err)
		if !ok || kind == "" || kind == types.ErrorKindUnclassified {
			t.Errorf("%s is recorded under kind %q (%v)", name, kind, ok)
		}
	}
	for _, fault := range []error{nil, errors.New("disk I/O error")} {
		if kind, ok := builtin.ConditionKind(fault); ok {
			t.Errorf("a fault %v is recorded under the condition kind %q", fault, kind)
		}
	}
}

// A CONDITION NO WAIT CLEARS IS NOT RETRIED BY A MODEL. Told to try again
// whatever the condition, a model re-asked a log at its byte ceiling for the
// whole turn. The sentence ends on what a model can do: say it could not check
// or change, and why, never the person's advice (another node, an
// administrator), which a model cannot reach. Only a condition that clears on
// its own is offered the retry. A read and a write each, through the tools,
// beside a control that clears; other helpers are held to the same in
// TestEveryToolTellsAModelWhatAConditionNoWaitClearsLeavesIt.
//
// Mutation: answer every condition "Try again shortly", drop the next clause
// from the write's sentence, or offer the person's advice, and the full log's
// rows go red; offer the model's to a condition that clears and the control does.
func TestAConditionNoWaitClearsIsNotRetriedByAModel(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	read := func() tools.Result {
		return callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"})
	}
	write := func() tools.Result {
		return callWork(t, reg, builtin.UpdateWorkItemTool,
			map[string]any{"item": "ENG-1", "priority": "urgent"})
	}
	personal := func(said string) bool {
		return strings.Contains(said, "another node") ||
			strings.Contains(said, "administrator")
	}

	trk.readErr = &statelog.Refused{Code: statelog.RefuseLogFull,
		Level: statelog.ReadLinearizable, Detail: conditionDetail}
	got := read()
	if tools.RefusalOf(got) != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "say you could not check") ||
		strings.Contains(got.Output, "Try again") || personal(got.Output) {
		t.Errorf("a read refused on a full log is told %q (%s); want a model's own "+
			"advice and no retry", got.Output, tools.RefusalOf(got))
	}
	trk.readErr = nil
	trk.writeErr = fmt.Errorf("worklet: update ENG-1: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: conditionDetail})
	got = write()
	if tools.RefusalOf(got) != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "say it could not be done") || personal(got.Output) {
		t.Errorf("a write refused on a full log is told %q (%s); want a model's own advice",
			got.Output, tools.RefusalOf(got))
	}

	trk.writeErr = nil
	trk.readErr = &statelog.Refused{Code: statelog.RefuseBehind,
		Level: statelog.ReadLinearizable, Detail: conditionDetail, RetryAfter: time.Second}
	got = read()
	if !strings.Contains(got.Output, "Try again") || personal(got.Output) ||
		strings.Contains(got.Output, "won't clear") {
		t.Errorf("a read refused for a node behind is told %q; want the retry and "+
			"nothing for an administrator", got.Output)
	}
}

// EVERY SANDBOX CLASS IS A CONDITION, AND WAITS EXACTLY WHERE THE CLASS SAYS.
// A sandbox backend marks a failure by what clears it ([sandbox.Why]), and
// every reader outside a box tells it through this classification: unclassed,
// a box that did not answer was a fault of this node, a 500 nothing would
// change. The hint follows the class's word on whether a wait clears it, and
// the class wins over a deadline in its chain: a stuck configuration whose
// command also ran out of time is still a person's to fix.
//
// Mutation: drop a class from the classification, or mark a stuck class as
// one that waits (or the reverse), and its row goes red; read a deadline
// before the class and the last check does.
func TestEverySandboxClassIsACondition(t *testing.T) {
	t.Parallel()
	for _, why := range sandbox.Whys {
		err := sandbox.Because(why, errTransport)
		if _, ok := builtin.Condition(err); !ok {
			t.Errorf("a sandbox %s failure is not a condition", why)
			continue
		}
		if waits := builtin.RetryAfter(err, time.Second) > 0; waits != why.Waits() {
			t.Errorf("a sandbox %s failure carries a Retry-After: %v, but the class "+
				"says a wait clears it: %v", why, waits, why.Waits())
		}
	}
	timedOut := sandbox.Because(sandbox.WhyConfiguration,
		fmt.Errorf("container: create: %w", context.DeadlineExceeded))
	if builtin.RetryAfter(timedOut, time.Second) != 0 {
		t.Error("a configuration whose command also ran out of time was told to wait")
	}
}

// EVERY CONDITION KIND IS THE NODE'S OR THE WORK'S, AND NEVER BOTH, decided
// once ([builtin.InfrastructureKind]): the work's side is exactly worksKinds
// (a write the state log refused for what it is, a deadline, an abandoned
// wait) and any sandbox class sandboxClassSides leaves with the work, today
// none; every other declared kind and condition is the node's. The state log's
// two kinds hold for every reason it names, and a sandbox class added there
// fails until it has a side here (builtin's "Whose failure a condition is").
//
// Mutation: take a declared kind off InfrastructureKind's list, put a work's
// kind on it, read every state-log refusal under one kind, move a sandbox
// class to the work's side or read every class as the work's, or declare a
// kind no condition is recorded under, and its rows go red.
func TestEveryConditionKindIsTheNodesOrTheWorks(t *testing.T) {
	t.Parallel()
	works := func(kind string) bool {
		if class, ok := strings.CutPrefix(kind, builtin.SandboxKindPrefix); ok {
			nodes, decided := sandboxClassSides[sandbox.Why(class)]
			return decided && !nodes
		}
		return worksKinds[kind]
	}
	for _, why := range sandbox.Whys {
		if _, decided := sandboxClassSides[why]; !decided {
			t.Errorf("the sandbox class %q has no side decided here: say whether a mail can "+
				"cause it, here and in builtin's sandboxClassIsTheNodes", why)
		}
	}
	for why := range sandboxClassSides {
		if !why.Valid() {
			t.Errorf("%q is decided here and is not a class the sandbox names", why)
		}
	}
	recorded := map[string]bool{}
	for name, err := range everyCondition() {
		kind, ok := builtin.ConditionKind(err)
		if !ok {
			t.Errorf("%s is recorded under no kind", name)
			continue
		}
		recorded[kind] = true
		if nodes := builtin.InfrastructureKind(kind); nodes == works(kind) {
			t.Errorf("%s (kind %q): the node's = %v and the work's = %v, want exactly one",
				name, kind, nodes, works(kind))
		}
	}
	for _, reason := range statelog.Reasons() {
		err := fmt.Errorf("worklet: decide: %w", &statelog.Unavailable{Reason: reason, OpID: "op-1"})
		want := builtin.KindStateLogUnavailable
		if reason.RefusesTheWrite() {
			want = builtin.KindStateLogWriteRefused
		}
		if kind, _ := builtin.ConditionKind(err); kind != want {
			t.Errorf("a write refused %s is recorded under %q, want %q", reason, kind, want)
		}
	}
	for _, kind := range declaredKinds(t) {
		if !recorded[kind] {
			t.Errorf("the declared kind %q is recorded for no condition", kind)
		}
		if builtin.InfrastructureKind(kind) == worksKinds[kind] {
			t.Errorf("the declared kind %q: the node's = %v, want %v", kind,
				builtin.InfrastructureKind(kind), !worksKinds[kind])
		}
	}
	for _, kind := range []string{"", types.ErrorKindUnclassified, "sandbox"} {
		if builtin.InfrastructureKind(kind) {
			t.Errorf("%q, no kind of a condition, is the node's", kind)
		}
	}
}

// ONLY A CONDITION AN OPERATOR MUST CLEAR IS ONE A FINISHED JOB'S RESUME WAITS
// FOR THE OPERATOR ON: a disk full, read-only or failing, a process at its
// limit of open files, and nodes holding different keys or running different
// builds, each of them the node's too; and nothing a store, a log or a broker
// answering again clears, nothing of the work's, and no kind at all. Raised as
// executor_resume_blocked, a kind put on the wrong side either pages an
// operator for an outage that ends by itself or leaves a run waiting on one
// unsaid.
//
// Mutation: drop a kind from builtin.OperatorKind, or add one a wait clears
// (coordination_unavailable), and its row goes red.
func TestOnlyAConditionAnOperatorClearsWaitsForTheOperator(t *testing.T) {
	t.Parallel()
	operators := map[string]bool{
		builtin.KindDiskFull: true, builtin.KindDiskReadOnly: true, builtin.KindDiskFailed: true,
		builtin.KindOpenFilesExhausted: true, builtin.KindFleetKeysDiffer: true,
		builtin.KindFleetBuildsDiffer: true,
	}
	seen := map[string]bool{}
	for name, err := range everyCondition() {
		kind, _ := builtin.ConditionKind(err)
		seen[kind] = true
		if got := builtin.OperatorKind(kind); got != operators[kind] {
			t.Errorf("%s (kind %q): waits for an operator = %v, want %v", name, kind, got, operators[kind])
		}
	}
	for kind := range operators {
		if !seen[kind] {
			t.Errorf("the operator's kind %q is recorded for no condition", kind)
		}
		if !builtin.InfrastructureKind(kind) {
			t.Errorf("the operator's kind %q is not the node's", kind)
		}
	}
	for _, kind := range []string{"", types.ErrorKindUnclassified, builtin.KindCoordinationUnavailable} {
		if builtin.OperatorKind(kind) {
			t.Errorf("%q waits for an operator", kind)
		}
	}
}

// sandboxClassSides is every coding sandbox class's side, true for the node's:
// a box or runtime that did not answer, a setup download failing on the far
// side and a configuration the deployment cannot start are no mail's doing.
var sandboxClassSides = map[sandbox.Why]bool{
	sandbox.WhyProviderUnavailable: true,
	sandbox.WhySetupUnavailable:    true,
	sandbox.WhyConfiguration:       true,
}

// worksKinds is every kind outside a coding sandbox's classes that is the
// work's rather than the node's: each one what was asked can cause.
var worksKinds = map[string]bool{
	builtin.KindStateLogWriteRefused: true,
	llm.KindTimeout.String():         true,
	llm.KindCancelled.String():       true,
}

// declaredKinds is every kind constant conditionkind.go declares, read from
// its source so a kind added there is held to a side without being listed
// here too.
func declaredKinds(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "conditionkind.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Kind") || i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				kind, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, kind)
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatal("conditionkind.go declares no kind: the walk read nothing")
	}
	return kinds
}
