package secrets

import (
	"reflect"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/redact"
)

// WHICH FIELDS OF A DOCUMENT HOLD A CREDENTIAL, decided in one place.
//
// # A tag on the field, and one reader of it
//
// A field that holds a credential says so where it is declared —
// `secret:"true"`, or `secret:"content"` for a body (below) — and everything
// that has to treat credentials differently reads that tag through [Field] and
// nothing else. A list of paths would be maintained by whoever remembered it
// existed, so the day somebody added `integrations.newthing.token` the surface
// serving the document would start publishing it and nothing would fail.
// internal/config is the reader: it MASKS the company document it serves and
// restores the masks a write hands back.
//
// # Two kinds of credential, and the tag is what tells them apart
//
// A SETTING — a token, a header, an environment value — is expanded where it
// is used, so a `${VAR}` inside one is the engine's own and `Bearer
// ${GITHUB_TOKEN}` sends the token. CONTENT — a file's body written into a
// sandbox box — is written where the engine does not run, so a `${…}` inside
// it belongs to whatever reads the file, and only a value that is exactly one
// whole reference is a pointer at all ([ReadContent]). So the difference is
// declared where the field is, `secret:"content"` ([TagContent]), rather than
// in a list of which credential fields are files kept by whichever package
// remembered.

// FieldTag is the struct tag that marks a field holding a credential. A field
// carrying it holds one, and so does everything beneath it — a map of them, a
// list of them, a block of them. It takes one of two values, [TagCredential]
// and [TagContent], and both are credentials to every reader: masked where a
// document is served.
const FieldTag = "secret"

// The two values [FieldTag] takes.
const (
	// TagCredential is a SETTING that holds a credential: a token, a key,
	// a header. `${VAR}` references anywhere inside one are the engine's
	// own, expanded where the value is used, so `Bearer ${GITHUB_TOKEN}`
	// sends the token.
	TagCredential = "true"

	// TagContent is a credential that is CONTENT — a file's body written
	// somewhere the engine does not run, a registry's auth file or a
	// helper script. What is inside one belongs to whatever reads the
	// content, so a `${…}` in it is THAT reader's syntax (a shell's, an
	// .npmrc's), never an engine reference: a value is a pointer only when
	// it is exactly one whole `${VAR}`, and anything else is the content,
	// byte for byte. See [ReadContent].
	TagContent = "content"
)

// Field reports whether a struct field is tagged as holding a credential, of
// either kind.
func Field(f reflect.StructField) bool {
	switch f.Tag.Get(FieldTag) {
	case TagCredential, TagContent:
		return true
	}
	return false
}

// ReadContent is a content credential's value as the place it is written to
// must receive it: a value that is exactly one whole `${VAR}` names the
// content kept elsewhere and reads as that variable's value, BYTE FOR BYTE and
// expanded no further; anything else IS the content, handed on as it is.
//
// # Why not the resolver's expansion
//
// Expanding every reference inside a file's body is what a setting gets, and
// it is exactly wrong for content: a helper script's own `${HOME}` would be
// substituted from the engine host's environment, and an .npmrc's
// `${NPM_TOKEN}` — which npm expands from the box's environment, where the
// token is declared — would be replaced by whatever the engine held under that
// name, or by nothing. And the value a whole reference names is content too,
// so it is not expanded either: a sealed body is exactly the body somebody
// wrote.
//
// Unresolved is the variable a whole reference named that lookup did not
// answer for, and empty otherwise; the value then reads as empty, as an
// unresolved reference does everywhere.
func ReadContent(value string, lookup func(name string) (string, bool)) (
	content, unresolved string) {

	name, whole := envref.Whole(value)
	if !whole {
		return value, ""
	}
	held, found := lookup(name)
	if !found {
		return "", name
	}
	return held, ""
}

// Mask is what a credential reads as on a surface that serves it.
//
// # Only a WHOLE reference is shown
//
// A value that is exactly one `${VAR}` names a credential and carries none:
// the engine resolves it where a provider is built, so nothing it points at is
// in the document to leak, and it is the half an operator edits.
//
// Anything else is masked, including a value that merely CONTAINS a reference:
// "Bearer sk-live-${SUFFIX}" is legitimate — the resolver expands embedded
// references — and its literal half is a credential. An empty value is shown
// as empty, because it is a real setting and the mask ([redact.FieldMask]) is
// a distinctive literal precisely so the two stay apart.
func Mask(value string) string {
	if value == "" {
		return value
	}
	if _, whole := envref.Whole(value); whole {
		return value
	}
	return redact.FieldMask
}
