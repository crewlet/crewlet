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

	// Prepare is the steps to connect the tool, at the vendor and in Crewlet, in order.
	Prepare []Step `json:"prepare,omitempty"`

	// Caution is the credential's one risk, nil where it has none worth a
	// warning; setupapi's copy test lists the tools that carry one.
	Caution *Caution `json:"caution,omitempty"`
}

// Step is one thing to do to connect the tool, at the vendor or in Crewlet.
type Step struct {
	// Title is the step in a few words, its heading; Where is the place it is
	// taken, as the person names it ([Here] for Crewlet's own form). A guide
	// gives both on every step or on none.
	Title string `json:"title,omitempty"`
	Where string `json:"where,omitempty"`

	// Text is the step, imperative, its vendor UI names and pages marked.
	Text vendortext.Text `json:"text"`

	// Grants are the permissions, roles or scopes to grant, in the vendor's words.
	Grants []string `json:"grants,omitempty"`

	// Result is what the person sees once the step worked; Trouble is what
	// people do hit at it, each with its fix.
	Result  vendortext.Text `json:"result,omitempty"`
	Trouble []Trouble       `json:"trouble,omitempty"`

	// Yields names the field, in the same tool, the step produces a value for.
	Yields string `json:"yields,omitempty"`

	// Needs is the integration the step connects first, empty for a step at
	// the vendor alone; Connected is whether it is connected, which setupapi
	// answers from that integration's own setup state.
	Needs     integration.Kind `json:"needs,omitempty"`
	Connected *bool            `json:"connected,omitempty"`
}

// Caution is a credential's one risk, said once per form.
type Caution struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Here is the place a step taken in Crewlet's own form names.
const Here = "Crewlet"

// Trouble is one way a step goes wrong: what the person sees, and the fix.
type Trouble struct {
	Sign vendortext.Text `json:"sign"`
	Fix  vendortext.Text `json:"fix"`
}
