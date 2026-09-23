package engine

import "testing"

// A REANCHOR'S INPUTS NAME THE STREAM THEY WERE READ FROM.
//
// The creation instant, the first sequence, this node's position and the
// fleet's high-water mark are each a fact about ONE log, the one the operator
// named. The engine's own `statelog_reanchored` carried that stream; the
// framework's line, which replaced it, is handed only these inputs — so a
// stream they do not carry is a high-water mark the line cannot pair with
// anything. Every domain, because the operator may name any of them.
func TestAReanchorsInputsNameTheStreamTheyWereReadFrom(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.log
	if len(s.order) == 0 {
		t.Fatal("the node runs no domain, so nothing below is checked")
	}
	for _, name := range s.order {
		running := s.domains[name]
		want := running.domain.Stream().Name
		if got := e.reanchorInputs(t.Context(), running).Stream; got != want {
			t.Errorf("a reanchor of %s reads its inputs naming stream %q, want %q",
				name, got, want)
		}
	}
}
