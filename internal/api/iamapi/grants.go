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
// where it names a permission this build does not know. It runs BEFORE ANY KEY
// IS READ OR LINK MINTED: a refusal fixed by typing a field publishes nothing.
//
// Absent is refused and `[]` is a value (the overview alone, ADR-0033): no
// default could stand for an omission, so the field is a POINTER. The CLI and
// the dashboard state a default of their own, out loud.
//
// An unknown name is refused HERE, naming the set, so the caller learns which
// exist rather than that they lack a permission nobody holds
// ([iamdomain.Writer.MayConfer] refuses it again in the record's snapshot).
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
