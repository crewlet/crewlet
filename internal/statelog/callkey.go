package statelog

import (
	"strconv"
	"strings"
	"time"
)

// CallKey is a caller's own idempotency key for one write: the unit of work
// that made it — a turn, or the request a person's write carried — when that
// unit began, which of its calls this is, and what the call was. Or the zero
// value, for a call nothing will repeat.
//
// ONE TYPE FOR EVERY DOMAIN THAT TAKES ONE. A key is the rule for what makes
// two calls one operation, and two copies of a rule are two answers: the
// wiki's own copy went without the digest of the call's arguments until a fix
// of its own.
//
// # Why a write takes one
//
// A write whose answer never arrived is sent again — a redelivered turn runs
// again, a person whose save answered `unknown` presses retry — and an
// operation id minted fresh on every call makes that second send a second
// record under new identities. Derived from this key instead
// ([CallKey.DeriveOpID]), the repetition is the SAME operation carrying the
// same ids: whatever keys on an operation — the broker's duplicate window, the
// operation ledger — sees one operation rather than two, and a create's second
// copy lands on the address and the id its first attempt took.
//
// # Why it carries an instant
//
// Every operation id carries the instant it was minted ([DeriveOpID]), and
// that instant is what the publisher reads to decide whether this node's
// operation ledger can vouch for a retry: a ledger that lost the first
// attempt's row to its retention sweep answers an operation minted before the
// loss `unknown` rather than deciding it again on rows that already hold it.
// So [CallKey.Since] is the UNIT OF WORK's instant, reproduced by every
// repetition, and never the call's: a re-run's clock is after the loss by
// construction, and an id carrying it would be decided and published twice.
//
// # Why it counts repeats
//
// The key and the call's arguments say WHICH write a call is, not WHEN it
// happened. A run that renamed a page to "Runbook", then to "Old runbook",
// then back to "Runbook" would derive one operation for the first and the
// third, and the third would be answered as the first's retry — applied, at
// the first's position — while the page stayed "Old runbook". [CallKey.Repeat]
// is how many earlier calls in the run asked for something else, and it is part
// of every derived id, so the third call is an operation of its own while a
// call repeated with nothing different in between — an executor asking twice,
// a retry after `unknown`, a re-run — stays one.
//
// # Why it carries the call's own arguments
//
// Because the count and the identity have to agree on what makes two calls
// different, and on their own they do not. [CallKey.Repeat] tells calls apart
// by their ARGUMENTS, as a model typed them; the parts a domain derives an id
// over are what those arguments RESOLVED to — an object's id, never the name
// it was typed as. Two calls typed differently can resolve alike, and
// then a count two of them share hands the later one the earlier one's id: the
// later call is answered with the earlier one's success and writes nothing.
// With a digest of the arguments in the identity ([CallKey.Args]) two calls
// derive one id only where they share the arguments and the count, and the
// call log gives a repeated call its earlier count only when nothing different
// came between: which is exactly a call made again, and nothing else. The
// parts still follow it, because a key that carries none — a request's, whose
// operation already names the call — is distinguished by them alone.
//
// # Why a domain may name the tool
//
// Where two of a domain's tools make ONE gesture, the count cannot tell them
// apart: it counts calls to one tool, so each is its own tool's first.
// Collapsing the two is the unsafe direction — a second remark answered as
// the first's retry and dropped with a success; naming the tool
// ([CallKey.Tool]) can only make a repetition a second write, which is what
// was asked for. The wiki has no such pair, leaves it empty, and an empty
// tool adds nothing to the identity — so the wiki's derived ids are what they
// were before the field existed.
//
// A STRUCT RATHER THAN A STRING because the writes that take one already take
// several strings in a row, and a key passed where a body was meant is a write
// that compiles and derives nothing; and because its values are one identity,
// which a caller could otherwise hand over part of.
//
// THE ZERO VALUE IS A REAL ONE: a call that names no seed — an operator's
// assistant over MCP, an import — writes under a fresh operation every time,
// because nothing will repeat it and a stable id would collapse every later
// write into the first.
//
// NOT ATTRIBUTION, which is why it is no domain's actor field: nothing about it
// is written to a history row, and an actor is the record of who wrote.
type CallKey struct {
	// Seed names the unit of work: a turn's work key or run id, or a
	// person's request key. Empty is a call nothing will repeat.
	Seed string

	// Since is when the unit of work Seed names began — a turn's earliest
	// trigger, a request's own mint instant — and the instant every id
	// derived from this key carries. It must be the seed's own, reproduced
	// by every repetition. A zero Since is read as minted before every loss
	// the ledger has had, so where this node's ledger has lost rows a keyed
	// write answers `unknown` rather than publishing: the caller that could
	// not say when its work began pays for it, never with a duplicate.
	Since time.Time

	// Repeat is how many earlier calls in the seed's run asked for
	// something else — to the same tool (turnctx.CallLog.Ordinal). Zero adds
	// nothing to a derived id, so a run's first call of each kind derives the
	// id it always did.
	Repeat int

	// Tool is the tool the call was made through, for a domain whose
	// gestures two tools can make — see the type's doc. Empty adds nothing
	// to a derived id.
	Tool string

	// Args is a digest of the arguments the call was made with
	// (turnctx.ArgsDigest), for a key whose Repeat a call log counted — a
	// turn's. Empty for a key whose seed already names the one call it was
	// derived for, which is a request's operation. See the type's doc for
	// why the count needs it beside it.
	Args string
}

// String is the seed as text.
func (k CallKey) String() string { return strings.TrimSpace(k.Seed) }

// Empty reports a key that names no unit of work: a call nothing will repeat,
// which a domain answers with a freshly minted id.
func (k CallKey) Empty() bool { return k.String() == "" }

// Identity is what a derived id is computed over: a namespace, the seed, the
// tool where the key names one, the parts that make this operation this
// operation, the call's own arguments where the key carries them, and the
// repeat count when there is one.
//
// ONE SPELLING for every derivation a domain makes — an operation's or a new
// page's — so they can never disagree about what makes two calls one.
func (k CallKey) Identity(namespace string, parts ...string) []string {
	out := make([]string, 0, len(parts)+5)
	out = append(out, namespace, k.String())
	if tool := strings.TrimSpace(k.Tool); tool != "" {
		out = append(out, tool)
	}
	out = append(out, parts...)
	if k.Args != "" {
		out = append(out, "args:"+k.Args)
	}
	if k.Repeat > 0 {
		out = append(out, "repeat:"+strconv.Itoa(k.Repeat))
	}
	return out
}

// DeriveOpID is the operation one keyed write appends under: [DeriveOpID]
// over [CallKey.Identity], at the unit of work's own instant and UNNAMED —
// the id is also what a write answers with, and what it names is on the
// record.
//
// The parts are the caller's: what makes this operation this operation, over
// what the caller ASKED for and never over anything read from the rows — a
// retry arrives after its first attempt may have moved them, and a value read
// at call time would differ between a request and its own retry.
func (k CallKey) DeriveOpID(namespace string, parts ...string) string {
	return DeriveOpID(k.Since, "", k.Identity(namespace, parts...)...)
}
