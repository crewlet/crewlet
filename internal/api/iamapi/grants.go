package iamapi

import (
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// requireGrants answers whether a create or an invitation STATED what it
// confers, refusing it `400 invalid_body` naming `grants` where it did not and
// where it names a permission this build does not know.
//
// # Absent is refused, and `[]` is a value
//
// There is no default an omission could stand for here: a person enrolled
// holding every app and a person enrolled holding the overview alone are both
// legitimate, so a body that names neither is a body whose author did not
// decide, and the record that lands from it decides who may use Worklet
// and Doclet until somebody notices. The field is a POINTER so the
// two are told apart: `[]` is the overview alone (ADR-0033), and an omitted
// field is refused. The CLI and the dashboard each state a default of their
// own, out loud, before they send.
//
// # A name nobody knows is refused naming the set
//
// A conferral adds every name it lists, and a name this build does not know is
// one it may never add — kept on a row a newer peer wrote, but never written
// by this one ([iamdomain.Writer.MayConfer] refuses it again in the record's
// own snapshot). Refused HERE so the caller is told which names exist rather
// than that they lack a permission nobody holds.
//
// BEFORE ANY KEY IS READ OR LINK MINTED, as a missing seat is: a refusal the
// caller fixes by typing a field publishes nothing.
func requireGrants(w http.ResponseWriter, grants *[]iam.Grant, what string) bool {
	if grants == nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"field": "grants", "detail": "Send `grants` with " +
				what + ": the permissions it gives, from " + words.And(iam.AllGrants) +
				", or [] for the overview only."})
		return false
	}
	return refuseUnknown(w, "grants", *grants, nil)
}

// refuseUnknown refuses a list ADDING a name this build does not know — one in
// added that held does not already carry — `400 invalid_body` naming field and
// the set. It answers true where nothing was refused.
//
// held is what the row carries now: a name a newer peer wrote there is KEPT AND
// IGNORED (ADR-0033), so a whole set restating it adds nothing, and an edit
// that leaves it alone must not be refused for its presence.
func refuseUnknown(w http.ResponseWriter, field string, added,
	held []iam.Grant) bool {

	for _, g := range added {
		if g.Valid() || slices.Contains(held, g) {
			continue
		}
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"field": field, "detail": unknownGrant(g)})
		return false
	}
	return true
}

// unknownGrant is the refusal of a permission this build does not know,
// naming the set it does, in the words every surface refusing one uses
// ([iam.NotAGrant]): this route answers before the identity record would, so
// a sentence of its own here was the only one /iam ever told.
func unknownGrant(g iam.Grant) string { return iam.NotAGrant(g) + "." }
