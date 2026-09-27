package chart

import (
	"fmt"
	"slices"
	"strings"

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
// ([Writer.record]), against what the writer's own party holds — and about
// what each record CHANGES, which only the decide's own snapshot can say
// ([requirement]).
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
// # Fail-closed, including for a record this file does not name
//
// [Writer.record] takes the class as an argument every decide must state, so a
// record added later cannot reach the log without somebody choosing what it
// asks for; and a comparison that cannot tell whether a value changed — a row
// whose runtime half does not decode — answers that it did. Over-gated fails at
// the first write with a message naming the grant; ungated fails silently,
// for ever, and looks correct.

// PayloadClass is what a record asks for, as far as a grant can settle it.
type PayloadClass string

const (
	// ClassPublic is a content record that changes only the half this
	// domain can read and that no authority is derived from — a name, a
	// purpose, a goal. No grant decides it: it is the object's lead's,
	// which is a relation.
	ClassPublic PayloadClass = "public"

	// ClassPrivileged is a content record that CHANGES the OPAQUE half,
	// which travels on the row's `document` because this domain can say
	// what a unit key and a parent mean and cannot say what an `mcp_env`
	// key is for. That is the company's configuration by another name.
	//
	// A CHANGE, NOT A PRESENCE. A content record is full post-state, so
	// every one carries the runtime half its object will hold — the one
	// the decide carried forward from the row when the caller left it out
	// ([SeatContent.Runtime]) — and a class read off what the payload
	// merely CARRIES would make every edit of a seat that has a model
	// chain a configuration change. What the decide compares against the
	// row it read is what the record asks for.
	ClassPrivileged PayloadClass = "privileged"

	// ClassStructure is every record the chart serialises on one subject
	// for the whole tree — a create, a move, a rename, a removal, an import.
	// A rename is among them for the same reason a create is: an address is
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

// requirement is what one record asks of the party publishing it, decided by
// the decide that forms the record.
//
// # Why the decide decides it, and not the payload
//
// The payload says what the record CARRIES, and the class is about what it
// CHANGES: the same runtime half is a configuration change on a seat whose row
// holds another one and nothing at all on a seat whose row holds exactly it.
// Only the decide's own snapshot holds the row to compare against, so the
// class is formed there — in the transaction the expectation is formed in —
// and handed to [Writer.record] rather than recomputed from bytes that cannot
// say.
type requirement struct {
	class PayloadClass

	// fields are the fields of a content record whose change asked for
	// its class, in the order the record states them — what a refusal
	// names, so the person reading it knows which edit to take back.
	// Empty for a record whose whole kind asks (structure).
	fields []string
}

// structural is the requirement every record on the tree's subject states.
var structural = requirement{class: ClassStructure}

// contentRequirement is the requirement of a content record whose changed
// fields ask for the company's grant: public where none did.
func contentRequirement(changed []string) requirement {
	if len(changed) == 0 {
		return requirement{class: ClassPublic}
	}
	return requirement{class: ClassPrivileged, fields: changed}
}

// mayAuthor refuses a record this writer's party is not entitled to publish.
//
// [ErrRefused] rather than a bare error, so the three outcomes a surface
// renders stay three: this is a decision that will not change on a retry, not
// a contention and not a fault.
func (w *Writer) mayAuthor(object ObjectRef, need requirement) error {
	grant := GrantFor(need.class)
	if grant == "" || slices.Contains(w.Grants, grant) {
		return nil
	}
	if len(need.fields) > 0 {
		return fmt.Errorf("chart: %s acts with %v and this write to %s changes "+
			"%s, which needs %q — the half of a chart object this domain holds "+
			"as opaque bytes is the company's configuration (a seat's models, "+
			"its credentials, its sandbox cell, its mcp_env), so it is not one "+
			"team's to change. Leave it out to keep what the object holds: %w",
			w.Actor, w.Grants, object, strings.Join(need.fields, ", "), grant,
			ErrRefused)
	}
	return fmt.Errorf("chart: %s acts with %v and a %s record needs %q — "+
		"the chart's structure is the company's shape, so it is not one "+
		"team's to change: %w",
		w.Actor, w.Grants, need.class, grant, ErrRefused)
}
