package tools

import (
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// counted is one counter of the metrics catalogue as this package records
// into it: its name, held to the catalogue's declaration of it as it is first
// recorded ([mustCount]). A recording goes through a typed method per
// attribute set ([toolCounted.add]), so a recording with the wrong labels, or
// the wrong number of them, does not compile, and one the catalogue declares
// otherwise panics on its first recording, which its test makes. The recorder
// drops a label the catalogue does not declare without a word.
type counted struct {
	name string
}

// mustCount is the counter name with attrs, refusing it when the catalogue
// declares it otherwise.
func mustCount(name string, attrs ...string) counted {
	catalogue := metrics.Catalogue()
	i := slices.IndexFunc(catalogue, func(in metrics.Instrument) bool { return in.Name == name })
	if i < 0 {
		panic(fmt.Sprintf("tools: %s is not in the metrics catalogue", name))
	}
	inst := catalogue[i]
	if inst.Kind != metrics.KindCounter || !slices.Equal(inst.Attributes, attrs) {
		panic(fmt.Sprintf("tools: %s records %s, and the catalogue declares a %s with %s",
			name, words.And(attrs), inst.Kind, words.And(inst.Attributes)))
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

// The counters this package records, each held to the catalogue AT ITS FIRST
// RECORDING, never at init: the catalogue is built afresh on every read, and
// two counters checked at init built it four times on every start of the
// binary, a push's hook included (cmd/crewlet's
// TestTheHookPaysLittleForTheEnginesInit), about 212 KB of the module's bound.
var (
	argsOutsideSchema = sync.OnceValue(func() toolCounted {
		return countedByTool(metrics.ToolArgsOutsideSchema)
	})
	policyRefusalLoops = sync.OnceValue(func() toolCounted {
		return countedByTool(metrics.ToolPolicyRefusalLoops)
	})
)
