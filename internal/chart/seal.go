package chart

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/secrets"
)

// WHAT A CHART RECORD MAY AND MAY NOT CARRY IN PLAINTEXT.
//
// # The rule this replaces, and why it had to change
//
// A Tier B company document is sealed WHOLE — one opaque blob — and
// [internal/secrets] says why in one sentence: per-field sealing looks tidier
// and leaks the SHAPE, and an operator's org chart, their role names, which
// integrations they run and how many seats they have are all shape.
//
// That argument is correct about a DOCUMENT and does not survive the chart
// becoming a log. A log's records are arbitrated per object at the broker, and
// a whole-document blob has exactly one subject: sealing the chart that way
// would put every writer back on one lock and undo the entire reason the domain
// exists. And the shape is on the wire regardless — the SUBJECT of every record
// is a unit key or a seat handle, in a broker path, which is the org chart's
// shape spelled out one token at a time.
//
// So the chart's own trade is stated rather than inherited:
//
//   - THE STRUCTURE IS PLAINTEXT, deliberately and unavoidably. Keys, handles,
//     parents and leads are subjects and scope paths; an operator running this
//     engine can see them, and so can anyone who can read the broker.
//   - A SECRET-TAGGED VALUE IS NEVER PLAINTEXT. A literal credential in a
//     content record is sealed into the company's secret store and the record
//     carries a `${VAR}` REFERENCE — never the value, on the wire or in a row.
//   - A PERSON'S OWN FIELDS ARE SEALED UNDER A KEY THAT CAN BE DELETED. A human
//     seat's name, email and contact identities are personal data rather than
//     company structure, so they ride under a per-seat key: deleting that one
//     key makes every copy of those fields — in every node's rows, in every
//     snapshot, in the log itself — unreadable, which is the only erasure a
//     write-ahead log can actually offer.
//
// # Why the predicate is envref.Whole everywhere
//
// "Is this a credential or a pointer at one" is asked in four places, and a
// second spelling of it is the defect: a value that one place reads as a
// reference and another as a literal is either a credential written to a row or
// a pointer sealed as though it were the thing it points at. [envref.Whole] is
// the one test, and it is deliberately strict — a WHOLE `${VAR}` and nothing
// else, so `Bearer ${TOKEN}` is a literal that happens to mention a variable
// and is sealed like any other.

// Sealer turns a literal credential into a reference the record may carry.
//
// DEFINED BY THE CONSUMER, as every seam in this tree is: the chart needs one
// verb and it is not the secret store's whole surface. What satisfies
// it is [internal/fleetsecrets], wired by the engine.
type Sealer interface {
	// Seal stores value under name and returns nothing but an error. The
	// NAME is the caller's, derived deterministically from the object and
	// the field, so a re-seal of one field overwrites rather than
	// accumulating a key per edit.
	//
	// BY IS THE PARTY WRITING THE SEAT — the person who typed the
	// credential into it, and the credential they typed it through — which
	// the secret store records as the value's author. It used to record the
	// NODE the write happened to land on, so every credential a founder put
	// into a seat read as set by a machine nobody chose.
	Seal(ctx context.Context, name, value string, by secrets.Author) error
}

// SecretRef is the name a sealed value is stored under, rendered as the
// reference a record carries.
//
// DERIVED AND NOT MINTED, so a second edit of one field overwrites the value
// rather than leaving the previous one in the store for ever. The store has no
// retention: a key nothing overwrites and nothing deletes outlives the company.
//
// The shape is `CHART_<KIND>_<ID>_<FIELD>`, upper-cased with every character a
// variable name cannot take folded to `_`. It is not reversible and is not
// meant to be: what reads it is the resolver, which looks the name up.
func SecretRef(object ObjectRef, field string) string {
	return "${" + SecretName(object, field) + "}"
}

// SecretName is [SecretRef] without the reference syntax.
func SecretName(object ObjectRef, field string) string {
	return "CHART_" + varToken(string(object.Kind)) + "_" +
		varToken(NormalizeKey(object.ID)) + "_" + varToken(field)
}

// varToken folds one segment into what a variable name may hold.
func varToken(in string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sealValue is the one place a secret-tagged value is decided about.
//
// THREE ANSWERS, and the middle one is why the function exists:
//
//   - A WHOLE `${VAR}` is stored VERBATIM. It names a credential rather than
//     being one, it is what an operator edits, and sealing it would put a
//     pointer inside the store and a pointer to that pointer on the record.
//   - AN EMPTY VALUE is stored verbatim too, and is a real setting: an
//     operator who deliberately cleared a credential has said something, and
//     sealing "" would store an empty secret and hand back a reference that
//     resolves to nothing.
//   - ANYTHING ELSE is a literal. It is sealed under a derived name and the
//     record carries the reference — so the value reaches the store and never
//     the log, the rows, a snapshot or a backup of any of them.
func (w *Writer) sealValue(ctx context.Context, object ObjectRef, field, value string) (
	string, error) {

	if value == "" {
		return "", nil
	}
	if _, whole := envref.Whole(value); whole {
		return value, nil
	}
	if w.seal == nil {
		return "", fmt.Errorf("chart: %s on %s holds a literal credential and "+
			"this node has no secret store to seal it into — the value is "+
			"REFUSED rather than written to the log, which every node applies "+
			"and every snapshot copies. Configure the secret store, or give "+
			"the field a ${VAR} reference", field, object)
	}
	name := SecretName(object, field)
	if err := w.seal.Seal(ctx, name, value, secrets.Author{
		Name: w.Actor, Kind: string(w.ActorKind), OperatorID: w.OperatorID,
	}); err != nil {
		return "", fmt.Errorf("chart: seal %s on %s: %w", field, object, err)
	}
	return SecretRef(object, field), nil
}

// marshal is json.Marshal, named so the encode path reads as one step.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }
