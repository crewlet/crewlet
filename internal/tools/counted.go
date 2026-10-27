package tools

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// counted is one counter of the metrics catalogue as this package records
// into it: its name, held to the catalogue's declaration of it when the
// package loads ([mustCount]). A recording goes through a typed method per
// attribute set ([toolCounted.add]), so a recording with the wrong labels, or
// the wrong number of them, does not compile, and one the catalogue declares
// otherwise does not load. The recorder drops a label the catalogue does not
// declare without a word, which is why neither may be left to a test.
type counted struct {
	name string
}

// mustCount is the counter name with attrs, refusing to load when the
// catalogue declares it otherwise.
func mustCount(name string, attrs ...string) counted {
	i := slices.IndexFunc(metrics.Catalogue(), func(in metrics.Instrument) bool { return in.Name == name })
	if i < 0 {
		panic(fmt.Sprintf("tools: %s is not in the metrics catalogue", name))
	}
	inst := metrics.Catalogue()[i]
	if inst.Kind != metrics.KindCounter || !slices.Equal(inst.Attributes, attrs) {
		panic(fmt.Sprintf("tools: %s records %v, and the catalogue declares a %s with %v",
			name, attrs, inst.Kind, inst.Attributes))
	}
	return counted{name: name}
}

// attrTool is the one attribute a [toolCounted] counter records.
const attrTool = "tool"

// toolCounted is a counter whose one attribute is the tool.
type toolCounted struct{ counted }

// countedByTool is the counter name, held to a catalogue declaration of
// exactly one attribute, the tool.
func countedByTool(name string) toolCounted {
	return toolCounted{mustCount(name, attrTool)}
}

// add counts one call to tool.
func (k toolCounted) add(c Counter, tool string) {
	if c == nil {
		return
	}
	c.Add(k.name, 1, metrics.Attrs{attrTool: tool})
}

// The counters this package records.
var (
	argsOutsideSchema  = countedByTool(metrics.ToolArgsOutsideSchema)
	policyRefusalLoops = countedByTool(metrics.ToolPolicyRefusalLoops)
)
