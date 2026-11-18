package iamdomain

import "slices"

// Moved is what one committed batch of the identity log moved that something
// beyond the estate acts on: this node's contact routing, and every connection
// held open on a credential this estate answers for.
//
// It names the CREDENTIALS moved: a dashboard socket is authenticated once, at
// its handshake, and re-deciding every socket on every sign-out would be a
// directory read per tab. It names WHOSE standing moved, never how: a listener
// re-decides from the rows, committed before the value arrives. Over-reporting
// costs a re-read; under-reporting leaves a revoked credential serving an open
// connection, so an apply names everybody whose row it moved (an identity
// change, the login taken and the one left) and [Moved.Everyone] is for the
// few moves that cannot name anyone.
type Moved struct {
	// Seats is whether a seat's STANDING may have moved (a stage, a seat
	// bound or released, a removal): what this node's contact routing is
	// rebuilt from. A sign-in sets nothing: it is the bulk of this log's
	// traffic and moves no seat.
	Seats bool

	// People are the persons whose row a committed record moved (content,
	// stage, revocation epoch, login or seat, removal), by id, each once:
	// every session or machine token acting for one of them may now be
	// over or carry other grants.
	People []string

	// Logins are the logins those people held when their row moved, and
	// every login an identity change gave or took, each once. A Tier A
	// token binds through the directory row under its own login,
	// `token:<id>`, and never learns the person id: the login reaches it.
	Logins []string

	// Removed are the persons a committed record REMOVED, by id, each once
	// (a subset of People). The engine hands them to the domains that erase
	// what a person keeps there (Dashboard Assistant conversations,
	// ADR-0053), which cannot read this one.
	Removed []string

	// Sessions are the lineages a committed record ended: a sign-out, a
	// step-up replacing the session it was made from, an administrator
	// ending one by name. Each once.
	Sessions []string

	// Everyone is a move no list above can name: the fleet-wide session
	// generation moved, which ends every session and machine token in the
	// company at once; or the batch RETAINED a record this node cannot
	// read, whose person is inside the payload it could not open
	// ([Applier.Retained]). The engine sets it too, when an adoption or a
	// reopened estate replaced the rows with no committed batch to say what
	// moved, and when this node stops vouching for its rows.
	Everyone bool
}

// Empty reports whether nothing moved.
func (m Moved) Empty() bool { return !m.Seats && !m.Credentials() }

// Credentials reports whether anything moved that a credential depends on —
// every field but [Moved.Seats], which is about routing rather than about who
// may act.
func (m Moved) Credentials() bool {
	return m.Everyone || len(m.People) > 0 || len(m.Logins) > 0 ||
		len(m.Sessions) > 0
}

// person records that the row of person id, holding login, moved.
func (m *Moved) person(id, login string) {
	if id != "" && !slices.Contains(m.People, id) {
		m.People = append(m.People, id)
	}
	if login != "" && !slices.Contains(m.Logins, login) {
		m.Logins = append(m.Logins, login)
	}
}

// removed records that the person of id was removed.
func (m *Moved) removed(id string) {
	if id != "" && !slices.Contains(m.Removed, id) {
		m.Removed = append(m.Removed, id)
	}
}

// session records that the session of lineage ended.
func (m *Moved) session(lineage string) {
	if lineage != "" && !slices.Contains(m.Sessions, lineage) {
		m.Sessions = append(m.Sessions, lineage)
	}
}
