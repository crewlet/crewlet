package wire_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A SCREEN'S OUTLINE IS HELD TO ITS SHAPE AND ITS BOUNDS before a turn reads
// it: a field states its kind and whether it is filled, nothing else does, a
// heading has its level, names are plain bounded text, and the whole fits its
// bytes and its count. Mutation: drop any case of PageView.validate and its
// row here is admitted.
func TestAScreenOutlineIsHeldToItsShapeAndBounds(t *testing.T) {
	t.Parallel()
	yes := true
	ok := []wire.ViewNode{
		{Role: wire.ViewDialog, Name: "Disconnect Google Cloud?"},
		{Role: wire.ViewHeading, Name: "Google Cloud", Level: 2, Depth: 1},
		{Role: wire.ViewField, Name: "Key", Field: wire.ViewFieldSecret, Filled: &yes,
			Required: true, Error: "Paste the whole key file"},
		{Role: wire.ViewStep, Name: "Connect", Current: true},
		{Role: wire.ViewButton, Name: "Connect", Disabled: true},
	}
	page := func(nodes ...wire.ViewNode) wire.PageContext {
		return wire.PageContext{Screen: "integrations", Title: "Integrations",
			SourceRevision: "integrations@1", View: &wire.PageView{Nodes: nodes}}
	}
	if err := page(ok...).Validate(); err != nil {
		t.Fatalf("a well-formed outline is refused: %v", err)
	}
	many := make([]wire.ViewNode, wire.PageViewMaxNodes+1)
	for i := range many {
		many[i] = wire.ViewNode{Role: wire.ViewLink, Name: "x"}
	}
	big := make([]wire.ViewNode, 60)
	for i := range big {
		big[i] = wire.ViewNode{Role: wire.ViewNotice, Name: strings.Repeat("y", 200)}
	}
	for name, nodes := range map[string][]wire.ViewNode{
		"an unknown role":           {{Role: "paragraph", Name: "x"}},
		"a field with no kind":      {{Role: wire.ViewField, Name: "x", Filled: &yes}},
		"a field with no fill":      {{Role: wire.ViewField, Name: "x", Field: wire.ViewFieldText}},
		"a button with a kind":      {{Role: wire.ViewButton, Name: "x", Field: wire.ViewFieldText, Filled: &yes}},
		"a button with an error":    {{Role: wire.ViewButton, Name: "x", Error: "no"}},
		"an unknown field kind":     {{Role: wire.ViewField, Name: "x", Field: "colour", Filled: &yes}},
		"a heading with no level":   {{Role: wire.ViewHeading, Name: "x"}},
		"a link with a level":       {{Role: wire.ViewLink, Name: "x", Level: 2}},
		"a name with a line break":  {{Role: wire.ViewNotice, Name: "a\nb"}},
		"a name past its bound":     {{Role: wire.ViewNotice, Name: strings.Repeat("z", wire.PageViewNameMaxRunes+1)}},
		"a depth past its bound":    {{Role: wire.ViewLink, Name: "x", Depth: 9}},
		"more entries than allowed": many,
		"more bytes than allowed":   big,
	} {
		if err := page(nodes...).Validate(); !errors.Is(err, wire.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}
