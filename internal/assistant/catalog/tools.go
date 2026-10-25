package catalog

import (
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// all is the catalogue, built and checked once.
var all = sync.OnceValues(func() ([]Tool, error) {
	var out []Tool
	for _, t := range readTools() {
		// EVERY READ IS A ROUTE'S OWN ANSWER about the company's records,
		// so every one carries the same label: the answer names no writer
		// per field, and colleague is the least every writer earns.
		t.Kind, t.Tier = KindRead, TierRead
		t.Source, t.Trust = provenance.SourceAPIAnswer, provenance.TrustColleague
		out = append(out, t)
	}
	out = append(out, writeTools()...)
	out = append(out, interactionTools()...)
	seen := map[string]bool{}
	for i := range out {
		t := &out[i]
		if seen[t.Name] {
			return nil, fmt.Errorf("catalog: %s is declared twice", t.Name)
		}
		seen[t.Name] = true
		if t.schema == nil {
			return nil, fmt.Errorf("catalog: %s declares no schema", t.Name)
		}
		t.Parameters = t.schema.JSON()
		if preview, ok := previews[t.Name]; ok {
			t.Preview = preview(*t)
		}
		if err := t.validate(); err != nil {
			return nil, err
		}
	}
	return out, nil
})

// Tools are every tool the assistant may be offered, in the order a model
// meets them: reads, then writes, then the two interaction tools. A
// catalogue that does not hold together (a tool declared twice, a card naming
// a screen the link table does not hold) is a build defect, and panics here
// on the first read rather than serving a part of it.
func Tools() []Tool {
	tools, err := all()
	if err != nil {
		panic(err)
	}
	return slices.Clone(tools)
}

// Lookup is the tool named name.
func Lookup(name string) (Tool, bool) {
	tools := Tools()
	i := slices.IndexFunc(tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		return Tool{}, false
	}
	return tools[i], true
}

// Offered are the tools this person may be offered now, in [Tools] order: a
// read whose apps they hold, a write whose act tool is in acts (the viewer's
// own list of what the authority table could admit them to,
// internal/api/operator's Acts), and the page change and the link tool
// always (a page change is held to the turn's accepts by [PageActionFor]).
//
// A FILTER AND NEVER AUTHORITY. A tool the person could not take is not
// offered, so the model cannot be talked into trying it; every call is still
// decided by the route it sends, as the person.
func Offered(p iam.Principal, acts []string) []Tool {
	var out []Tool
	for _, t := range Tools() {
		switch t.Kind {
		case KindRead:
			if !slices.ContainsFunc(t.Apps, func(g iam.Grant) bool { return !p.Can(g) }) {
				out = append(out, t)
			}
		case KindWrite:
			if slices.Contains(acts, t.Act) {
				out = append(out, t)
			}
		case KindPageAction, KindNavigate:
			out = append(out, t)
		}
	}
	return out
}

// Curated is the curated table as the orchestrator asks it: [Offered], for
// the internal/assistant Catalogue seam a node and every harness fill alike.
type Curated struct{}

// Offered is [Offered].
func (Curated) Offered(p iam.Principal, acts []string) []Tool { return Offered(p, acts) }

// validate reports what is wrong with one tool as declared.
func (t Tool) validate() error {
	switch {
	case t.Name == "" || t.Description == "" || t.Phrase == "":
		return fmt.Errorf("catalog: %q needs a name, a description and a phrase", t.Name)
	case !t.Kind.Valid() || !t.Tier.Valid():
		return fmt.Errorf("catalog: %s has kind %q and tier %q", t.Name, t.Kind, t.Tier)
	case t.Build == nil:
		return fmt.Errorf("catalog: %s builds no request", t.Name)
	case (t.Kind == KindWrite) != (t.Act != ""):
		return fmt.Errorf("catalog: %s names an act tool exactly when it is a write", t.Name)
	case t.Kind != KindPageAction && len(t.Routes) == 0:
		return fmt.Errorf("catalog: %s names no route", t.Name)
	case (t.Kind == KindWrite || t.Kind == KindPageAction) != (t.Preview != nil):
		return fmt.Errorf("catalog: %s has a preview exactly when it proposes", t.Name)
	}
	for name := range t.schema.properties {
		if slices.Contains(IdentityArguments, name) {
			return fmt.Errorf("catalog: %s declares the identity argument %q", t.Name, name)
		}
	}
	if t.Kind == KindRead {
		if !t.Source.Valid() || !t.Trust.Valid() {
			return fmt.Errorf("catalog: the read %s declares no fence source and trust", t.Name)
		}
		if len(t.Cards) == 0 {
			return fmt.Errorf("catalog: the read %s cards nothing", t.Name)
		}
		for variant, spec := range t.Cards {
			if err := spec.Validate(); err != nil {
				return fmt.Errorf("catalog: %s's %q card: %w", t.Name, variant, err)
			}
			if spec.Prose() && !t.Fence {
				return fmt.Errorf("catalog: %s's %q card reads text a person or an agent "+
					"wrote, and the tool is not fenced", t.Name, variant)
			}
		}
	}
	return nil
}
