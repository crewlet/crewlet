package iam

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

// AssistPrefix is the class the Dashboard Assistant's grant carries where the
// audit trail records which credential a request was made through:
// `assist:<grant>` in the operator column, beside the person as the author
// (ADR-0046).
//
// A THIRD CREDENTIAL CLASS beside [MachineTokenPrefix] and [SessionPrefix],
// because a request the assistant makes is the person's own, decided by their
// own grants, and still has to say in every row it writes that it came
// through the assistant rather than from their keyboard. The grant names the
// session it was derived from, the conversation and the turn, in the record
// internal/api/auth keeps of it; the row only has to point there.
//
// A CLASS NO LOGIN MAY TAKE, for [MachineTokenPrefix]'s reason: a service
// account enrolled as `assist:<something>` would read as the assistant in
// every trail that records one.
const AssistPrefix = "assist:"

// AssistBearerPrefix marks a bearer as an assistant grant by its SHAPE, so the
// request guard sends it to the assistant arm before any other comparison and
// refuses it before routing when it arrives over the wire (internal/api/auth).
//
// HERE, IN THE LEAF, because two packages must agree on it and one of them
// cannot import the other: the guard reads it, and Tier A refuses a configured
// token value that starts with it (internal/config), since such a value would
// always be read as a grant and could never authenticate as itself.
const AssistBearerPrefix = "cwl_ast_"

// AssistName is how an assistant grant is recorded as the credential a
// request was made through ([Principal.Via]).
func AssistName(grant uuid.UUID) string { return AssistPrefix + grant.String() }

// ValidAssistName reports whether s names an assistant grant: the class and a
// grant id in its canonical form, which is the only form one is minted in.
func ValidAssistName(s string) bool { return validCredential(s, AssistPrefix) }

// AssistScope is how far an assistant grant reaches, narrowing what the
// person's own grants admit and never widening it (ADR-0046, ADR-0048).
//
// TWO VALUES AND NO ZERO MEANING. A grant that reads a turn's answers is not a
// grant that may write anything, and the one that writes is bound to exactly
// one approved request; a zero scope read as either would be a grant nobody
// decided the reach of.
type AssistScope string

const (
	// AssistScopeRead is a turn's tool loop: reads only, on rows the
	// authority table marks as the assistant's to read.
	AssistScopeRead AssistScope = "read"
	// AssistScopeExec is one approved write: the one request its proposal's
	// digest binds, once.
	AssistScopeExec AssistScope = "exec"
)

// AssistScopes are the two, in declaration order.
var AssistScopes = []AssistScope{AssistScopeRead, AssistScopeExec}

// Valid reports whether a scope is one this build knows.
func (s AssistScope) Valid() bool { return slices.Contains(AssistScopes, s) }

// Assisted is the id of the assistant grant this principal acts through, and
// whether it acts through one at all.
//
// THE ONE FACT ON A PRINCIPAL THAT SAYS THE ASSISTANT MADE THIS REQUEST, the
// twin of [Principal.MachineToken]: the principal is the person, composed from
// their identity rows exactly as their own browser's request would be, so
// neither the kind nor the grants can tell the two apart. What it came through
// can, read here once off [Principal.Via]; internal/authz refuses it every
// verb its row keeps from the assistant, and [Principal.AssistScope] says how
// far it reaches.
func (p Principal) Assisted() (uuid.UUID, bool) {
	raw, ok := strings.CutPrefix(p.Via, AssistPrefix)
	if !ok || !ValidAssistName(p.Via) {
		return uuid.Nil, false
	}
	grant, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return grant, true
}
