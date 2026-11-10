package jetstream_test

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/words"
)

// A HANDLER RUNS INLINE ON THE GOROUTINE THAT TOOK ITS DELIVERY: a Subscribe or
// SubscribeBatch handler on its attachment's consume loop, which fetches again
// only once the handler has returned, and a listener's on the goroutine nats.go
// runs its subscription's callback on, which hands the callback one message
// after another. That is what makes two of the conformance suite's claims true
// on this backend — one listener hears one message at a time
// (one_listener_hears_one_message_at_a_time), and a handler that publishes to
// its own subscription is not re-entered
// (a_handler_that_publishes_to_its_own_subscription_is_not_re_entered) — and
// the suite cannot decide either here exactly: each is an absence it could read
// only behind a quiet window, which a second delivery the runner made late
// passed with the bug present. So both are decided where they are made, on the
// syntax tree (queuetest's doc: any invariant about WHERE code sits belongs on
// the syntax tree), and the twin's half in a bubble (memory's
// TestAHandlerNeverRunsBesideItself).
//
// THE WALK FOLLOWS EACH HANDLER, hop by hop, from the verb a caller hands it to,
// to the one call that runs it (inlinePath), and every reference to it on the
// way must be an ordinary call written directly where its row says: in the
// declaration the row names, or in the function literal that declaration hands
// one call — the consume loop it hands attach, a take that loop hands a fetch,
// the callback Listen hands the client's Subscribe. So a handler handed on in a
// go statement, from inside any other function literal, or to any function off
// the path is refused, and with them a hand-off to a worker started earlier,
// which reaches the handler only through a literal or a function the path does
// not name. The path's one go statement is attach starting the consume loop,
// once and outside any loop, and the walk follows that loop and each fetch's
// take to the calls that run them as well. Beside the path, a function the path
// hands a handler to may be referred to nowhere else, and a handler type of
// internal/queue may be named only by a parameter the path follows, so a
// handler kept in a field, a variable or a channel is refused where it is
// declared.
//
// IT MUST FIND WHAT IT GUARDS. Every row names its function, its parameter and
// where its calls sit, and the walk fails when one of them is gone or no call
// of it is found there: a rename, or a delivery rebuilt another way, turns it
// red rather than leaving it checking nothing (queuetest's doc: a guard
// asserting an absence must also assert it matched its subject).
//
// WHAT IT DOES NOT HOLD, so a reader does not infer coverage it lacks: that
// nats.go runs one subscription's callbacks one at a time, which is the
// library's (the package doc says what this package relies on), and what a
// handler does itself — one that starts a goroutine of its own runs it beside
// the next delivery, and that is its caller's design. Two attachments of one
// pair run two loops by the contract's design, so one at a time is per
// attachment, and the walk holds that one Subscribe call is one attachment
// (attach is called once, outside any loop).
//
// Mutation: hand a delivery to its handler in a go statement, from a worker fed
// by a channel, or from a helper off the path, or rename a function on the
// path, and the walk names the line.
func TestAHandlerRunsInlineOnTheGoroutineThatTookItsDelivery(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	dir := filepath.Join(root, "internal", "queue", "jetstream")
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	// EVERY FILE OF THE PACKAGE, whatever its build constraints: a delivery
	// behind a //go:build line is still a delivery.
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files[path] = file
	}
	if len(files) == 0 {
		t.Fatalf("no Go source under %s: the walk would pass having read nothing", dir)
	}
	for _, p := range checkInline(fset, files, inlinePath) {
		where := "internal/queue/jetstream"
		if p.where.IsValid() {
			rel, err := filepath.Rel(root, p.where.Filename)
			if err != nil {
				rel = p.where.Filename
			}
			where = filepath.ToSlash(rel) + ":" + strconv.Itoa(p.where.Line)
		}
		t.Errorf("%s: %s", where, p.what)
	}
}

// inlinePath is every way this package runs a handler: from the verb a caller
// hands it to, to the call that runs it, and the goroutine that call is made on.
var inlinePath = []hop{
	// Subscribe's handler: its consume loop hands it to dispatchOne in the take
	// of each fetch, and dispatchOne to runHandler, which calls it.
	{from: "Queue.Subscribe", param: "h", carries: "Handler", to: "dispatchOne",
		at: place{"Queue.Subscribe", []step{{calls: []string{"attach"}, once: true},
			{calls: []string{"fetch", "fetchLane"}}}}},
	{from: "attachment.dispatchOne", param: "h", carries: "Handler", to: "runHandler",
		at: place{decl: "attachment.dispatchOne"}},
	{from: "runHandler", param: "h", carries: "Handler", at: place{decl: "runHandler"}},

	// SubscribeBatch's: its consume loop hands it to dispatchBatch, and
	// dispatchBatch to runBatchHandler, which calls it.
	{from: "Queue.SubscribeBatch", param: "h", carries: "BatchHandler", to: "dispatchBatch",
		at: place{"Queue.SubscribeBatch", []step{{calls: []string{"attach"}, once: true}}}},
	{from: "attachment.dispatchBatch", param: "h", carries: "BatchHandler", to: "runBatchHandler",
		at: place{decl: "attachment.dispatchBatch"}},
	{from: "runBatchHandler", param: "h", carries: "BatchHandler", at: place{decl: "runBatchHandler"}},

	// Listen's: the client's callback for its subscription hands it to listen,
	// which calls it.
	{from: "Queue.Listen", param: "h", carries: "ListenFunc", to: "listen",
		at: place{"Queue.Listen", []step{{calls: []string{"Subscribe"}, once: true}}}},
	{from: "listen", param: "h", carries: "ListenFunc", at: place{decl: "listen"}},

	// The consume loop both subscriptions hand attach: started on a goroutine of
	// its own, once, and run there.
	{from: "Queue.attach", param: "loop", to: "run", at: place{decl: "Queue.attach"}, goroutine: true},
	{from: "attachment.run", param: "loop", at: place{decl: "attachment.run"}},

	// Each fetch hands what it takes to its take on the goroutine that fetched.
	{from: "attachment.fetch", param: "take", at: place{decl: "attachment.fetch"}},
	{from: "attachment.fetchLane", param: "take", at: place{decl: "attachment.fetchLane"}},
}

// hop is one step a value takes on its way to the call that runs it: in
// function from, the parameter param is handed to function to — or, where to
// is empty, called — by an ordinary call written directly in at.
type hop struct {
	from, param string
	// carries is the internal/queue handler type param is declared as, on a
	// handler's hops; empty on the consume loop's and a take's, whose types
	// are this package's own.
	carries string
	to      string
	at      place
	// goroutine marks the path's one go statement: attach starting the
	// consume loop on the goroutine every delivery of its attachment runs on.
	goroutine bool
}

// does says what h has its parameter do: called, or handed to h.to.
func (h hop) does() string {
	if h.to == "" {
		return "called"
	}
	return "handed to " + h.to
}

// place is where a hop's calls must be written: a function declaration
// ("Type.Method", or a function's name), or a function literal reached from it,
// each step a literal handed to a call of one of the step's functions written
// directly in the place so far.
type place struct {
	decl  string
	steps []step
}

// step is one literal of a place.
type step struct {
	calls []string
	// once: exactly one such call, outside any loop, because the literal is
	// a consume loop or a subscription's callback, and a second would deliver
	// to the same handler beside the first.
	once bool
}

func (p place) String() string {
	s := p.decl
	for _, st := range p.steps {
		s += ", in the literal it hands " + words.Or(st.calls)
	}
	return s
}

// inlineProblem is one way the source leaves the path.
type inlineProblem struct {
	where token.Position
	what  string
}

// queuePath is the import path of the handler types the walk follows.
const queuePath = "github.com/crewlet/crewlet/internal/queue"

// inlineCheck is one walk of a package's source against a path.
type inlineCheck struct {
	fset     *token.FileSet
	files    map[string]*ast.File
	decls    map[string]*ast.FuncDecl
	declFile map[*ast.FuncDecl]*ast.File
	// handed is where each call the path accepted names its callee.
	handed   map[token.Pos]bool
	problems []inlineProblem
}

// checkInline holds files to path, returning every way they leave it, in the
// order of the source.
func checkInline(fset *token.FileSet, files map[string]*ast.File, path []hop) []inlineProblem {
	c := &inlineCheck{fset: fset, files: files, decls: map[string]*ast.FuncDecl{},
		declFile: map[*ast.FuncDecl]*ast.File{}, handed: map[token.Pos]bool{}}
	declared := map[string]int{}
	for _, file := range files {
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				c.decls[declKey(fn)] = fn
				c.declFile[fn] = file
				declared[declKey(fn)]++
			}
		}
	}
	// ONE DECLARATION A NAME: files a build constraint keeps apart may each
	// declare a function of the path, and the walk would follow only one.
	var named []string
	for _, h := range path {
		named = append(named, h.from, h.at.decl)
	}
	slices.Sort(named)
	for _, name := range slices.Compact(named) {
		if declared[name] > 1 {
			c.fail(c.decls[name].Pos(), "%s is declared %d times, and the walk follows one: "+
				"each would need a path of its own", name, declared[name])
		}
	}
	for _, h := range path {
		c.follow(h)
	}
	c.calledOnlyOnThePath(path)
	c.handlerTypesOnlyOnThePath(path)
	slices.SortFunc(c.problems, func(a, b inlineProblem) int {
		return cmp.Or(cmp.Compare(a.where.Filename, b.where.Filename),
			cmp.Compare(a.where.Line, b.where.Line), cmp.Compare(a.what, b.what))
	})
	return c.problems
}

func (c *inlineCheck) fail(pos token.Pos, format string, args ...any) {
	var where token.Position
	if pos.IsValid() {
		where = c.fset.Position(pos)
	}
	c.problems = append(c.problems, inlineProblem{where: where, what: fmt.Sprintf(format, args...)})
}

// follow holds every reference to h's parameter in h's function to h.
func (c *inlineCheck) follow(h hop) {
	from := c.decls[h.from]
	if from == nil {
		c.fail(token.NoPos, "%s is gone, or renamed: the walk follows its %s from it, and would "+
			"follow nothing; rename its rows in inlinePath with it", h.from, h.param)
		return
	}
	if paramType(from.Type, h.param) == nil {
		c.fail(from.Pos(), "%s declares no parameter %s for the walk to follow; rename its row in "+
			"inlinePath with it", h.from, h.param)
		return
	}
	places := c.resolve(h.at)
	if len(places) == 0 {
		return
	}
	who := h.from + "'s " + h.param
	found := 0
	inspectWithStack(from.Body, func(n ast.Node, stack []ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok && paramType(lit.Type, h.param) != nil {
			return false // a parameter of its own of that name: another value
		}
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != h.param {
			return true
		}
		switch p := stack[len(stack)-1].(type) {
		case *ast.SelectorExpr:
			if p.Sel == id {
				return true // a field or a method of that name
			}
		case *ast.KeyValueExpr:
			if p.Key == id {
				return true // a literal's field of that name
			}
		case *ast.BinaryExpr:
			if (p.Op == token.EQL || p.Op == token.NEQ) && (isNil(p.X) || isNil(p.Y)) {
				return true // compared with nil, which hands it nowhere
			}
		}
		call, ok := stack[len(stack)-1].(*ast.CallExpr)
		if !ok || (call.Fun != ast.Expr(id) && !slices.Contains(call.Args, ast.Expr(id))) {
			c.fail(id.Pos(), "%s is kept, sent, returned or converted here rather than called or "+
				"handed on by a call: whatever holds it can run it on any goroutine, beside the "+
				"next delivery's", who)
			return true
		}
		ancestors := stack[:len(stack)-1]
		act := "called"
		if call.Fun != ast.Expr(id) {
			act = "handed to " + calleeOf(call)
		}
		in := enclosing(from, stack)
		switch {
		case act != h.does():
			c.fail(id.Pos(), "%s is %s, where the path has it %s and nothing else: a function off "+
				"the path can run it on any goroutine; hand it along the path (inlinePath)",
				who, act, h.does())
		case !slices.Contains(places, in):
			c.fail(id.Pos(), "%s is %s inside %s, where the path has that call written directly in "+
				"%s: a function literal can run on any goroutine, beside the next delivery's",
				who, act, describe(c.fset, in), h.at)
		case startedBy(call, ancestors) && !h.goroutine:
			c.fail(id.Pos(), "%s is %s in a go statement: it would run on a goroutine of its own, "+
				"beside the next delivery's; make the call where the delivery was taken", who, act)
		case !startedBy(call, ancestors) && h.goroutine:
			c.fail(id.Pos(), "%s is %s by an ordinary call, where the path starts it on the consume "+
				"loop's own goroutine", who, act)
		case h.goroutine && inLoop(ancestors):
			c.fail(id.Pos(), "%s is %s in a loop: one attachment runs one consume loop, and two "+
				"would deliver to the same handler beside each other", who, act)
		default:
			found++
			if callee, ok := calleeIdent(call); ok && call.Fun != ast.Expr(id) {
				c.handed[callee.Pos()] = true
			}
		}
		return true
	})
	switch {
	case found == 0:
		c.fail(from.Pos(), "%s is never %s in %s: the hop is gone or renamed, and the walk would "+
			"check nothing there; follow the delivery's new way in inlinePath", who, h.does(), h.at)
	case h.goroutine && found != 1:
		c.fail(from.Pos(), "%s is %s in %d go statements, where one attachment runs one consume loop",
			who, h.does(), found)
	}
}

// resolve finds the functions a place names: its declaration, or the literals
// its steps reach from it.
func (c *inlineCheck) resolve(p place) []ast.Node {
	decl := c.decls[p.decl]
	if decl == nil {
		c.fail(token.NoPos, "%s is gone, or renamed: the path's calls are written in it; rename "+
			"its rows in inlinePath with it", p.decl)
		return nil
	}
	here := []ast.Node{decl}
	for _, st := range p.steps {
		var next []ast.Node
		inspectWithStack(decl.Body, func(n ast.Node, stack []ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !slices.Contains(st.calls, calleeOf(call)) ||
				!slices.Contains(here, enclosing(decl, stack)) {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				switch {
				case startedBy(call, stack):
					c.fail(call.Pos(), "%s is called in a go statement with a literal the path runs "+
						"a handler in: it would run on a goroutine of its own, beside the next "+
						"delivery's", calleeOf(call))
				case st.once && inLoop(stack):
					c.fail(call.Pos(), "%s is called in a loop with the literal the path runs a "+
						"handler in: a second would deliver to the same handler beside the first",
						calleeOf(call))
				default:
					next = append(next, lit)
				}
			}
			return true
		})
		switch {
		case len(next) == 0:
			c.fail(decl.Pos(), "%s hands no function literal directly to %s any more: the path's "+
				"place %s is gone, and the walk would check nothing there", p.decl,
				words.Or(st.calls), p)
			return nil
		case st.once && len(next) > 1:
			c.fail(decl.Pos(), "%s hands %d function literals to %s, where the path has one: a "+
				"second would deliver to the same handler beside the first", p.decl, len(next),
				words.Or(st.calls))
		}
		here = next
	}
	return here
}

// calledOnlyOnThePath refuses a reference anywhere but on the path to a
// function the path hands a handler to: a call from elsewhere runs a handler
// the walk did not follow there.
func (c *inlineCheck) calledOnlyOnThePath(path []hop) {
	for _, h := range path {
		if h.to == "" || h.carries == "" {
			continue
		}
		method := c.decls[h.to] == nil // a function is keyed by its own name
		for _, file := range c.files {
			inspectWithStack(file, func(n ast.Node, stack []ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || id.Name != h.to || c.handed[id.Pos()] || len(stack) == 0 ||
					!refersTo(id, stack[len(stack)-1], method) {
					return true
				}
				c.fail(id.Pos(), "%s is referred to here, off the path, which hands it a handler only "+
					"in %s: from anywhere else it runs one the walk did not follow there", h.to, h.at)
				return true
			})
		}
	}
}

// handlerTypesOnlyOnThePath refuses internal/queue's handler types named
// anywhere but as a parameter the path follows, and a parameter the path
// follows declared as any other type.
func (c *inlineCheck) handlerTypesOnlyOnThePath(path []hop) {
	followed := map[ast.Expr]bool{}
	var carried []string
	for _, h := range path {
		if h.carries == "" {
			continue
		}
		if !slices.Contains(carried, h.carries) {
			carried = append(carried, h.carries)
		}
		fn := c.decls[h.from]
		if fn == nil {
			continue // follow said so
		}
		field := paramField(fn.Type, h.param)
		if field == nil {
			continue // follow said so
		}
		typ := field.Type
		if len(field.Names) > 1 {
			c.fail(typ.Pos(), "%s declares another parameter beside %s as its queue.%s, which the "+
				"walk does not follow; give it a field of its own and a row in inlinePath",
				h.from, h.param, h.carries)
		}
		if !isSelector(typ, queueImport(c.declFile[fn]), h.carries) {
			c.fail(typ.Pos(), "%s's %s is no longer a queue.%s: the walk follows the handler a "+
				"caller hands this package, and would follow something else", h.from, h.param, h.carries)
			continue
		}
		followed[typ] = true
	}
	for _, file := range c.files {
		name := queueImport(file)
		switch name {
		case "":
			continue
		case ".":
			c.fail(file.Name.Pos(), "internal/queue is dot-imported, which hides its handler types "+
				"from the walk")
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || followed[sel] || !slices.ContainsFunc(carried, func(typ string) bool {
				return isSelector(sel, name, typ)
			}) {
				return true
			}
			c.fail(sel.Pos(), "queue.%s is named here, off the path: a handler kept here is one "+
				"the walk does not see run; hand it on as a parameter the path follows "+
				"(inlinePath)", sel.Sel.Name)
			return true
		})
	}
}

// inspectWithStack walks root depth first, handing fn each node with its
// ancestors below root, the nearest last; fn returning false skips the node's
// children.
func inspectWithStack(root ast.Node, fn func(n ast.Node, stack []ast.Node) bool) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if !fn(n, stack) {
			return false
		}
		stack = append(stack, n)
		return true
	})
}

// declKey names a function declaration as the path does: "Type.Method", or a
// function's own name.
func declKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch generic := typ.(type) {
	case *ast.IndexExpr:
		typ = generic.X
	case *ast.IndexListExpr:
		typ = generic.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// paramField is the field declaring ft's parameter name, or nil.
func paramField(ft *ast.FuncType, name string) *ast.Field {
	if ft.Params == nil {
		return nil
	}
	for _, field := range ft.Params.List {
		for _, n := range field.Names {
			if n.Name == name {
				return field
			}
		}
	}
	return nil
}

// paramType is the declared type of ft's parameter name, or nil.
func paramType(ft *ast.FuncType, name string) ast.Expr {
	if field := paramField(ft, name); field != nil {
		return field.Type
	}
	return nil
}

// enclosing is the function a node is written directly in: the nearest
// function literal among its ancestors, or decl.
func enclosing(decl *ast.FuncDecl, stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		if lit, ok := stack[i].(*ast.FuncLit); ok {
			return lit
		}
	}
	return decl
}

// startedBy reports whether call is a go statement's, given its ancestors.
func startedBy(call *ast.CallExpr, ancestors []ast.Node) bool {
	if len(ancestors) == 0 {
		return false
	}
	g, ok := ancestors[len(ancestors)-1].(*ast.GoStmt)
	return ok && g.Call == call
}

// inLoop reports whether a node is inside a loop of the function it is written
// directly in, given its ancestors.
func inLoop(ancestors []ast.Node) bool {
	for i := len(ancestors) - 1; i >= 0; i-- {
		switch ancestors[i].(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return true
		case *ast.FuncLit, *ast.FuncDecl:
			return false
		}
	}
	return false
}

// calleeIdent is the identifier naming what call calls: a function's name, or
// a method's selector.
func calleeIdent(call *ast.CallExpr) (*ast.Ident, bool) {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		return fun, true
	case *ast.SelectorExpr:
		return fun.Sel, true
	}
	return nil, false
}

func calleeOf(call *ast.CallExpr) string {
	if id, ok := calleeIdent(call); ok {
		return id.Name
	}
	return "a function value"
}

// refersTo reports whether id, under parent, names a function of its name (or,
// with method, a method): not a declaration, a parameter, a field or a
// literal's key of that name.
func refersTo(id *ast.Ident, parent ast.Node, method bool) bool {
	switch p := parent.(type) {
	case *ast.SelectorExpr:
		return method == (p.Sel == id)
	case *ast.FuncDecl, *ast.Field:
		return false
	case *ast.KeyValueExpr:
		if p.Key == id {
			return false
		}
	}
	return !method
}

// queueImport is file's name for internal/queue, or "" where it imports none.
func queueImport(file *ast.File) string {
	for _, imp := range file.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err != nil || path != queuePath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "queue"
	}
	return ""
}

// isSelector reports whether expr is pkg.name.
func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && pkg != "" && x.Name == pkg
}

func isNil(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "nil"
}

// describe names a function a failure points at.
func describe(fset *token.FileSet, fn ast.Node) string {
	if decl, ok := fn.(*ast.FuncDecl); ok {
		return declKey(decl)
	}
	return "the function literal at line " + strconv.Itoa(fset.Position(fn.Pos()).Line)
}
