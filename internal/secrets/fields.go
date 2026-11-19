package secrets

import (
	"reflect"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/redact"
)

// WHICH FIELDS OF A DOCUMENT HOLD A CREDENTIAL, decided in one place.
//
// A credential field says so where declared (`secret:"true"`, or
// `secret:"content"` for a body) and everything reads that tag through [Field]
// and nothing else: a list of paths would be kept by whoever remembered it, so a
// new `integrations.newthing.token` would be published.
// A SETTING (token, header, environment value) is expanded where used, so
// `Bearer ${GITHUB_TOKEN}` sends the token. CONTENT (a file's body written into a
// sandbox box) is written where the engine does not run, so a `${…}` inside it
// belongs to whatever reads the file; only a value that is exactly one whole
// reference is a pointer ([ReadContent]). `secret:"content"` ([TagContent])
// declares which.

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
// must receive it: a value that is exactly one whole `${VAR}` names the content
// kept elsewhere and reads as that variable's value, BYTE FOR BYTE and expanded
// no further; anything else IS the content, handed on as it is.
//
// Not the resolver's expansion, which is exactly wrong for content: a helper
// script's own `${HOME}` would be substituted from the engine host, and an
// .npmrc's `${NPM_TOKEN}`, which npm expands from the box's environment, would
// be replaced by whatever the engine held under that name, or by nothing.
//
// Unresolved is the variable a whole reference named that lookup did not answer
// for, and empty otherwise; the value then reads as empty.
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
// Only a WHOLE reference is shown: a value that is exactly one `${VAR}` names a
// credential and carries none, and is the half an operator edits. Anything else
// is masked, including a value that merely CONTAINS a reference ("Bearer
// sk-live-${SUFFIX}": its literal half is a credential). An empty value is shown
// as empty, a real setting, which is why the mask ([redact.FieldMask]) is a
// distinctive literal.
func Mask(value string) string {
	if value == "" {
		return value
	}
	if _, whole := envref.Whole(value); whole {
		return value
	}
	return redact.FieldMask
}
