package setup

import (
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// Guide is what a tool's form says around its fields: what connecting does,
// the steps at the vendor, and the one risk its credential carries.
type Guide struct {
	// Summary is one sentence on what connecting the tool does.
	Summary string `json:"summary,omitempty"`

	// Prepare is what to do at the vendor, in order.
	Prepare []Step `json:"prepare,omitempty"`

	// Caution is the credential's one risk, nil where it has none worth a
	// warning; setupapi's copy test lists the tools that carry one.
	Caution *Caution `json:"caution,omitempty"`
}

// Step is one thing to do at the vendor.
type Step struct {
	// Text is the step, imperative, its vendor UI names and pages marked.
	Text vendortext.Text `json:"text"`

	// Grants are the permissions, roles or scopes to grant, in the vendor's words.
	Grants []string `json:"grants,omitempty"`

	// Yields names the field, in the same tool, the step produces a value for.
	Yields string `json:"yields,omitempty"`

	// Needs is the integration the step connects first, empty for a step at
	// the vendor alone; Connected is whether it is connected, which setupapi
	// answers from that integration's own setup state.
	Needs     integration.Kind `json:"needs,omitempty"`
	Connected *bool            `json:"connected,omitempty"`

	// Done is whether the company is past this step, which setupapi answers
	// from its setup state ([MarkDone]).
	Done bool `json:"done,omitempty"`
}

// MarkDone marks each of steps done from reqs, the state they are answered
// with: a step that yields a field once that field holds a value that
// resolves, one that connects an integration first once it is connected, and
// any step once a step after it is done, since the steps run in order. The
// step a person is on is the first not done.
func MarkDone(steps []Step, reqs []Requirement) {
	held := map[string]bool{}
	for _, r := range reqs {
		held[r.Field] = r.Present && (r.Resolved == nil || *r.Resolved)
	}
	later := false
	for i := len(steps) - 1; i >= 0; i-- {
		s := &steps[i]
		s.Done = later || (s.Yields != "" && held[s.Yields]) ||
			(s.Needs != "" && s.Connected != nil && *s.Connected)
		later = s.Done
	}
}

// Caution is a credential's one risk, said once per form.
type Caution struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
