package setup

// LinkMark is where a field's vendor link sits in its [Requirement.Hint], its
// [Requirement.More] or a [Step.Text]: a form draws [Requirement.LinkText]
// there, linked to [Requirement.VendorURL] once every placeholder in that
// address has a value.
const LinkMark = "{link}"

// Guide is what a tool's form says around its fields: the sentence it opens
// with, the steps to take at the vendor first, and the one risk its
// credential carries.
//
// THE FORM'S WORDS, NOT A FIELD'S. A field says what it holds in one line
// ([Requirement.Hint]). The order of work at the vendor and the permissions
// to grant there span several fields, and said under each of them they were
// the wall of text a form opened on, so they are said once, here, for a form
// to lay out beside its fields.
type Guide struct {
	// Summary is one sentence on what connecting the tool does.
	Summary string `json:"summary,omitempty"`

	// Prepare is what to do at the vendor, in order, before or while the
	// form is filled in. Empty for a tool with nothing to do there.
	Prepare []Step `json:"prepare,omitempty"`

	// Caution is the one risk the tool's credential carries, nil where it
	// carries none worth a warning. setupapi's copy test names the tools
	// that carry one, so a warning is a decision rather than a habit.
	Caution *Caution `json:"caution,omitempty"`
}

// Step is one thing to do at the vendor.
type Step struct {
	// Text is the step, imperative, in one sentence. [LinkMark] places the
	// vendor link of the field Link names.
	Text string `json:"text"`

	// Link names the field whose vendor link [LinkMark] stands for, by its
	// [Requirement.Field] in the same tool's lists.
	Link string `json:"link,omitempty"`

	// Grants are the permissions, roles or scopes the step grants, each in
	// the vendor's own words, so a person can find and tick off each one.
	Grants []string `json:"grants,omitempty"`

	// Yields names the field the step produces a value for, by its
	// [Requirement.Field] in the same tool's lists: the join from a step to
	// the box its result is pasted into.
	Yields string `json:"yields,omitempty"`
}

// Caution is a credential's one risk, said once per form.
type Caution struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
