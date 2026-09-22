package chart

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/iam"
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
// DEFINED BY THE CONSUMER, as every seam in this tree is: the chart needs two
// verbs and neither of them is the secret store's whole surface. What satisfies
// it is [internal/fleetsecrets], wired by the engine.
type Sealer interface {
	// Seal stores value under name and returns nothing but an error. The
	// NAME is the caller's, derived deterministically from the object and
	// the field, so a re-seal of one field overwrites rather than
	// accumulating a key per edit.
	Seal(ctx context.Context, name, value string) error

	// BlindKey is the keyed material a blind index is computed with. It is
	// a SEPARATE key from the one that seals values: a blind index is a
	// lookup surface that every node computes and compares, so the key has
	// to be readable by every node — where a sealing key ideally is not.
	BlindKey(ctx context.Context) ([]byte, error)
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
	if err := w.seal.Seal(ctx, name, value); err != nil {
		return "", fmt.Errorf("chart: seal %s on %s: %w", field, object, err)
	}
	return SecretRef(object, field), nil
}

// BlindIndex is the searchable form of a value whose plaintext is sealed.
//
// # What it is for
//
// A vendor payload carries an email address, and something has to turn that
// into a seat. A plaintext column would answer that in one indexed read and
// would also be the personal data the sealing exists to protect — in every
// node's rows, in every snapshot, in every backup.
//
// So the column holds a KEYED HASH instead: every node computes the same value
// from the same address under the fleet's own key, so the lookup is still one
// indexed read, and the column reveals nothing to anyone without the key. It is
// keyed rather than a plain digest because an email address has far too little
// entropy for an unkeyed hash to hide it — the whole corpus of plausible
// addresses at one company is enumerable in seconds.
//
// # What it deliberately cannot do
//
// It answers EQUALITY and nothing else. There is no prefix search, no domain
// filter, no ordering. A surface that wants "everyone at example.com" cannot
// have it from this column, and that is the trade the sealing bought.
//
// The output is base32 without padding: it goes in a TEXT column and is
// compared for equality, so a case-stable alphabet with no `=` is what makes
// two nodes' values identical byte for byte without anybody normalising.
func BlindIndex(key []byte, value string) string {
	normalised := iam.NormalizeEmail(value)
	if normalised == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	// THE DOMAIN SEPARATOR is what stops one blind index being valid in
	// another: a key reused for two kinds of value would let a match in one
	// confirm a guess in the other.
	mac.Write([]byte("crewlet/chart/email\x00"))
	mac.Write([]byte(normalised))
	return base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(mac.Sum(nil))
}

// marshal is json.Marshal, named so the encode path reads as one step.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }
