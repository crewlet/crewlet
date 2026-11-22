package growth

import "slices"

// Connection is one connection of a product's block as a product's details
// answer it (`GET /company`, the integration's `connections`): its public
// identity, the roles the engine reads that it holds, and the accounts it
// reaches. Never a credential.
type Connection struct {
	ID string `json:"id"`
	// Name is the vendor's name for what the connection signs in to (a
	// team, an organization, a business portfolio).
	Name string `json:"name" untrusted:"name"`
	// Identity is public: an email, a key ID or a user ID.
	Identity  string    `json:"identity"`
	State     string    `json:"state"`
	RolesHeld []string  `json:"roles_held"`
	Accounts  []Account `json:"accounts"`
	// Open is a connection the company does not narrow to named accounts:
	// it reaches whatever its credentials reach at the vendor, which
	// Accounts lists where the engine read it.
	Open bool `json:"open,omitempty"`
	// Unread is why the engine could not read what the connection reaches,
	// in words a person acts on; empty where it could.
	Unread string `json:"unread,omitempty"`
}

// RoleOffer is one vendor role as setup explains it (the integration's
// `roles`): what it allows, by class, and the seat levels it serves.
type RoleOffer struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Note    string  `json:"note"`
	Classes []Class `json:"classes"`
	Levels  []Level `json:"levels"`
}

// RoleOffers is every role of c with the classes of the operations it may
// call (an operation naming no role is every role's) and the levels those
// classes need, in rank order.
func (c Catalog) RoleOffers() []RoleOffer {
	out := make([]RoleOffer, 0, len(c.Roles))
	for _, r := range c.Roles {
		offer := RoleOffer{ID: r.ID, Name: r.Name, Note: r.Note}
		for _, op := range c.Operations {
			if !op.Class.Callable() || !Permits(op, []string{r.ID}) {
				continue
			}
			for _, cl := range op.classes() {
				if cl.Callable() && !slices.Contains(offer.Classes, cl) {
					offer.Classes = append(offer.Classes, cl)
				}
			}
		}
		slices.SortFunc(offer.Classes, func(a, b Class) int { return a.Rank() - b.Rank() })
		for _, cl := range offer.Classes {
			if l := cl.Level(); !slices.Contains(offer.Levels, l) {
				offer.Levels = append(offer.Levels, l)
			}
		}
		out = append(out, offer)
	}
	return out
}
