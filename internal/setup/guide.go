package setup

// LinkMark is where a field's vendor link sits in its Hint, its More or a
// Step's Text: a form draws [Requirement.LinkText] there, linked to its VendorURL.
const LinkMark = "{link}"

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
	// Text is the step, imperative; [LinkMark] places Link's vendor link.
	Text string `json:"text"`

	// Link names the field, in the same tool, whose vendor link Text places.
	Link string `json:"link,omitempty"`

	// Grants are the permissions, roles or scopes to grant, in the vendor's words.
	Grants []string `json:"grants,omitempty"`

	// Yields names the field, in the same tool, the step produces a value for.
	Yields string `json:"yields,omitempty"`
}

// Caution is a credential's one risk, said once per form.
type Caution struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
