package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the fixtures ------------------------------------------------------------ //

// openArgs is an open of feature into site's default branch, asking agent-ceo
// and the founder for review.
func openArgs() map[string]any {
	return map[string]any{"repo": "site", "branch": "feature", "title": "Retry the upload",
		"reviewers": []any{"agent-ceo", "founder"}}
}

// changeWriteDeps is the code tools over host with writer as Gitlet's
// writes, and tasks as the tracker's read of one task.
func changeWriteDeps(host builtin.CodeReader, writer *codeWrites, tasks builtin.TaskReader) builtin.CodeDeps {
	deps := writeDeps(host, writer)
	deps.Tasks = tasks
	return deps
}

// taskBook is the engine's tracker as a key is resolved: the tasks it holds,
// each by its key and its id.
type taskBook []worklet.Task

func (b taskBook) Task(_ context.Context, idOrKey string, _ worklet.DetailWants,
	_ statelog.Freshness) (worklet.TaskDetail, error) {

	for _, t := range b {
		if t.Key == idOrKey || t.ID == idOrKey {
			return worklet.TaskDetail{Task: t}, nil
		}
	}
	return worklet.TaskDetail{}, fmt.Errorf("%w: %s", worklet.ErrNoTask, idOrKey)
}

// countedTasks is the engine's tracker as a key is resolved, counting the
// reads it is asked — for a call that must judge its arguments before it
// reads anything.
type countedTasks struct {
	book taskBook
	n    atomic.Int32
}

func (c *countedTasks) Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {

	c.n.Add(1)
	return c.book.Task(ctx, idOrKey, want, fresh)
}

// reads is how many reads c was asked.
func (c *countedTasks) reads() int { return int(c.n.Load()) }

// decoded is a tool's answer as a JSON object.
func decoded(t *testing.T, got tools.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
		t.Fatalf("the answer is no JSON object: %v\n%s", err, got.Output)
	}
	return out
}

// ---- the list and the surfaces ---------------------------------------------- //

// A CHANGE WRITE DELIVERS TO CODE: a turn woken about a change that answered
// by opening, editing or closing one delivered, on Gitlet's own surface
// — the surface the code feed's wakes come from — and each is a write to a
// shared surface, so no worker acting under its parent's name is given one.
//
// Mutation: the delivery switch leaving the change writes out of CodeWrites →
// they deliver nowhere, red.
func TestAChangeWriteDeliversToCode(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newGitletHost(), &codeWrites{}))
	if len(builtin.ChangeWrites()) == 0 {
		t.Fatal("there are no change writes, so this certifies nothing")
	}
	for _, name := range builtin.ChangeWrites() {
		if !slices.Contains(builtin.CodeWrites(), name) {
			t.Errorf("%s is not one of the code writes", name)
		}
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("a seat's registry holds no %s", name)
			continue
		}
		if entry.Surface != gitlet.Source {
			t.Errorf("%s delivers to %q, want Gitlet's surface %q", name, entry.Surface, gitlet.Source)
		}
		if !mcp.WritesToSharedSurface(entry.Annotations) || mcp.ReadOnlyProven(entry.Annotations) {
			t.Errorf("%s is annotated %+v, want a write to a shared surface", name, entry.Annotations)
		}
	}
}

// ---- code_open_change ----------------------------------------------------- //

// AN OPEN REQUESTS ITS REVIEWERS AS A STEP: the change is opened under the
// call's operation — its id derived from it — and the reviewers it names are
// asked by a SECOND write, a step of that operation, of the change the open
// answered, decided at or past the open's own record; the same call made again
// is the same two operations, so the ledger collapses both. An open the
// committer refuses asks nobody, and an ask refused leaves the change open and
// says so on the answer, never as the open's failure.
//
// Mutations: the ask minted an operation of its own → the repeat is a second
// ask, red; the ask sent after a refused open → red; the ask decided below the
// open's record → red.
func TestOpenChangeRequestsItsReviewersAsAStep(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	if got := callWrite(everyGrant(), t, reg, turn, builtin.CodeOpenChangeTool, openArgs()); got.Failed {
		t.Fatalf("the open answered %q", got.Output)
	}
	changes, converses := writer.changesAsked()
	if len(changes) != 1 || changes[0].Open == nil || len(converses) != 1 || converses[0].Reviewers == nil {
		t.Fatalf("the open sent %+v and %+v, want one open and one ask", changes, converses)
	}
	open, ask := changes[0], converses[0]
	if open.Open.ChangeID != gitlet.OpenChangeID(open.OpID) || open.Open.SourceRef != "refs/heads/feature" ||
		open.Open.TargetRef != "refs/heads/main" || open.Open.TurnID != turn.RunID {
		t.Errorf("the open is %+v under %s", open.Open, open.OpID)
	}
	if !strings.HasPrefix(ask.OpID, open.OpID+".") || ask.OpID == open.OpID {
		t.Errorf("the ask's operation %s is no step of the open's %s", ask.OpID, open.OpID)
	}
	var asked []string
	for _, r := range ask.Reviewers.Request {
		asked = append(asked, r.Handle)
	}
	if ask.Reviewers.ChangeID != open.Open.ChangeID || ask.Reviewers.RepoID != siteID ||
		!slices.Equal(asked, []string{"agent-ceo", "founder"}) || ask.Asker != open.Asker ||
		ask.Seen != codeAt(15) {
		t.Errorf("the ask is %+v by %+v at %s, want the opened change's reviewers asked by its opener at the "+
			"open's own record (%s)", ask.Reviewers, ask.Asker, ask.Seen, codeAt(15))
	}

	// THE SAME CALL AGAIN is the same two operations.
	callWrite(everyGrant(), t, reg, turn, builtin.CodeOpenChangeTool, openArgs())
	changes, converses = writer.changesAsked()
	if len(changes) != 2 || len(converses) != 2 || changes[1].OpID != open.OpID || converses[1].OpID != ask.OpID {
		t.Errorf("the open made again is %d opens and %d asks under %v and %v, want the first's two operations",
			len(changes), len(converses), opIDsOf(changes), opIDsOfConverse(converses))
	}

	// AN OPEN REFUSED ASKS NOBODY.
	refusing := &codeWrites{change: func(gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
		return gitlet.ChangeResult{Refusal: gitlet.ChangeRefusalChangeExists,
			Detail: "refs/heads/feature heads site!3 already"}, nil
	}}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), refusing)), writerTurn(t),
		builtin.CodeOpenChangeTool, openArgs())
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalExists || !strings.Contains(got.Output, "site!3") {
		t.Errorf("a refused open answered %q as %s", got.Output, tools.RefusalOf(got))
	}
	if _, asks := refusing.changesAsked(); len(asks) != 0 {
		t.Errorf("a refused open asked %+v", asks)
	}

	// AN ASK REFUSED LEAVES THE CHANGE OPEN, and says so.
	unasked := &codeWrites{converse: func(gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
		return gitlet.ConversationResult{Refusal: gitlet.ConversationRefusalNotPermitted,
			Detail: "agent-ceo reviews nothing of site"}, nil
	}}
	got = callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), unasked)), writerTurn(t),
		builtin.CodeOpenChangeTool, openArgs())
	answer := decoded(t, got)
	said, _ := answer["reviewers_not_asked"].(string)
	if got.Failed || answer["change"] != "site!7" || !strings.Contains(said, "WAS opened") ||
		!strings.Contains(said, "agent-ceo reviews nothing of site") || answer["reviewers"] != nil {
		t.Errorf("an open whose ask was refused answered %q", got.Output)
	}
}

// opIDsOf and opIDsOfConverse are the operations a list of writes was made
// under.
func opIDsOf(ps []gitlet.ChangeProposal) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.OpID
	}
	return out
}

func opIDsOfConverse(ws []gitlet.ConversationWrite) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.OpID
	}
	return out
}

// AN OPEN CARRIES THE TURN'S WORK ITEM UNLESS IT NAMES ONE: the item the turn
// is charged to by default; another tracker's by its identity,
// `<backend>:<id>`, as named; a Worklet item's key resolved by Worklet's own
// read to the item's identity; and `none` carrying nothing. A key nothing
// resolves — no task by it, or no Worklet on this surface — refuses the call
// before anything is written.
//
// Mutations: an explicit item ignored for the turn's; a key carried
// unresolved as the item's id → red.
func TestAnOpenCarriesTheTurnsWorkItemUnlessItNamesOne(t *testing.T) {
	t.Parallel()
	turnItem := types.WorkItem{Backend: types.WorkWorklet, ID: "t-7", Key: "ENG-7", Project: "ENG"}
	tasks := taskBook{{ID: "t-9", Key: "ENG-9", Project: "ENG"}}
	for _, c := range []struct {
		name  string
		named any // the call's work_item, nil for none
		tasks builtin.TaskReader
		want  *types.WorkItem
		class tools.Refusal // the refusal, "" for an open
	}{
		{name: "the turn's own", want: &turnItem},
		{name: "another tracker's by its identity", named: "jira:10042",
			want: &types.WorkItem{Backend: types.WorkJira, ID: "10042"}},
		{name: "a Worklet key", named: "ENG-9", tasks: tasks,
			want: &types.WorkItem{Backend: types.WorkWorklet, ID: "t-9", Key: "ENG-9", Project: "ENG"}},
		{name: "a Worklet identity", named: "native:t-9", tasks: tasks,
			want: &types.WorkItem{Backend: types.WorkWorklet, ID: "t-9", Key: "ENG-9", Project: "ENG"}},
		{name: "none", named: "none"},
		{name: "a key no task has", named: "ENG-404", tasks: tasks, class: tools.RefusalInvalid},
		{name: "a key with no tracker to resolve it", named: "ENG-9", class: tools.RefusalInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{}
			reg := codeRegistry(t, changeWriteDeps(newGitletHost(), writer, c.tasks))
			turn := writerTurn(t)
			turn.WorkItem = &turnItem
			args := map[string]any{"repo": "site", "branch": "feature", "title": "Retry the upload"}
			if c.named != nil {
				args["work_item"] = c.named
			}
			got := callWrite(everyGrant(), t, reg, turn, builtin.CodeOpenChangeTool, args)
			changes, _ := writer.changesAsked()
			if c.class != "" {
				if !got.Failed || tools.RefusalOf(got) != c.class || len(changes) != 0 {
					t.Errorf("answered %q as %s with %d opens, want a %s refusal and none", got.Output,
						tools.RefusalOf(got), len(changes), c.class)
				}
				return
			}
			if got.Failed || len(changes) != 1 {
				t.Fatalf("answered %q with %d opens", got.Output, len(changes))
			}
			if gotItem := changes[0].Open.WorkItem; (gotItem == nil) != (c.want == nil) ||
				(gotItem != nil && *gotItem != *c.want) {
				t.Errorf("the open carries %+v, want %+v", gotItem, c.want)
			}
		})
	}
}

// ---- writes of an existing change -------------------------------------------- //

// A CHANGE WRITE IS HELD TO THE VERSION ITS CALLER READ: an edit, a draft's
// state, a retarget, a close and a reopen each name the change by the id the
// call's ONE read of it answers, carry the caller's expected_version, and are
// decided at or past that read — and a call that names no version, or more
// than one thing to change, is refused before anything is read.
//
// Mutations: the write decided at the turn's resolution of the repository
// rather than the read of the change; expected_version taken from the read →
// red.
func TestAChangeWriteIsHeldToTheVersionItsCallerRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		tool string
		args map[string]any
		is   func(p gitlet.ChangeProposal) bool
	}{
		{name: "an edit", tool: builtin.CodeUpdateChangeTool,
			args: map[string]any{"change": "site!7", "expected_version": 4, "title": "Retry twice", "body": ""},
			is: func(p gitlet.ChangeProposal) bool {
				return p.Edit != nil && *p.Edit.Title == "Retry twice" && p.Edit.Body != nil && *p.Edit.Body == ""
			}},
		{name: "a draft's state", tool: builtin.CodeUpdateChangeTool,
			args: map[string]any{"repo": "site", "change": 7.0, "expected_version": 4, "draft": false},
			is:   func(p gitlet.ChangeProposal) bool { return p.Draft != nil && !p.Draft.Draft }},
		{name: "a retarget", tool: builtin.CodeUpdateChangeTool,
			args: map[string]any{"change": "site!7", "expected_version": 4, "target": "release"},
			is: func(p gitlet.ChangeProposal) bool {
				return p.Retarget != nil && p.Retarget.TargetRef == "refs/heads/release"
			}},
		{name: "a close", tool: builtin.CodeCloseChangeTool,
			args: map[string]any{"change": "site!7", "expected_version": 4, "reason": "superseded by site!8"},
			is:   func(p gitlet.ChangeProposal) bool { return p.Close != nil && p.Close.Reason == "superseded by site!8" }},
		{name: "a reopen", tool: builtin.CodeCloseChangeTool,
			args: map[string]any{"change": "site!7", "expected_version": 4, "reopen": true},
			is:   func(p gitlet.ChangeProposal) bool { return p.Reopen != nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{}
			host := newChangeHost(siteChange())
			got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t), c.tool, c.args)
			changes, _ := writer.changesAsked()
			if got.Failed || len(changes) != 1 {
				t.Fatalf("answered %q with %d writes", got.Output, len(changes))
			}
			p := changes[0]
			if !c.is(p) || p.ChangeID() != siteChangeID || p.RepoID() != siteID || p.ExpectedVersion != 4 ||
				p.Seen != codeAt(40) {
				t.Errorf("the write is %+v of %s at %s expecting %d, want change site!7 at the read's %s "+
					"expecting 4", p, p.ChangeID(), p.Seen, p.ExpectedVersion, codeAt(40))
			}
			if answer := decoded(t, got); answer["change"] != "site!7" || answer["version"] != 5.0 {
				t.Errorf("answered %q, want site!7 at its next version", got.Output)
			}
		})
	}
	for _, c := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"no version", builtin.CodeUpdateChangeTool, map[string]any{"change": "site!7", "title": "x"}},
		{"two things at once", builtin.CodeUpdateChangeTool,
			map[string]any{"change": "site!7", "expected_version": 4, "title": "x", "draft": true}},
		{"nothing to change", builtin.CodeUpdateChangeTool, map[string]any{"change": "site!7", "expected_version": 4}},
		{"a reopen with a reason", builtin.CodeCloseChangeTool,
			map[string]any{"change": "site!7", "expected_version": 4, "reopen": true, "reason": "x"}},
	} {
		host, writer := newChangeHost(siteChange()), &codeWrites{}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t), c.tool, c.args)
		reads, _, _ := host.asked()
		changes, _ := writer.changesAsked()
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || len(reads)+len(changes) != 0 {
			t.Errorf("%s answered %q as %s after %d reads and %d writes, want an invalid call and nothing sent",
				c.name, got.Output, tools.RefusalOf(got), len(reads), len(changes))
		}
	}
}

// AN UPDATE ASKING TWO THINGS READS NO ITEM: code_update_change counts what a
// call asks from its arguments' presence before it resolves the work item one
// names, so a call naming a Worklet key beside `draft` or a `target` is
// refused as changing more than ONE thing without a read of the tracker — and
// a key naming no task does not hide that refusal behind its own. The same key
// asked alone is read once and carried by the edit.
//
// Mutation: the item resolved before the ask is counted → the tracker is read
// and a key naming no task answers as one, red.
func TestAnUpdateAskingTwoThingsReadsNoItem(t *testing.T) {
	t.Parallel()
	book := taskBook{{ID: "t-9", Key: "ENG-9", Project: "ENG"}}
	for name, args := range map[string]map[string]any{
		"a draft and a key":              {"draft": true, "work_item": "ENG-9"},
		"a title, a target and a key":    {"title": "x", "target": "release", "work_item": "ENG-9"},
		"a draft and a key with no task": {"draft": true, "work_item": "ENG-404"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host, writer, tasks := newChangeHost(siteChange()), &codeWrites{}, &countedTasks{book: book}
			args["change"], args["expected_version"] = "site!7", 4
			got := callWrite(everyGrant(), t, codeRegistry(t, changeWriteDeps(host, writer, tasks)), writerTurn(t),
				builtin.CodeUpdateChangeTool, args)
			reads, _, _ := host.asked()
			changes, _ := writer.changesAsked()
			if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || !strings.Contains(got.Output, "ONE thing") {
				t.Errorf("answered %q as %s, want the call refused for changing more than ONE thing", got.Output,
					tools.RefusalOf(got))
			}
			if n := tasks.reads(); n+len(reads)+len(changes) != 0 {
				t.Errorf("the refused call made %d tracker reads, %d code reads and %d writes, want none", n,
					len(reads), len(changes))
			}
		})
	}

	host, writer, tasks := newChangeHost(siteChange()), &codeWrites{}, &countedTasks{book: book}
	got := callWrite(everyGrant(), t, codeRegistry(t, changeWriteDeps(host, writer, tasks)), writerTurn(t),
		builtin.CodeUpdateChangeTool, map[string]any{"change": "site!7", "expected_version": 4, "work_item": "ENG-9"})
	changes, _ := writer.changesAsked()
	if got.Failed || len(changes) != 1 || changes[0].Edit == nil || changes[0].Edit.WorkItem == nil ||
		changes[0].Edit.WorkItem.ID != "t-9" || tasks.reads() != 1 {
		t.Errorf("the key alone answered %q with %+v after %d tracker reads, want one edit carrying t-9 after one",
			got.Output, changes, tasks.reads())
	}
}

// EVERY CHANGE REFUSAL IS IN AN EXISTING CLASS, by the rule a push's are: a
// rule of the repository or the change refusing the caller forbidden, a
// change that moved stale, a branch already heading a live change exists,
// branches no change can be about invalid, the change or its repository in a
// state that refuses the write until somebody changes it (closed, merged,
// archived, a pull mirror) conflict, what is gone not found, and what waiting
// clears unavailable — never the engine's own fault — each answered with the
// decide's own sentence.
//
// Mutation: drop change_exists from changeClass → red; answer repo_mirrored
// unavailable, as an import's wait is → red; answer repo_archived forbidden
// again, as a rule → red.
func TestEveryChangeRefusalIsInAnExistingClass(t *testing.T) {
	t.Parallel()
	want := map[gitlet.ChangeRefusal]tools.Refusal{
		gitlet.ChangeRefusalNotPermitted: tools.RefusalForbidden, gitlet.ChangeRefusalRepoArchived: tools.RefusalConflict,
		gitlet.ChangeRefusalStale: tools.RefusalStaleVersion, gitlet.ChangeRefusalChangeExists: tools.RefusalExists,
		gitlet.ChangeRefusalNoSource: tools.RefusalInvalid, gitlet.ChangeRefusalNoDifference: tools.RefusalInvalid,
		gitlet.ChangeRefusalChangeClosed: tools.RefusalConflict, gitlet.ChangeRefusalChangeMerged: tools.RefusalConflict,
		gitlet.ChangeRefusalRepoDeleted: tools.RefusalNotFound, gitlet.ChangeRefusalChangeNotFound: tools.RefusalNotFound,
		gitlet.ChangeRefusalBusy: tools.RefusalUnavailable, gitlet.ChangeRefusalUnavailable: tools.RefusalUnavailable,
		gitlet.ChangeRefusalRepoMirrored: tools.RefusalConflict, gitlet.ChangeRefusalRepoImporting: tools.RefusalUnavailable,
	}
	for _, refusal := range gitlet.ChangeRefusals() {
		writer := &codeWrites{change: func(gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
			return gitlet.ChangeResult{Refusal: refusal, Detail: "the " + string(refusal) + " sentence"}, nil
		}}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), writer)), writerTurn(t),
			builtin.CodeOpenChangeTool, map[string]any{"repo": "site", "branch": "feature", "title": "t"})
		class := tools.RefusalOf(got)
		switch {
		case !got.Failed || class == tools.RefusalInternalError || class == "":
			t.Errorf("a change refused %s answered %q as %q", refusal, got.Output, class)
		case want[refusal] != "" && class != want[refusal]:
			t.Errorf("a change refused %s is answered %s, want %s", refusal, class, want[refusal])
		case !strings.Contains(got.Output, "the "+string(refusal)+" sentence"):
			t.Errorf("a change refused %s answered %q without its sentence", refusal, got.Output)
		}
	}
}

// ---- the act route and the bound --------------------------------------------- //

// AN UNBOUND TOKEN CANNOT OPEN A CHANGE: on the act route — a request carrying
// its own operation key, a person's dashboard pressing the tool — a credential
// bound to nobody is refused every change write in Gitlet's sentence
// before anything is read or routed, and reads a change all the same; a person
// bound to their human seat opens one as that seat, under the operation the
// request's key derives.
//
// Mutation: drop writeCaller's unbound check → the token's open reaches the
// committer, red.
func TestAnUnboundTokenCannotOpenAChange(t *testing.T) {
	t.Parallel()
	key := statelog.NewOpID(time.Now(), "request")
	host, writer := newChangeHost(siteChange()), &codeWrites{}
	deps := writeDeps(host, writer)
	deps.Actor = func(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
		actor, err := builtin.PrincipalActor(ctx, turn)
		actor.WorkKey = key
		actor.WorkSince, _ = statelog.OpMintedAt(key)
		return actor, err
	}
	operator := builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)}
	for _, name := range builtin.ChangeWrites() {
		args, _ := holdingCall(name)
		args["repo"] = "site"
		got, err := operatorToolOver(t, operator, name).CallForTurn(asUnboundToken(), nil, args)
		if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
			!strings.Contains(got.Output, gitlet.UnboundDetail) {
			t.Errorf("%s by a token bound to nobody answered %v %q", name, err, got.Output)
		}
	}
	reads, _, _ := host.asked()
	changes, converses := writer.changesAsked()
	if len(reads)+len(changes)+len(converses) != 0 {
		t.Errorf("an unbound token's change writes sent %d reads and %d writes", len(reads),
			len(changes)+len(converses))
	}
	got, err := operatorToolOver(t, operator, builtin.CodeGetChangeTool).CallForTurn(asUnboundToken(), nil,
		map[string]any{"change": "site!7"})
	if err != nil || got.Failed {
		t.Errorf("the token's read of site!7 answered %v %q", err, got.Output)
	}
	got, err = operatorToolOver(t, operator, builtin.CodeOpenChangeTool).CallForTurn(asBoundPerson(), nil,
		map[string]any{"repo": "site", "branch": "feature", "title": "Retry the upload"})
	changes, _ = writer.changesAsked()
	person := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:jane-laptop", Seat: "founder"}
	if err != nil || got.Failed || len(changes) != 1 || changes[0].Asker != person ||
		statelog.CheckCallerOpID(changes[0].OpID) != nil {
		t.Fatalf("the person's open answered %v %q, sending %+v", err, got.Output, changes)
	}
	if mintedAt, _ := statelog.OpMintedAt(changes[0].OpID); !mintedAt.Equal(mustMinted(t, key)) {
		t.Errorf("the person's open is operation %s, not one the request's key derives", changes[0].OpID)
	}
}

// mustMinted is the instant an operation id carries.
func mustMinted(t *testing.T, op string) (at time.Time) {
	t.Helper()
	at, ok := statelog.OpMintedAt(op)
	if !ok {
		t.Fatalf("%s carries no instant", op)
	}
	return at
}

// EVERY CHANGE WRITE STAYS INSIDE THE OP ID BOUND: on the operator's surface,
// whose operations are the longest — a minted id naming the tool and a digest
// of its call, then the step naming the verb and the repository — every write
// every change tool makes, its second write included, carries an id every
// party to it accepts (statelog.CheckCallerOpID).
//
// Mutation: name the second write's step `reviewers` → the open's ask is past
// the bound, red.
func TestEveryChangeWriteStaysInsideTheOpIDBound(t *testing.T) {
	t.Parallel()
	host, writer := scratchHost(0), &codeWrites{}
	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	operator := builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)}
	for _, name := range builtin.ChangeWrites() {
		args, _ := holdingCall(name)
		got, err := operatorToolOver(t, operator, name).CallForTurn(asBoundPerson(), nil, args)
		if err != nil || got.Failed {
			t.Fatalf("%s answered %v %q", name, err, got.Output)
		}
	}
	changes, converses := writer.changesAsked()
	writes, _ := writer.asked()
	ids := slices.Concat(opIDsOf(changes), opIDsOfConverse(converses))
	for _, w := range writes {
		ids = append(ids, w.OpID)
	}
	if len(ids) < len(builtin.ChangeWrites()) {
		t.Fatalf("the change writes made %d writes", len(ids))
	}
	for _, id := range ids {
		if err := statelog.CheckCallerOpID(id); err != nil {
			t.Errorf("operation %q (%d bytes) is past what a write may carry: %v", id, len(id), err)
		}
	}
}
