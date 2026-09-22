package chart

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT A WRITER MAY AUTHOR, decided by the DOMAIN and not only by the door.
//
// # Why the domain asks at all
//
// Four doors reach this package's writes — the HTTP surface, the operator
// MCP, the command line and the engine's own seeding — and each decides
// authority for itself before calling. That is one decision per door, which
// is one place per door to forget it, and what is forgotten is not a
// nuisance: a seat's RUNTIME half is its model chain, its credentials, its
// sandbox cell and its `mcp_env`, and a stdio MCP server is exec.Command with
// the config's command. A door that skipped the check would hand shell on
// every engine host to whoever could reach it, and the record would look
// exactly like a legitimate one for ever afterwards.
//
// So the domain asks too, from the one place every door funnels through
// ([Writer.record]), against what the writer's own party holds.
//
// # It is NOT a second opinion about the same question
//
// internal/authz decides a RELATION — does this principal LEAD that unit —
// which this package cannot see and must not try to: the chart it would read
// the relation out of is the state the write is changing, and a decision
// taken inside the decide's own snapshot would be a second router. What is
// settled here is the half a GRANT settles, where the answer depends on
// nothing but what the party carries and what the payload holds, and is
// therefore the same on every surface.
//
// The division is exact. A public content edit is a RELATION and is refused
// nowhere here; everything a grant can decide is refused here as well as at
// the door.
//
// # Fail-closed, including for a payload this file does not name
//
// [classOf] answers [ClassStructure] for anything it does not recognise, so a
// payload added later is over-gated rather than ungated. Over-gated fails at
// the first write with a message naming the grant; ungated fails silently,
// for ever, and looks correct.

// PayloadClass is what a record asks for, as far as a grant can settle it.
type PayloadClass string

const (
	// ClassPublic is a content record carrying only the half this domain
	// can read — a name, a purpose, a goal, who somebody manages. No grant
	// decides it: it is the unit's lead's, which is a relation.
	ClassPublic PayloadClass = "public"

	// ClassPrivileged is a content record carrying the OPAQUE half, which
	// travels on the row's `document` because this domain can say what a
	// unit key and a parent mean and cannot say what an `mcp_env` key is
	// for. That is the company's configuration by another name.
	ClassPrivileged PayloadClass = "privileged"

	// ClassStructure is every record the chart serialises on one subject
	// for the whole tree — a create, a move, a removal, an import — plus a
	// rekey, which is per-object and is still the company's: an address is
	// how every other domain refers to an object, so reassigning one in a
	// namespace the whole company shares is not a fact about one team.
	ClassStructure PayloadClass = "structure"
)

// GrantFor is the capability a class requires, or empty where none does.
//
// EXPORTED so a door can state the same requirement without spelling it
// again — a second copy of this mapping is how one surface starts asking for
// a grant the domain does not want and another stops asking for one it does.
func GrantFor(c PayloadClass) iam.Grant {
	if c == ClassPublic {
		return ""
	}
	return iam.GrantConfigWrite
}

// classOf reports what a record asks for, from the payload alone.
//
// THE PAYLOAD AND NOT THE OP, because the op says which record this is and
// the payload says what it carries: one [OpUpsert] is a lead correcting a
// goal and the next is somebody handing a seat a credential, and only the
// second is the company's to decide.
func classOf(payload any) PayloadClass {
	switch p := payload.(type) {
	case UnitPayload:
		return contentClass(p.Runtime)
	case SeatPayload:
		return contentClass(p.Runtime)
	}
	return ClassStructure
}

// contentClass is one content payload's class, from whether it carries the
// opaque half.
//
// AN EMPTY RUNTIME HALF IS PUBLIC, and that is not a hole a caller can use:
// a content record is FULL POST-STATE, so a write that omits the runtime half
// CLEARS it. Clearing a seat's model chain and its credentials is a change to
// the company's configuration and is refused as one — which is why the test
// for it is a control rather than an afterthought.
func contentClass(runtime []byte) PayloadClass {
	if len(runtime) > 0 {
		return ClassPrivileged
	}
	return ClassPublic
}

// mayReplace refuses a content write that would CLEAR an opaque half the
// party may not author.
//
// A content record is FULL POST-STATE, so a write that omits the runtime half
// SETS it to empty. Without this, deleting every seat's model chain and
// credentials one request at a time would be a public edit — the payload
// carries no privileged bytes, and what it destroys is the whole of them.
//
// IT IS A SECOND CALL RATHER THAN A CLEVERER classOf, because the two
// questions read different things: what is being written is on the payload,
// and what is being replaced is in a row only the decide's own snapshot may
// read. Folding them would mean passing a row into [Writer.record], which
// every structural decide would then have to supply and none of them has.
func (w *Writer) mayReplace(prior []byte) error {
	return w.mayAuthor(contentClass(prior))
}

// mayAuthor refuses a record this writer's party is not entitled to publish.
//
// [ErrRefused] rather than a bare error, so the three outcomes a surface
// renders stay three: this is a decision that will not change on a retry, not
// a contention and not a fault.
func (w *Writer) mayAuthor(c PayloadClass) error {
	need := GrantFor(c)
	if need == "" || slices.Contains(w.Grants, need) {
		return nil
	}
	return fmt.Errorf("chart: %s acts with %v and a %s record needs %q — "+
		"the half of a chart object this domain holds as opaque bytes is "+
		"the company's configuration (a seat's models, its credentials, its "+
		"sandbox cell, its mcp_env), and its structure is the company's "+
		"shape, so neither is one team's to change: %w",
		w.Actor, w.Grants, c, need, ErrRefused)
}
