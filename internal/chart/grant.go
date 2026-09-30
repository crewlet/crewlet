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
// # Four classes of field, and the one a lead may write
//
// Every field of a chart object is in exactly one of four classes, and only
// the first is the lead's:
//
//   - PROSE — a seat's name, backstory, goal, responsibilities and behavioural
//     guidelines; a unit's name, type, purpose, goals and knowledge refs.
//     Whoever leads the object, decided at the door.
//   - AUTHORITY-BEARING RELATIONS — a seat's `project`, `space` and `email`;
//     a unit's `project`, `space` and `channel`. The company's grant, because
//     each is what somebody's AUTHORITY is derived from: `project` and `space`
//     are which tracker project and which page container a seat or unit leads,
//     so a lead pointing their unit at another team's key — or at the org
//     root's container — took over its removals, its archive and its policy; a
//     unit's `channel` is which chat channel's messages it answers; and a
//     seat's `email` is whose vendor actions — a Jira comment, a push — are
//     attributed and routed to it. [ownFields] names them per kind, and a
//     write CHANGING one is [ClassPrivileged].
//   - RUNTIME — the opaque half. The company's grant.
//   - STRUCTURE — create, move, lead, kind, `manages:`, rename:
//     [ClassStructure], the company's grant. A seat's `manages:` list is who
//     its manager is — a lead adding the founder to a report's list made
//     themselves the founder's ancestor — and it is STRUCTURE rather than a
//     relation on the content record because a rename's cascade moves its
//     entries on the tree's subject: restated by a content write on the
//     seat's own, a list read before a rename applied was accepted after it
//     and wrote the renamed entry back ([OpSetManages]). And a REMOVAL,
//     [ClassRemoval], which takes the deployment's grant as well: it is the
//     one structural change nothing undoes (see that class).
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
	// key is for — that is the company's configuration by another name —
	// or that changes a field somebody's authority is derived from (see
	// this file's header).
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
	// for the whole tree that PLACES something — a create, a move, a lead,
	// a kind, a `manages:` list, a rename, an import. A rename is among
	// them for the same reason a create is: an address is how every other
	// domain refers to an object, so reassigning one in a namespace the
	// whole company shares is not a fact about one team.
	ClassStructure PayloadClass = "structure"

	// ClassRemoval is the record that takes objects OUT of the chart —
	// structure too, and the one structural change nothing undoes. A
	// removed address is tombstoned for ever, so no create, rename or
	// import may take it again; the seat's mailbox, lease and diary go
	// with it; and every node's removal gate drops whatever is still in
	// flight to the object. So it takes the deployment's grant beside the
	// company's — a purge's bar, for a purge's blast radius — and an
	// automation holding only the grant that applies a configuration
	// cannot dissolve a team between two of its runs.
	ClassRemoval PayloadClass = "removal"

	// ClassNodeGate is a node's eviction from this log, or its readmission:
	// a gesture about the DEPLOYMENT rather than the company, so it takes
	// the deployment's grant alone. Whether the node may be evicted at all
	// is judged once, before any log is written, by the engine's node gate.
	ClassNodeGate PayloadClass = "node_gate"
)

// GrantsFor is the capabilities a class requires — every one of them — or
// none where no grant decides it.
//
// EXPORTED so a door can state the same requirement without spelling it
// again — a second copy of this mapping is how one surface starts asking for
// a grant the domain does not want and another stops asking for one it does.
// A fresh slice each call, so a caller appending to one answer is not editing
// the next.
func GrantsFor(c PayloadClass) []iam.Grant {
	switch c {
	case ClassPublic:
		return nil
	case ClassRemoval:
		return []iam.Grant{iam.GrantConfigWrite, iam.GrantFleetOperate}
	case ClassNodeGate:
		return []iam.Grant{iam.GrantFleetOperate}
	}
	// FAIL-CLOSED for a class nobody named here, as for the two that are:
	// a record is refused below the company's grant unless a decide said
	// it is public.
	return []iam.Grant{iam.GrantConfigWrite}
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

// structural is the requirement every placing record on the tree's subject
// states, and removal the requirement of the record that takes objects out.
var (
	structural = requirement{class: ClassStructure}
	removal    = requirement{class: ClassRemoval}
	nodeGate   = requirement{class: ClassNodeGate}
)

// contentRequirement is the requirement of a content record whose changed
// fields ask for the company's grant: public where none did.
func contentRequirement(changed []string) requirement {
	if len(changed) == 0 {
		return requirement{class: ClassPublic}
	}
	return requirement{class: ClassPrivileged, fields: changed}
}

// ownFields are the content fields whose change asks for the company's grant,
// per kind of object — the authority-bearing relations of this file's header,
// and the runtime half.
//
// ONE LIST PER KIND, in the order a refusal names them, and the decides read
// their comparisons off it ([fieldChanges]) rather than spelling the fields a
// second time: a field added to a decide and not here would be compared by
// nothing, which is a lead's write the company's grant was meant to decide.
var ownFields = map[ObjectKind][]string{
	KindSeat: {"email", "project", "space", "runtime"},
	KindUnit: {"channel", "project", "space", "runtime"},
}

// fieldChanges is which of kind's [ownFields] a write changes, given whether
// each one does — every field in the list must be answered, and an answer for
// a field the list does not hold is a build mistake reported as one.
func fieldChanges(kind ObjectKind, changed map[string]bool) ([]string, error) {
	fields := ownFields[kind]
	if len(changed) != len(fields) {
		return nil, fmt.Errorf("chart: a %s write compared %d of its %d "+
			"privileged fields (%v) — every one has to be answered, or a "+
			"change nothing compared is a change nobody may refuse",
			kind, len(changed), len(fields), fields)
	}
	var out []string
	for _, field := range fields {
		differs, answered := changed[field]
		if !answered {
			return nil, fmt.Errorf("chart: a %s write did not say whether it "+
				"changes %s", kind, field)
		}
		if differs {
			out = append(out, field)
		}
	}
	return out, nil
}

// GrantRefusal is a record refused because the party publishing it does not
// hold a capability the record needs.
//
// TYPED, because the surface renders it as the refusal it is — `403
// unauthorized` naming the grant — where every other refusal of this domain's
// rules is the caller's to fix in the body. It answers [ErrRefused] too, since
// it is one: a decision a retry will not change.
type GrantRefusal struct {
	// Object is what the record was about: the object a content write
	// names, or the tree for a structural record.
	Object ObjectRef

	// Class is what the record asked for.
	Class PayloadClass

	// Grants are the capabilities the record needs and the party does not
	// hold — every one of them needed.
	Grants []iam.Grant

	// Fields are the content fields whose change asked for them, in
	// [ownFields]' order; empty for a record whose whole kind asks.
	Fields []string

	// Actor and Held are the party refused, and what it holds.
	Actor string
	Held  []iam.Grant
}

func (e *GrantRefusal) Error() string {
	if e.Class == ClassRemoval {
		return fmt.Sprintf("chart: %s acts with %v and a removal needs %v — a "+
			"removed address is tombstoned for ever and the seat's mailbox goes "+
			"with it, so taking an object out of the chart is the company's "+
			"shape to change AND the deployment's to make irreversible",
			e.Actor, e.Held, e.Grants)
	}
	if len(e.Fields) > 0 {
		return fmt.Sprintf("chart: %s acts with %v, and this write to %s changes "+
			"%s, which needs %v. A chart object's runtime half is the company's "+
			"configuration (a seat's models, its credentials, its sandbox cell, "+
			"its mcp_env), and a seat's project, space and email and a unit's "+
			"project, space and channel are what leadership and attribution "+
			"are derived from — so none of them is one team's to change. Send "+
			"the value you read to keep it, and leave the runtime out",
			e.Actor, e.Held, e.Object, strings.Join(e.Fields, ", "),
			e.Grants)
	}
	return fmt.Sprintf("chart: %s acts with %v and a %s record needs %v — the "+
		"chart's structure is the company's shape, so it is not one team's to "+
		"change", e.Actor, e.Held, e.Class, e.Grants)
}

// Is makes a grant refusal answer [ErrRefused].
func (e *GrantRefusal) Is(target error) bool { return target == ErrRefused }

// mayAuthor refuses a record this writer's party is not entitled to publish,
// with a [GrantRefusal] naming what the party LACKS.
//
// [ErrRefused] rather than a bare error, so the three outcomes a surface
// renders stay three: this is a decision that will not change on a retry, not
// a contention and not a fault.
//
// THE MISSING ONES AND NOT ALL OF THEM, which is how internal/authz names a
// refusal of a row asking for two grants: an administrator holding the
// company's grant refused a removal is told the deployment's is missing, and
// not sent to ask for one they already hold.
func (w *Writer) mayAuthor(object ObjectRef, need requirement) error {
	var missing []iam.Grant
	for _, grant := range GrantsFor(need.class) {
		if !slices.Contains(w.Grants, grant) {
			missing = append(missing, grant)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &GrantRefusal{
		Object: object, Class: need.class, Grants: missing,
		Fields: need.fields, Actor: w.Actor, Held: slices.Clone(w.Grants),
	}
}
