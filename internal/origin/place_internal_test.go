package origin

import "testing"

// ONLY A FUNCTION UNDER THE MODULE'S INTERNAL TREE IS A PLACE, and its file is
// a base name: the binary's own package, the runtime, another module and a
// frame with no source file are refused, and a deferred call's wrapper is its
// function's.
//
// Mutation: drop the prefix check and the foreign rows pass; keep the whole
// path and the path row does.
func TestOnlyAnEngineFunctionIsAPlace(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/"
	for _, tc := range []struct {
		function, file string
		want           Place
		ok             bool
	}{
		{module + "internal/engine.(*Dispatcher).Dispatch", "/home/someone/src/crewlet/internal/engine/turn.go",
			Place{Function: module + "internal/engine.(*Dispatcher).Dispatch", File: "turn.go"}, true},
		{module + "internal/store.(*DB).Tx.func1.deferwrap1", "internal/store/store.go",
			Place{Function: module + "internal/store.(*DB).Tx.func1", File: "store.go"}, true},
		{"main.main", "/src/cmd/crewlet/main.go", Place{}, false},
		{module + "cmd/crewlet.run", "cmd/crewlet/main.go", Place{}, false},
		{"runtime.gopanic", "runtime/panic.go", Place{}, false},
		{"github.com/nats-io/nats.go.(*Conn).Request", "nats.go", Place{}, false},
		{"github.com/crewlet/crewlet-operator/internal/x.F", "x.go", Place{}, false},
		{module + "internal/engine.F", "", Place{}, false},
	} {
		got, ok := placeOf(tc.function, tc.file)
		if got != tc.want || ok != tc.ok {
			t.Errorf("placeOf(%q, %q) = %+v, %v; want %+v, %v", tc.function, tc.file, got, ok, tc.want, tc.ok)
		}
	}
	if enginePrefix != module+"internal/" {
		t.Errorf("the engine's prefix reads %q", enginePrefix)
	}
}
