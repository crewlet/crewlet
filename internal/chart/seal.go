package chart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/redact"
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
//   - A SECRET-TAGGED VALUE IS NEVER PLAINTEXT, wherever in the object it sits.
//     Every literal credential a content record would carry is sealed into the
//     company's secret store first and the record carries a `${VAR}`
//     REFERENCE — never the value, on the wire, in a row, in a snapshot or in
//     a backup of either. That is a seat's address, and it is every credential
//     in the RUNTIME HALF: a seat's or a unit's `mcp_env`, the sandbox's env
//     and its setup steps' files and env, a seat's own Slack app, its
//     Mattermost bot, its GitHub App — whatever internal/org's types tag
//     `secret:"true"`, found by [Runtime] and never by a list kept here.
//   - A SEAT'S ADDRESS IS SEALED LIKE A CREDENTIAL, and nothing else about a
//     seat's own columns is. A seat's name and its contact identities are
//     chart content, in the clear on the log, in every node's rows and in
//     every snapshot. The place a PERSON's name and address live — sealed
//     under the fleet keyring and erased from every row when they are removed
//     — is the identity directory (internal/iamdomain), not the chart; a
//     leaver's seat stops naming them when the seat's own fields are edited.
//
// # The runtime half is sealed by walking it, and this package cannot read it
//
// The half is internal/org's shape and the organization model is built on
// this domain, so the writer is HANDED what reads it ([Runtime]) rather than
// knowing it. A writer built without one is refused rather than left to
// publish a half it cannot see into: it sealed only the address for as long
// as that was the one field it could name, and every credential beside it
// went onto the log in the clear while this file said none ever would.
//
// # A sealed name is derived from WHO, WHERE and WHICH WRITE
//
// A value is sealed under a name derived from the object's IDENTITY — the
// address it was created under, which no rename moves and the chart never
// issues twice — the path of the field inside it, and the OPERATION the write
// is ([SecretName]). The identity rather than the address, because an address
// is reassigned: keyed on the handle a seat answers to, a rename left the old
// name's value referenced by the renamed seat while a new hire given the freed
// handle sealed their own over it.
//
// THE OPERATION, because a seal happens INSIDE THE DECIDE — before the broker
// arbitrates the write and before it is published — and a name derived from
// the object and the field alone is the name the object's LIVE ROW already
// references. So a seal over it rotated the credential every node resolves
// whatever became of the write: lost to a concurrent writer, refused by a full
// log, abandoned by a cancelled caller — the caller was told nothing changed,
// the chart's history said nothing changed, and the working credential was
// gone. Under the operation's own name, a seal writes a value nothing names
// yet, and the one thing that moves a row onto it is the RECORD that carries
// the reference: a write that never lands leaves only a value no row names,
// which the orphan sweep collects ([OrphanedSeals]), and the value it would
// have replaced goes on resolving until one that does land. A rotation is
// therefore a new reference on the row, which is also what lets every node
// see it without comparing anything but names. (What a decide can refuse on
// its own — a mask with nothing behind it, a cap — it refuses BEFORE it seals,
// so those write nothing at all.)
//
// DERIVED FROM THE OPERATION ID rather than minted, so every re-decide of one
// write — a round the broker sent back, the retry an `unknown` outcome asks
// for, two nodes seeding one file — derives the same name and finds its own
// value there. And never from a digest of the VALUE: the name travels in the
// clear on the log, and a short hash of a low-entropy credential is a guess
// anybody can check offline.
//
// THE SEAL CREATES AND NEVER REPLACES ([Sealer]): a name already holding a
// value is confirmed when it holds this one — the retry above — and refused
// when it holds another, which only a caller reusing an operation id for a
// different change can cause. A put there would be the rotation this section
// exists to close, reached by the one path the name does not.
//
// # Why the predicate is envref.Whole everywhere, and envref.Split beside it
//
// "Is this a credential or a pointer at one" is asked in four places, and a
// second spelling of it is the defect: a value that one place reads as a
// reference and another as a literal is either a credential written to a row
// or a pointer sealed as though it were the thing it points at. [envref.Whole]
// is the one test, and it is deliberately strict — a WHOLE `${VAR}` and
// nothing else. A value that EMBEDS references beside literal text —
// `Bearer ${GITHUB_TOKEN}`, the ordinary way a header names a credential — is
// cut by [envref.Split] and only its LITERAL RUNS are sealed, each under a name
// of its own, so the record carries references alone and expanding it gives
// exactly what expanding the original gave. Sealing such a value whole would
// store the embedded reference as text nothing ever expands again, and the
// header would send the words `${GITHUB_TOKEN}` to the vendor.
//
// CONTENT IS THE ONE EXCEPTION, and for the mirror of that reason: a setup
// step's file is written into a box and never expanded by the engine, so a
// `${…}` in a script or an .npmrc is the FILE's syntax. It is sealed whole
// (the walk says which values are content, [secrets.Path.Content]) and read
// whole at launch ([secrets.ReadContent]); cut into runs, it reached the box as
// the chart's own references strung together.

// Sealer turns a literal credential into a reference the record may carry.
//
// DEFINED BY THE CONSUMER, as every seam in this tree is: the chart needs one
// verb and it is not the secret store's whole surface. What satisfies
// it is [internal/fleetsecrets], wired by the engine.
type Sealer interface {
	// Seal stores value under name, which is the caller's — derived from the
	// object, the field and the operation ([SecretName]) — and returns
	// nothing but an error.
	//
	// IT CREATES AND NEVER REPLACES. Where name holds nothing it writes the
	// value; where name already holds exactly this value — the same write,
	// decided again — it succeeds and moves the row's version, so an orphan
	// sweep that judged the value before is refused its delete; and where
	// name holds a DIFFERENT value it answers [ErrSealTaken] and writes
	// nothing. See seal.go's header for what a replacing put cost.
	//
	// BY IS THE PARTY WRITING THE SEAT — the person who typed the
	// credential into it, and the credential they typed it through — which
	// the secret store records as the value's author. It used to record the
	// NODE the write happened to land on, so every credential a founder put
	// into a seat read as set by a machine nobody chose.
	Seal(ctx context.Context, name, value string, by secrets.Author) error
}

// ErrSealTaken is a [Sealer]'s answer to a seal whose name already holds a
// different value: the operation id the name is derived from was used for
// another change of the same field, and the value stored under it is left as
// it was.
var ErrSealTaken = errors.New("chart: the sealed name already holds a " +
	"different value")

// Runtime is where an object's runtime half keeps its credentials — the one
// thing this domain needs to know about a half it cannot read.
//
// CONSUMER-DEFINED and satisfied by internal/org, which owns the half's shape
// at both ends: it walks the encoded half against its own types with
// [secrets.Walk], so which values are credentials is the `secret:"true"` tag
// on those types and nothing written here.
type Runtime interface {
	// Credentials hands visit every credential in one object's runtime
	// half and answers the half with each answer in its place — the
	// half's own bytes where no answer differed.
	Credentials(kind ObjectKind, runtime json.RawMessage, visit secrets.Visit) (
		json.RawMessage, error)
}

// secretPrefix opens every name this domain seals under.
const secretPrefix = "CHART_"

// digestHex is how many hex digits of the digest end a derived name.
//
// FORTY BITS, because the readable half of a name is a FOLD — `a-b` and `a_b`,
// `token` and `TOKEN`, a unit key and a map key holding the separator all fold
// to one spelling — and it names no operation at all, so every write of one
// field shares it. The digest is over the unfolded identity, path and
// operation, and a collision needs the readable halves to collide as well: one
// field's next write lands on the name its row holds now with a chance of one
// in 2^40, and even then the seal is refused ([ErrSealTaken]) rather than
// written over it. Forty bits keep the name short enough to read in a
// listing.
const digestHex = 10

// SecretName is the name a sealed value is stored under: the object's
// IDENTITY and the path of the field inside it, folded into a variable name
// and ended with a digest of both unfolded and of the OPERATION that sealed
// it — see seal.go's header for why the operation.
//
// The shape is `CHART_<KIND>_<IDENTITY>_<PATH…>_<DIGEST>`. The readable half
// is for the operator listing their secrets; what makes a name belong to one
// write of one field of one object and nothing else is the digest.
func SecretName(object ObjectRef, opID string, path ...string) string {
	id := NormalizeKey(object.ID)
	sum := sha256.New()
	sum.Write([]byte(object.Kind))
	sum.Write([]byte{0})
	sum.Write([]byte(id))
	sum.Write([]byte{0})
	sum.Write([]byte(opID))
	for _, step := range path {
		sum.Write([]byte{0})
		sum.Write([]byte(step))
	}
	var b strings.Builder
	b.WriteString(secretPrefix)
	b.WriteString(varToken(string(object.Kind)))
	b.WriteByte('_')
	b.WriteString(varToken(id))
	for _, step := range path {
		b.WriteByte('_')
		b.WriteString(varToken(step))
	}
	b.WriteByte('_')
	b.WriteString(strings.ToUpper(hex.EncodeToString(sum.Sum(nil))[:digestHex]))
	return b.String()
}

// SecretRef is [SecretName] as the reference a record carries.
func SecretRef(object ObjectRef, opID string, path ...string) string {
	return "${" + SecretName(object, opID, path...) + "}"
}

// derivedName is the shape [SecretName] produces and nothing else can.
var derivedName = regexp.MustCompile(`^CHART_[A-Z0-9_]+_[0-9A-F]{` +
	strconv.Itoa(digestHex) + `}$`)

// OwnsSecret reports whether a secret name has the shape this domain derives —
// the names an engine re-reads when the chart's rows move and the ones the
// orphan sweep may collect. The SHAPE and not merely the prefix: an operator
// who names a credential of their own `CHART_API_KEY` has named one this
// domain never wrote.
func OwnsSecret(name string) bool { return derivedName.MatchString(name) }

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
// FOUR ANSWERS, and the middle two are why the function exists:
//
//   - A WHOLE `${VAR}` is stored VERBATIM. It names a credential rather than
//     being one, it is what an operator edits, and sealing it would put a
//     pointer inside the store and a pointer to that pointer on the record.
//   - AN EMPTY VALUE is stored verbatim too, and is a real setting: an
//     operator who deliberately cleared a credential has said something, and
//     sealing "" would store an empty secret and hand back a reference that
//     resolves to nothing.
//   - A LITERAL is sealed under the name this write derives for the field,
//     and the record carries the reference — so the value reaches the store
//     and never the log, the rows, a snapshot or a backup of any of them.
//   - A VALUE EMBEDDING REFERENCES has each LITERAL RUN sealed under a name of
//     its own and keeps its references where they were, so the record holds
//     references alone and expands to exactly what the value expanded to.
//
// CONTENT IS NEVER CUT: a value that is content rather than a setting
// ([secrets.TagContent] — a setup step's file) is sealed WHOLE, `${…}` and
// all, because the `${…}` inside a script or an .npmrc is the file's own
// syntax and the only reading it ever gets is [secrets.ReadContent]'s, which
// expands nothing inside a body. Cut into runs, a file reached the box as the
// chart's own references strung together.
//
// OBJECT names the field in a refusal; SEALAS is the identity the names are
// derived from, and OPID the write's operation.
func (w *Writer) sealValue(ctx context.Context, object, sealAs ObjectRef,
	opID string, path []string, content bool, value string) (string, error) {

	if value == "" {
		return "", nil
	}
	if _, whole := envref.Whole(value); whole {
		return value, nil
	}
	parts := envref.Split(value)
	if content {
		parts = []envref.Part{{Text: value}}
	}
	literals := 0
	for _, part := range parts {
		if part.Name == "" {
			literals++
		}
	}
	if literals == 0 {
		// NOTHING BUT REFERENCES — a value this writer sealed before, or
		// one an operator composed from their own variables. There is no
		// literal text in it to keep off the log.
		return value, nil
	}
	field := strings.Join(path, ".")
	if w.seal == nil {
		return "", fmt.Errorf("chart: %s on %s holds a literal credential and "+
			"this node has no secret store to seal it into — the value is "+
			"REFUSED rather than written to the log, which every node applies "+
			"and every snapshot copies. Configure the secret store, or give "+
			"the field a ${VAR} reference", field, object)
	}
	by := secrets.Author{Name: w.Actor, Kind: string(w.ActorKind), OperatorID: w.OperatorID}
	var out strings.Builder
	run := 0
	for _, part := range parts {
		if part.Name != "" {
			out.WriteString(part.Text)
			continue
		}
		// ONE NAME PER RUN, numbered in the value's own order: a pure
		// literal is the field's own name, and each run of a composite is
		// that name's own step, so the same write decided again derives
		// every name it sealed under and finds its own value there.
		at := path
		if len(parts) > 1 {
			run++
			at = append(slices.Clone(path), "#"+strconv.Itoa(run))
		}
		name := SecretName(sealAs, opID, at...)
		err := w.seal.Seal(ctx, name, part.Text, by)
		if errors.Is(err, ErrSealTaken) {
			return "", fmt.Errorf("chart: %s on %s: operation %q already sealed "+
				"a different value for this field — an operation id names one "+
				"write, and its retry must carry what the first attempt did. "+
				"Send this change under a new one: %w", field, object, opID,
				ErrRefused)
		}
		if err != nil {
			return "", fmt.Errorf("chart: seal %s on %s: %w", field, object, err)
		}
		out.WriteString("${" + name + "}")
	}
	return out.String(), nil
}

// sealRuntime seals every literal credential in an object's runtime half and
// answers the half carrying references in their place.
func (w *Writer) sealRuntime(ctx context.Context, object, sealAs ObjectRef,
	opID string, runtime json.RawMessage) (json.RawMessage, error) {

	if len(runtime) == 0 {
		return runtime, nil
	}
	sealed, err := w.runtime.Credentials(object.Kind, runtime,
		func(path secrets.Path, value string) (string, error) {
			return w.sealValue(ctx, object, sealAs, opID, runtimePath(path),
				path.Content(), value)
		})
	if err != nil {
		return nil, fmt.Errorf("chart: seal the runtime half of %s: %w", object, err)
	}
	return sealed, nil
}

// restoreRuntime is a stated runtime half with every mask in it replaced by the
// value the row's own half holds at the same place.
//
// THE MASK IS THE STORED VALUE HANDED BACK, exactly as it is for the address
// ([Writer.resolveMasked]): every surface that serves the runtime half masks
// its credentials, so GET-edit-PUT hands the marker back constantly, and a
// write that stored it would replace a working credential with twelve
// characters. It is restored VERBATIM — the row's value is already a
// reference — and matched by WHERE IT SITS, a list member by its identity, so
// a reorder never restores one member's credential into another.
//
// REFUSED, naming the field, where nothing can be restored: the row holds no
// value there, or the value sits in a list member with no identity of its own
// — found only by position, which is the one correspondence a reorder breaks.
// A row whose half does not decode holds nothing to restore from, so every
// mask over it is refused.
func (w *Writer) restoreRuntime(object ObjectRef, prior, stated json.RawMessage) (
	json.RawMessage, error) {

	held := map[string]string{}
	if len(prior) > 0 {
		if _, err := w.runtime.Credentials(object.Kind, prior,
			func(path secrets.Path, value string) (string, error) {
				held[path.Canonical()] = value
				return value, nil
			}); err != nil {
			held = map[string]string{}
		}
	}
	restored, err := w.runtime.Credentials(object.Kind, stated,
		func(path secrets.Path, value string) (string, error) {
			if value != redact.FieldMask {
				return value, nil
			}
			if path.Positional() {
				return "", fmt.Errorf("chart: runtime.%s on %s arrived as %q, and "+
					"it sits in a list member with no identity of its own — a "+
					"value restored by position after a reorder is another "+
					"member's credential. Send the value: %w",
					path, object, redact.FieldMask, ErrRefused)
			}
			was := held[path.Canonical()]
			if was == "" {
				return "", fmt.Errorf("chart: runtime.%s on %s arrived as %q and "+
					"there is no stored value to restore it from — storing the "+
					"marker would put it where a credential belongs, and "+
					"clearing the field would undo a value you did not mean to "+
					"touch. Send the value, or leave the field out: %w",
					path, object, redact.FieldMask, ErrRefused)
			}
			return was, nil
		})
	if err != nil {
		return nil, err
	}
	return restored, nil
}

// MaskRuntime is an object's runtime half as a surface may serve it: every
// credential in it shown only where it is a whole `${VAR}` reference
// ([secrets.Mask]).
//
// EXPORTED FOR THE SURFACES, because every read of the half — a chart read
// asked for the runtime, an export — must mask it exactly as a write restores
// it, and two spellings of either would be a credential served or a mask kept.
// A half that does not decode is served as nothing rather than as it is: what
// cannot be walked cannot be said to hold no credential.
func MaskRuntime(shape Runtime, kind ObjectKind, runtime json.RawMessage) json.RawMessage {
	if len(runtime) == 0 {
		return runtime
	}
	masked, err := shape.Credentials(kind, runtime,
		func(_ secrets.Path, value string) (string, error) {
			return secrets.Mask(value), nil
		})
	if err != nil {
		return nil
	}
	return masked
}

// runtimePath is a walked path as the steps a sealed name is derived from: a
// field's name, a map key, a list member's identity — and a member found only
// by position marked as one, so it never derives the name a member of that
// identity would.
func runtimePath(path secrets.Path) []string {
	out := make([]string, 0, len(path))
	for _, step := range path {
		if step.Positional {
			out = append(out, "["+step.Key+"]")
			continue
		}
		out = append(out, step.Key)
	}
	return out
}

// marshal is json.Marshal, named so the encode path reads as one step.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }
