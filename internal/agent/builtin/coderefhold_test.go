package builtin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE REF HOLD (M3-decisions M3-O61, M3-O65): a seat's turn holds every seat's
// branch it reads, writes or launches on BEFORE the request that depends on
// what the branch names — through one seam — records the hold where the
// branch has gone unset long enough that a deletion's listing could be in
// flight, and pays the read a hold is decided from once a turn and branch.

// seatBranch is the seat's branch the ref hold cases name: another seat's,
// qa's, which a turn of agent-cto reads and writes as any colleague may.
const (
	seatBranch      = "refs/heads/seats/qa/x"
	seatBranchShort = "seats/qa/x"
)

// seatBranchOID is what seatBranch names.
var seatBranchOID = strings.Repeat("e", 40)

// withSeatBranch is host holding seatBranch, set by seat qa unset ago.
func withSeatBranch(host *codeHost, seat string, unset time.Duration) *codeHost {
	host.refs[seatBranch] = seatBranchOID
	host.seats = map[string]string{seatBranch: seat}
	host.ats = map[string]time.Time{seatBranch: time.Now().Add(-unset)}
	return host
}

// seatBranchKey is the hold key of seatBranch of the repository the cases
// read — site, and the scratch repository, which share an id.
var seatBranchKey = code.RefHoldKey(siteID, seatBranch)

// refEvents is every question a turn's calls sent the code host, in order:
// the reads (read:<kind>, resolve_ref, git_read:<kind>) and the writes
// (git_write, lifecycle:<kind>).
type refEvents struct {
	mu   sync.Mutex
	seen []string
}

func (e *refEvents) add(what string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, what)
}

func (e *refEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.seen)
}

// watch records every question host and writer are asked, in order, and calls
// also, where set, with each as it arrives.
func watch(host *codeHost, writer *codeWrites, also func(what string)) *refEvents {
	ev := &refEvents{}
	host.before = func(what string) {
		ev.add(what)
		if also != nil {
			also(what)
		}
	}
	writer.before = func(what, _ string) {
		ev.add(what)
		if also != nil {
			also(what)
		}
	}
	return ev
}

// count is how many of seen are what.
func count(seen []string, what string) int {
	n := 0
	for _, s := range seen {
		if s == what {
			n++
		}
	}
	return n
}

// refArgs classifies every argument a code tool's schema may declare: true
// for one that names a ref — a revision, a target, a branch — whose seat's
// branch the call must hold before the request that reads or writes through
// it; false for one that names none, with why. An argument in neither fails
// TestEveryCodeToolHoldsTheSeatRefsItNamesBeforeItsRequest, so neither a new
// tool nor a new argument escapes the hold unclassified.
var refArgs = map[string]bool{
	"rev": true, "ref": true, "base": true, "head": true, "commit": true, "target": true, "at": true,
	"branch": true,
}

var notRefArgs = map[string]string{
	"repo":               "a repository, held by HoldRepo",
	"name":               "a repository's name, held by HoldRepo on its derived id",
	"rename_to":          "a repository's new name",
	"path":               "a path in a tree the call's revision names",
	"path_b64":           "a path in a tree the call's revision names, its bytes in base64",
	"paths":              "paths in the trees the revisions name",
	"file":               "a path in the trees the revisions name",
	"file_b64":           "a path in the trees the revisions name, its bytes in base64",
	"start_line":         "a line of a file",
	"end_line":           "a line of a file",
	"recursive":          "how a tree is listed",
	"renames":            "how a diff pairs files",
	"grep":               "a message filter",
	"since":              "an instant",
	"cursor":             "a page's place",
	"limit":              "a page's size",
	"status":             "a repository status filter",
	"branches_prefix":    "a listing's prefix: it reads every ref under it and depends on none",
	"branches_cursor":    "a listing's place",
	"protections_cursor": "a listing's place",
	"expected_head":      "an object id the write is compared with, never a ref",
	"message":            "a commit's message",
	"files":              "a commit's file operations",
	"state":              "a change state filter",
	"owner":              "a change's or a repository's owner",
	"work":               "a work item",
	"reviewer":           "a reviewer",
	"needs":              "a change filter",
	"change":             "a change's number",
	"part":               "a part of a change",
	"revision":           "a change's revision number",
	"since_revision":     "a change's revision number",
	"description":        "a repository's description",
	"default_branch":     "a repository's setting, which no request the call sends reads through",
	"visibility":         "a repository's setting",
	"search":             "a repository's setting",
	"path_owners":        "a repository's setting",
	"archived":           "a repository's setting",
	"reason":             "a deletion's reason",
	"pattern":            "a protection's pattern, never a ref",
	"direct_push":        "a protection's field",
	"allow_force":        "a protection's field",
	"allow_delete":       "a protection's field",
	"remove":             "a protection's field",
	"remove_gate":        "a protection's field",
	"merge_by":           "a merge gate's field",
	"approvals":          "a merge gate's field",
	"approvals_unless":   "a merge gate's field",
	"human_paths":        "a merge gate's field",
	"checks":             "a merge gate's field",
	"landing":            "a merge gate's field",
	"delete_source":      "a merge gate's field",
	"owners_approve":     "a merge gate's field",
	"auto_revert":        "a merge gate's field",
	"agents_edit_checks": "a merge gate's field",
	"op_id":              "an operation's id",
}

// refArgDefault is what a call gives a ref argument it does not name the seat's
// branch through: an id, or a ref no seat set — each of which holds nothing.
func refArgDefault(arg string) any {
	switch arg {
	case "branch":
		return "fix"
	case "target", "ref":
		return "main"
	}
	return mainOID
}

// EVERY CODE TOOL HOLDS THE SEAT REFS IT NAMES BEFORE ITS REQUEST: every
// argument of every code tool is classified as naming a ref or not, and each
// tool, called by a seat's turn with each of its ref arguments naming a seat's
// branch — in a project repository and in a scratch one — has the branch's key
// in the turn's holds before the code host is asked anything that reads or
// writes through it (a read by object id, an engine write). A whole object id,
// main, a tag and an operator's call hold nothing.
//
// Mutations: skip HoldRef in the one ref read (CodeDeps.refsOf) → red; skip it
// in CodeDeps.branch → red; add an argument to a tool's schema without
// classifying it → red.
func TestEveryCodeToolHoldsTheSeatRefsItNamesBeforeItsRequest(t *testing.T) {
	t.Parallel()
	for _, tool := range builtin.CodeTools(writeDeps(newCodeHost(), &codeWrites{})) {
		name := tool.Name()
		props, _ := tool.Parameters()["properties"].(map[string]any)
		var named []string
		for _, arg := range slices.Sorted(maps.Keys(props)) {
			switch {
			case refArgs[arg]:
				named = append(named, arg)
			case notRefArgs[arg] == "":
				t.Errorf("%s's argument %q is classified neither as naming a ref nor as naming none: add it "+
					"to refArgs, and hold what it names before the request, or to notRefArgs with why", name, arg)
			}
		}
		base, ok := holdingCall(name)
		if len(named) == 0 {
			continue
		}
		if !ok || base == nil {
			t.Errorf("%s names a ref and has no row in holdingCall", name)
			continue
		}
		for _, arg := range named {
			for _, scratch := range []bool{false, true} {
				t.Run(name+"/"+arg+map[bool]string{false: "/project", true: "/scratch"}[scratch], func(t *testing.T) {
					t.Parallel()
					host := newCodeHost()
					if scratch {
						host = scratchHost(time.Hour)
					}
					withSeatBranch(host, "qa", time.Hour)
					writer := &codeWrites{}
					turn := writerTurn(t)
					var mu sync.Mutex
					var unheld []string
					watch(host, writer, func(what string) {
						if !strings.HasPrefix(what, "git_") {
							return
						}
						if !slices.Contains(turn.Holds.IDs(), seatBranchKey) {
							mu.Lock()
							unheld = append(unheld, what)
							mu.Unlock()
						}
					})
					// THE TOOL'S OWN CALL, its other ref arguments naming
					// what holds nothing, and this one the seat's branch —
					// a commit from `base` being a create, which names no
					// head.
					args := maps.Clone(base)
					args["repo"] = host.detail.Name
					for _, other := range named {
						if _, given := args[other]; given {
							args[other] = refArgDefault(other)
						}
					}
					args[arg] = seatBranchShort
					if arg == "base" {
						delete(args, "expected_head")
					}
					got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), turn, name, args)
					if got.Failed {
						t.Fatalf("%s with %s %s answered %q", name, arg, seatBranchShort, got.Output)
					}
					if len(unheld) > 0 {
						t.Errorf("%s sent %v before the turn held %s", name, unheld, seatBranch)
					}
					if !slices.Contains(turn.Holds.IDs(), seatBranchKey) {
						t.Errorf("%s with %s naming %s left it unheld: the turn holds %v", name, arg, seatBranch,
							turn.Holds.IDs())
					}
				})
			}
		}
	}

	// AN ID, MAIN, A TAG: nothing held.
	for _, rev := range []string{mainOID, "main", "v1"} {
		host, writer := withSeatBranch(newCodeHost(), "qa", time.Hour), &codeWrites{}
		turn := writerTurn(t)
		callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), turn, builtin.CodeReadTool,
			map[string]any{"repo": "site", "rev": rev, "path": "README.md"})
		if held := turn.Holds.IDs(); len(held) != 0 {
			t.Errorf("a read at %s held %v", rev, held)
		}
	}

	// AN OPERATOR'S CALL: no turn, nothing held and nothing recorded, however
	// long the branch has gone unset.
	host, writer := withSeatBranch(newCodeHost(), "qa", code.RefHoldRecordAfter+time.Hour), &codeWrites{}
	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	read := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeReadTool)
	if got, err := read.CallForTurn(asBoundPerson(), nil, map[string]any{"repo": "site", "rev": seatBranchShort,
		"path": "README.md"}); err != nil || got.Failed {
		t.Fatalf("an operator's read answered %v %q", err, got.Output)
	}
	if _, lifecycles := writer.asked(); len(lifecycles) != 0 {
		t.Errorf("an operator's read sent %+v", lifecycles)
	}
}

// ONLY THE SEAM RESOLVES A REF FOR A TURN: in this package's own files, a
// code.RefQuery is built only inside CodeDeps.refsOf — the one ref read, which
// takes the turn's hold on every seat's branch it answers — so no tool reads a
// ref for a turn around the hold.
//
// Mutation: a tool building a RefQuery of its own → red.
func TestOnlyTheSeamResolvesARefForATurn(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(sourcetree.Root(t), "internal", "agent", "builtin")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	const seam = "CodeDeps.refsOf"
	built := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			who := declName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "RefQuery" {
					built++
					if who != seam {
						t.Errorf("%s builds a code.RefQuery: only %s reads a ref for a turn, taking its hold",
							who, seam)
					}
				}
				return true
			})
		}
	}
	if built == 0 {
		t.Fatal("the walk found no code.RefQuery at all — it no longer recognises what it checks")
	}
}

// A REF HOLD OF AN IDLE SEAT BRANCH IS RECORDED FIRST: a read through a seat's
// branch unset code.RefHoldRecordAfter or more sends a hold request — naming
// the whole ref, decided at the resolve's checkpoint — before its read by
// object id; one unset less, or a branch no seat set, sends none. A hold
// answered ref_deleted refuses the call naming the commit the read answered,
// and one whose outcome is unknown refuses it too — each with nothing sent
// through the branch. A WRITE naming the branch's head is held as a read is:
// a revert that conflicts and stores nothing still sends the resolve and the
// hold before its write; a branch a write creates sends neither.
//
// Mutations: skip the record → red; record on a branch no seat set → red;
// proceed on an unknown outcome → red; CodeDeps.branch holding an existing
// branch with no read → the write arm's hold is missing, red.
func TestARefHoldOfAnIdleSeatBranchIsRecordedFirst(t *testing.T) {
	t.Parallel()
	readArgs := map[string]any{"repo": "site", "rev": seatBranchShort, "path": "README.md"}
	for _, c := range []struct {
		name     string
		seat     string
		unset    time.Duration
		answer   func(code.LifecycleRequest) (code.LifecycleResult, error)
		recorded bool
		read     bool
		class    tools.Refusal
	}{
		{name: "unset past the threshold", seat: "qa", unset: code.RefHoldRecordAfter + time.Hour,
			recorded: true, read: true},
		{name: "unset under it", seat: "qa", unset: code.RefHoldRecordAfter - time.Hour, read: true},
		{name: "no seat's", unset: code.RefHoldRecordAfter + time.Hour, read: true},
		{name: "answered deleted", seat: "qa", unset: code.RefHoldRecordAfter + time.Hour, recorded: true,
			class: tools.RefusalNotFound,
			answer: func(code.LifecycleRequest) (code.LifecycleResult, error) {
				return code.LifecycleResult{Refusal: code.LifecycleRefDeleted,
					Detail: seatBranch + " is no live ref of site"}, nil
			}},
		{name: "unconfirmed", seat: "qa", unset: code.RefHoldRecordAfter + time.Hour, recorded: true,
			class: tools.RefusalUnavailable,
			answer: func(code.LifecycleRequest) (code.LifecycleResult, error) {
				return code.LifecycleResult{Outcome: statelog.OutcomeUnknown}, nil
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host, writer := withSeatBranch(newCodeHost(), c.seat, c.unset), &codeWrites{life: c.answer}
			ev := watch(host, writer, nil)
			turn := writerTurn(t)
			got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), turn, builtin.CodeReadTool,
				readArgs)
			_, lifecycles := writer.asked()
			if recorded := len(lifecycles) == 1; recorded != c.recorded {
				t.Fatalf("hold records sent %+v, want recorded %v", lifecycles, c.recorded)
			}
			seen := ev.list()
			if c.recorded {
				h := lifecycles[0]
				if h.Kind != code.LifecycleHold || h.Ref != seatBranch || h.Seen != codeAt(12) || h.TurnID != turn.RunID ||
					h.RepoID != siteID || h.Actor != (code.Principal{Kind: code.PrincipalAgent, Seat: "agent-cto"}) {
					t.Errorf("the hold record is %+v, want the turn's on %s, decided at its resolve", h, seatBranch)
				}
				if i := slices.Index(seen, "lifecycle:hold"); i < 0 || slices.Index(seen, "git_read:blob") >= 0 &&
					slices.Index(seen, "git_read:blob") < i {
					t.Errorf("the questions ran %v: the read through the branch was sent before its hold", seen)
				}
			}
			if read := count(seen, "git_read:blob") == 1; read != c.read {
				t.Fatalf("the read was sent: %v, want %v (answered %q)", read, c.read, got.Output)
			}
			if !c.read && (!got.Failed || tools.RefusalOf(got) != c.class) {
				t.Errorf("answered %q as %s, want a %s refusal", got.Output, tools.RefusalOf(got), c.class)
			}
			if c.class == tools.RefusalNotFound && !strings.Contains(got.Output, seatBranchOID) {
				t.Errorf("the deletion's answer %q names no commit to restore the branch at", got.Output)
			}
		})
	}

	// THE WRITE ARM: a revert that conflicts sends the resolve and the hold
	// before its write; a create sends neither.
	host := withSeatBranch(newCodeHost(), "qa", code.RefHoldRecordAfter+time.Hour)
	writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
		return code.EngineWriteResult{Refusal: code.WriteConflict, Detail: "a.txt conflicts"}, nil
	}}
	ev := watch(host, writer, nil)
	reg := codeRegistry(t, writeDeps(host, writer))
	callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeRevertTool, map[string]any{"repo": "site",
		"branch": seatBranchShort, "expected_head": seatBranchOID, "commit": featureOID})
	seen := ev.list()
	write := slices.Index(seen, "git_write")
	if write < 0 || !slices.Contains(seen[:write], "resolve_ref") || !slices.Contains(seen[:write], "lifecycle:hold") {
		t.Errorf("a conflicting revert's questions ran %v, want the resolve and the hold before its write", seen)
	}
	ev = watch(host, writer, nil)
	writer.write = nil
	callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeCreateBranchTool, map[string]any{"repo": "site",
		"branch": "seats/qa/new", "at": mainOID})
	if seen := ev.list(); count(seen, "resolve_ref") != 0 || count(seen, "lifecycle:hold") != 0 {
		t.Errorf("a create of a new branch sent %v, want neither a resolve nor a hold", seen)
	}
}

// A TURN RESOLVES A BRANCH IT WRITES ONCE (M3-decisions M3-O65): a turn that
// reads a seat's branch with code_resolve and then writes it twice at the head
// it read sends one resolve — its code_resolve's — and none before either
// write; a turn whose first contact with the branch is a write resolves it
// before that write, and not before the next; a branch the turn created, held
// with no read, is resolved once, before the first write that names its head;
// and a write no hold covers — on main, or an operator's — resolves nothing.
//
// Mutations: CodeDeps.branch ignoring the mark → a resolve before every write,
// red; resolving every existing branch → the main and operator rows red;
// confirming a hold taken with no read → the create-then-write row sends no
// resolve, red.
func TestATurnResolvesABranchItWritesOnce(t *testing.T) {
	t.Parallel()
	commit := func(branch string) map[string]any {
		return map[string]any{"repo": "site", "branch": branch, "expected_head": seatBranchOID, "message": "m",
			"files": []any{map[string]any{"op": "put", "path": "a.txt", "content": "a"}}}
	}
	before := func(seen []string, what string) []int {
		var out []int
		resolves := 0
		for _, s := range seen {
			switch s {
			case "resolve_ref":
				resolves++
			case what:
				out = append(out, resolves)
			}
		}
		return out
	}
	newCase := func() (*codeHost, *codeWrites, *refEvents, *turnctx.Turn) {
		host, writer := withSeatBranch(newCodeHost(), "qa", time.Hour), &codeWrites{}
		return host, writer, watch(host, writer, nil), writerTurn(t)
	}

	// THE GUIDANCE'S WAY: code_resolve, then two writes at its head.
	host, writer, ev, turn := newCase()
	reg := codeRegistry(t, writeDeps(host, writer))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeResolveTool, map[string]any{"repo": "site",
		"ref": seatBranchShort})
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commit(seatBranchShort))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commit(seatBranchShort))
	if seen := ev.list(); count(seen, "resolve_ref") != 1 || !slices.Equal(before(seen, "git_write"), []int{1, 1}) {
		t.Errorf("code_resolve and two writes sent %v, want the one resolve, before both writes", seen)
	}

	// A WRITE FIRST: resolved before it, and not before the next.
	host, writer, ev, turn = newCase()
	reg = codeRegistry(t, writeDeps(host, writer))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commit(seatBranchShort))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commit(seatBranchShort))
	if seen := ev.list(); !slices.Equal(before(seen, "git_write"), []int{1, 1}) {
		t.Errorf("two writes sent %v, want one resolve, before the first", seen)
	}

	// A CREATE, THEN A WRITE AT ITS HEAD: one resolve, before that write.
	host, writer, ev, turn = newCase()
	reg = codeRegistry(t, writeDeps(host, writer))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCreateBranchTool, map[string]any{"repo": "site",
		"branch": seatBranchShort, "at": mainOID})
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commit(seatBranchShort))
	if seen := ev.list(); !slices.Equal(before(seen, "git_write"), []int{0, 1}) {
		t.Errorf("a create and a write sent %v, want no resolve before the create and one before the write", seen)
	}

	// MAIN, AND AN OPERATOR: no resolve.
	host, writer, ev, turn = newCase()
	reg = codeRegistry(t, writeDeps(host, writer))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commit("main"))
	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	operator := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeCommitFilesTool)
	if got, err := operator.CallForTurn(asBoundPerson(), nil, commit(seatBranchShort)); err != nil || got.Failed {
		t.Fatalf("an operator's write answered %v %q", err, got.Output)
	}
	if seen := ev.list(); count(seen, "resolve_ref") != 0 || count(seen, "git_write") != 2 {
		t.Errorf("a write on main and an operator's write sent %v, want both writes and no resolve", seen)
	}
}

// A FAILED HOLD RECORD LEAVES THE BRANCH UNCONFIRMED (M3-decisions M3-O65): a
// write on a seat's branch unset past the threshold whose hold record is
// answered unknown, deleted, or not served is refused, and the same write again
// resolves the branch and asks for the record again; answered applied, a third
// write sends neither — the hold is confirmed only once its record is.
//
// Mutation: confirm at the Take, before the record's answer → the second write
// sends neither, red.
func TestAFailedHoldRecordLeavesTheBranchUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		answer func(code.LifecycleRequest) (code.LifecycleResult, error)
	}{
		{"unknown", func(code.LifecycleRequest) (code.LifecycleResult, error) {
			return code.LifecycleResult{Outcome: statelog.OutcomeUnknown}, nil
		}},
		{"deleted", func(code.LifecycleRequest) (code.LifecycleResult, error) {
			return code.LifecycleResult{Refusal: code.LifecycleRefDeleted, Detail: "gone"}, nil
		}},
		{"unserved", func(code.LifecycleRequest) (code.LifecycleResult, error) {
			return code.LifecycleResult{}, code.ErrNotServing
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := withSeatBranch(newCodeHost(), "qa", code.RefHoldRecordAfter+time.Hour)
			writer := &codeWrites{life: c.answer}
			ev := watch(host, writer, nil)
			reg := codeRegistry(t, writeDeps(host, writer))
			turn := writerTurn(t)
			args := map[string]any{"repo": "site", "branch": seatBranchShort, "expected_head": seatBranchOID,
				"message": "m", "files": []any{map[string]any{"op": "put", "path": "a.txt", "content": "a"}}}
			if got := callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, args); !got.Failed {
				t.Fatalf("a write whose hold record failed answered %q", got.Output)
			}
			writer.mu.Lock()
			writer.life = nil
			writer.mu.Unlock()
			for i, want := range []int{1, 0} {
				mark := len(ev.list())
				if got := callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, args); got.Failed {
					t.Fatalf("write %d answered %q", i+2, got.Output)
				}
				seen := ev.list()[mark:]
				if count(seen, "resolve_ref") != want || count(seen, "lifecycle:hold") != want {
					t.Errorf("write %d sent %v, want %d resolve and %d hold", i+2, seen, want, want)
				}
			}
		})
	}
}

// AN IDLE SCRATCH REPOSITORY IS RECORDED ONCE PER TURN (M3-decisions M3-O65):
// two reads of one scratch repository last active 23 h ago, in one turn, send
// one hold request, before the first read; and the seam itself, asked again
// for a repository it has confirmed — a second resolution in the turn, the
// lookups forgotten — sends nothing more.
//
// Mutation: HoldRepo ignoring the mark → the seam's second hold sends a second
// request, red.
func TestAnIdleScratchRepositoryIsRecordedOncePerTurn(t *testing.T) {
	t.Parallel()
	host, writer := scratchHost(23*time.Hour), &codeWrites{}
	ev := watch(host, writer, nil)
	turn := writerTurn(t)
	reg := codeRegistry(t, writeDeps(host, writer))
	args := map[string]any{"repo": "agent-cto.lab", "rev": mainOID, "path": "README.md"}
	callWrite(t, everyGrant(), reg, turn, builtin.CodeReadTool, args)
	callWrite(t, everyGrant(), reg, turn, builtin.CodeReadTool, args)
	seen := ev.list()
	if count(seen, "lifecycle:hold") != 1 ||
		slices.Index(seen, "lifecycle:hold") > slices.Index(seen, "git_read:blob") {
		t.Errorf("two reads of an idle scratch repository sent %v, want one hold, before the first read", seen)
	}

	repo := host.detail.RepoRef
	actor := builtin.Actor{Kind: tracker.AuthorAgent, Handle: "agent-cto"}
	again := writerTurn(t)
	for range 2 {
		if err := builtin.HoldRepo(t.Context(), again, writer, actor, repo, nil); err != nil {
			t.Fatalf("hold: %v", err)
		}
	}
	if _, lifecycles := writer.asked(); len(lifecycles) != 2 {
		t.Errorf("a turn's seam asked to hold one idle scratch repository twice sent %d hold requests in all, "+
			"want one more than the reads' one", len(lifecycles))
	}
}
