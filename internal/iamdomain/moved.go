package iamdomain

import "slices"

// Moved is what one committed batch of the identity log moved that something
// beyond the estate acts on: this node's contact routing, and every connection
// held open on a credential this estate answers for.
//
// # Why it says WHOSE, where the signal it replaced said nothing
//
// The applier's post-commit signal used to carry nothing, because its one
// listener — the notify registry — is rebuilt WHOLE from a fresh read of the
// directory, and a list of changed people could only have been wrong about
// that. An open connection is the opposite case. A dashboard socket is
// authenticated once, at its handshake, and then held for as long as the tab
// is: what ends it is the record that ended its credential, and re-deciding
// every open socket on every sign-out anywhere in the company would be a
// directory read per tab per sign-out. So the value names the CREDENTIALS a
// batch moved, and each listener reads the part it needs — the registry the
// one flag, a socket whether any of the rest is about the credential it was
// opened with.
//
// # Whose, never what
//
// It names whose standing moved and never how. A listener re-decides from the
// rows, which this batch has already committed by the time the value is
// handed over, so a grant or a stage carried here could only be a second,
// older copy of what the rows say.
//
// # Over-reporting is harmless; under-reporting is not
//
// A listener told about somebody nothing happened to re-reads a row and
// changes nothing. A listener NOT told leaves a revoked credential serving an
// open connection until it closes on its own. So an apply names everybody whose
// row it moved — a release reads who held the claim before it clears it — and
// [Moved.Everyone] is left for the few moves that cannot name anyone.
type Moved struct {
	// Seats is whether a seat's STANDING may have moved — a person's
	// stage, a seat bound or released, a removal — which is what this
	// node's contact routing is rebuilt from. A sign-in sets nothing: it
	// is the bulk of this log's traffic and moves no seat.
	Seats bool

	// People are the persons whose row a committed record moved — their
	// content (grants, the credentials and machine tokens they hold),
	// their stage, their revocation epoch, a claim bound to them or taken
	// off them, their removal — by id, each once. Every credential acting
	// FOR one of them, a session they signed in with or a machine token
	// they own, may now be over or may carry other grants.
	People []string

	// Logins are the logins those people held when their row moved, and
	// every login a claim gave or took — each once. A credential composed
	// from configuration (a Tier A token) is bound through the directory
	// row under its own login, `token:<id>`, whose person id it never
	// learns, so the login is the one name that reaches it.
	Logins []string

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

// session records that the session of lineage ended.
func (m *Moved) session(lineage string) {
	if lineage != "" && !slices.Contains(m.Sessions, lineage) {
		m.Sessions = append(m.Sessions, lineage)
	}
}
