package estate

import (
	"maps"
	"reflect"
	"slices"
	"testing"
)

// atlasRouter is a router that records what it asks of a data node that
// serves nothing.
func atlasRouter(t *testing.T) (*Router, *recordingAsker) {
	t.Helper()
	asker := &recordingAsker{answer: reply{Node: "data-a", Unserved: unservedNoBackend,
		Detail: "data-a runs no native backend for it"}}
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: asker,
		Placement: &fakePlacement{nodes: []string{"data-a"}}, Git: &fakePlacement{}, Session: NewSession(),
		Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	return r, asker
}

// THE FACADE ASKS EXACTLY THE ATLAS LOG'S OPERATIONS, each method one: every
// atlas read through the reader facade, nothing else through it, and no atlas
// operation that is not a read.
//
// Mutation: point AtlasReads.History at opAtlasEntry and the reader's
// operations lack atlas.history.
func TestTheAtlasFacadesAskExactlyTheAtlasOperations(t *testing.T) {
	t.Parallel()
	r, asker := atlasRouter(t)
	for _, tc := range []struct {
		facade any
		want   func(*opSpec) bool
	}{
		{r.AtlasReads(), func(s *opSpec) bool { return s.domain == atlasDomain }},
	} {
		v := reflect.ValueOf(tc.facade)
		got := map[string]bool{}
		for i := range v.NumMethod() {
			method, m := v.Type().Method(i).Name, v.Method(i)
			in := make([]reflect.Value, m.Type().NumIn())
			in[0] = reflect.ValueOf(t.Context())
			for j := 1; j < len(in); j++ {
				in[j] = reflect.Zero(m.Type().In(j))
			}
			before := len(asker.requests())
			m.Call(in)
			sent := asker.requests()[before:]
			if len(sent) != 1 {
				t.Errorf("%s.%s asked %d operations, want one", v.Type().Name(), method, len(sent))
				continue
			}
			got[opened(sent[0].payload).Op] = true
		}
		want := map[string]bool{}
		for name, spec := range registry {
			if tc.want(spec) {
				want[name] = true
			}
		}
		if len(want) == 0 || !maps.Equal(got, want) {
			t.Errorf("%s asks %v, want exactly %v", v.Type().Name(),
				slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
	}
}
