package gitlet

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The domain's names for gitref's rules: a caller deciding a push asks the
// domain, while the grammar stays gitref's, one copy below both this package
// and the configuration that validates a declaration with it.

// ZeroOID is git's null object id: the old value of a create and the new value
// of a delete.
const ZeroOID = gitref.ZeroOID

// MaxRefNameBytes is the longest ref a push may create or move.
const MaxRefNameBytes = gitref.MaxRefNameBytes

// ValidName reports whether name is a repository name.
func ValidName(name string) bool { return gitref.ValidRepoName(name) }

// ValidRef reports whether ref is a ref a push may name.
func ValidRef(ref string) bool { return gitref.ValidRef(ref) }

// RecordedRef is the ref a command carries in a proposal, a record and a push
// row: ref itself when it is at most [MaxRefNameBytes] of valid UTF-8 (every
// ref a push may move, and every one refused for its shape alone), otherwise
// [digestForm] within MaxRefNameBytes, an invalid byte of the opening shown as
// U+FFFD; RecordedRef leaves that form as it is.
//
// EVERY COMMAND RIDES THE RECORD, a refused one too, so a push row is the whole
// answer its client got; git sends one for any ref its pkt-line holds (about
// 64 KiB of any bytes but NUL, refused bad_ref). Carried whole, JSON's escapes take a few such
// refs past the record's budget, refusing the branch pushed beside them, and
// invalid UTF-8 reads back as U+FFFD, making two refs one. Nothing reads a ref
// back out of the form: internal/gitlet/host answers git with the whole ref.
func RecordedRef(ref string) string {
	if len(ref) <= MaxRefNameBytes && utf8.ValidString(ref) {
		return ref
	}
	return digestForm(ref, strings.ToValidUTF8(ref, string(utf8.RuneError)), MaxRefNameBytes)
}

// FitProvenance is s as a [RunProvenance] field carries it: s itself when it
// is at most [MaxProvenanceBytes] of valid UTF-8 with no control character,
// otherwise [digestForm] at MaxProvenanceBytes, each invalid byte and control
// character of the opening shown as U+FFFD; fitting a fitted field changes
// nothing.
//
// FITTED, NEVER REFUSED: every site that stamps a run passes each field copied
// from outside the code domain (the work backend, the work key, the model)
// through it. Those are other systems' labels (a GitLab key may nest twenty
// levels of 255 bytes), and one past the bound would have
// [RunProvenance.Validate] refuse every write the seat makes on that item.
func FitProvenance(s string) string {
	if len(s) <= MaxProvenanceBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	// strings.Map hands each invalid byte over as utf8.RuneError, so it too
	// shows as U+FFFD.
	opening := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return utf8.RuneError
		}
		return r
	}, s)
	return digestForm(s, opening, MaxProvenanceBytes)
}

// digestForm is s within limit bytes of valid UTF-8, for a value too long or
// too strange to carry whole: `<opening>… [<n> bytes, sha256 <32 hex>]`, as
// much of opening (s as it may be shown) as fits beside s's length and a
// SHA-256 digest sixteen bytes wide ([NameToken]'s). The digest is of the
// WHOLE (textcut's rule for a length-limited identifier), so two values
// sharing their opening still differ. One form for a recorded ref and a
// fitted run field, so a reader learns one spelling of "cut, and this long".
func digestForm(s, opening string, limit int) string {
	sum := sha256.Sum256([]byte(s))
	tail := " [" + strconv.Itoa(len(s)) + " bytes, sha256 " + hex.EncodeToString(sum[:16]) + "]"
	return textcut.Within(opening, limit-len(tail)) + tail
}
