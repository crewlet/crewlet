package tools

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// counted is one counter of the metrics catalogue as this package records
// into it: its name and its attributes, IN THE CATALOGUE'S ORDER, held to the
// catalogue's declaration when the package loads. A recording names a value
// for each attribute by position, so it can neither carry a label the
// catalogue does not declare (which the recorder would drop without a word)
// nor leave one out.
type counted struct {
	name  string
	attrs []string
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
	return counted{name: name, attrs: attrs}
}

// add counts one, with a value for each attribute, in order.
func (k counted) add(c Counter, values ...string) {
	if c == nil {
		return
	}
	if len(values) != len(k.attrs) {
		panic(fmt.Sprintf("tools: %s takes %d attribute values, and was given %d", k.name,
			len(k.attrs), len(values)))
	}
	attrs := make(metrics.Attrs, len(values))
	for i, a := range k.attrs {
		attrs[a] = values[i]
	}
	c.Add(k.name, 1, attrs)
}

// The counters this package records.
var (
	argsOutsideSchema  = mustCount(metrics.ToolArgsOutsideSchema, "tool")
	policyRefusalLoops = mustCount(metrics.ToolPolicyRefusalLoops, "tool")
)
