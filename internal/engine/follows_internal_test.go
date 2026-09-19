package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE BOOT HANDS THE FOLLOWS OFF, AND DOES IT BEFORE ANYTHING ROUTES CHAT.
//
// Derived from the source rather than driven through a full boot, on
// [TestTheBootMigratesBeforeItSnapshots]'s reasoning and for a sharper reason:
// the whole of internal/notify/followsync, the contract method it writes
// through, the store type that was reinstated to feed it and every test over
// all three become DEAD CODE if this one line is not there, and the suite stays
// green. That was measured, not supposed — deleting the call leaves
// `go build`, `go vet` and every package's tests passing.
//
// The ORDER is the other half and it is load-bearing. `installEpoch` is what
// builds the chat transports, so a handoff running after it would be filling
// the bucket while inbound messages were already being matched against it —
// and a reply arriving in that window reaches a seat by luck. The symptom is
// silent both ways: with the call gone, every follow written before the move
// stays in a table no peer can see; with it late, a thread goes quiet for as
// long as the pass takes.
func TestTheBootHandsOffTheFollowsBeforeItRoutes(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}

	handoff, epoch := -1, -1
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "New" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "migrateFollows":
				handoff = fset.Position(call.Pos()).Line
			case "installEpoch":
				if epoch < 0 {
					epoch = fset.Position(call.Pos()).Line
				}
			}
			return true
		})
	}

	switch {
	case epoch < 0:
		t.Fatal("New no longer installs an epoch, so this guard cannot say " +
			"whether the handoff precedes the chat transports — point it at " +
			"whatever replaced it")
	case handoff < 0:
		t.Fatal("New does not hand this node's own thread-follows to the fleet, " +
			"so every follow recorded before node migration 0028 stays in a " +
			"table no peer can read: a thread reply that is not a mention " +
			"reaches its seat only when the node that claimed it happens to be " +
			"the one that recorded the follow. internal/notify/followsync and " +
			"everything feeding it is then dead code, and nothing else notices")
	case handoff > epoch:
		t.Fatalf("the handoff runs at line %d, after the epoch installs at %d: "+
			"the chat transports are built by the epoch, so inbound messages "+
			"are matched against the follows bucket while it is still being "+
			"filled", handoff, epoch)
	}
}
