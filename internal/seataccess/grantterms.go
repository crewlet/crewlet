package seataccess

// GrantTerms is how an agent reads one lane's grants, in the vendor's own
// terms: what the vendor answers a call the credential lacks a permission for,
// what a grant names, and what its scope entries are. The request tool and a
// seat's access note are written from it, so neither names a vendor itself.
type GrantTerms struct {
	// Refusal is the vendor's answer to a missing permission, as an agent
	// meets it: `403 "request is not authorized"`.
	Refusal string

	// Permission is what a grant names, with an example: "a permission
	// group's API name, such as `Origin Write`".
	Permission string

	// Scope is what a scope entry is and what none means; empty where a
	// grant takes no scope.
	Scope string

	// Whole is where a scoped grant naming no entry applies ("every zone the
	// company's agents reach", "its own projects"), and Account where one on
	// the whole connection does ("the account", "the Organization").
	Whole, Account string
}

// GrantTermsOf is lane's [GrantTerms], zero for a lane that takes no grants
// or whose vendor this build does not register.
func GrantTermsOf(lane Lane) GrantTerms {
	v, ok := Lookup(lane.Kind())
	if !ok {
		return GrantTerms{}
	}
	spec, ok := v.Catalog().Lane(lane)
	if !ok || !spec.Grants {
		return GrantTerms{}
	}
	return spec.GrantTerms
}
