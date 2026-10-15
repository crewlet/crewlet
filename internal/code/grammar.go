package code

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The domain's names for gitref's rules, so a caller deciding a push asks the
// domain — while the grammar itself is gitref's, one copy below both this
// package and the configuration that validates a declaration with it.

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
// row: ref itself when it is at most [MaxRefNameBytes] of valid UTF-8 — every
// ref a push may move, and every one refused for its shape alone — and
// otherwise as much of its opening as fits beside its length and a digest of
// the whole, within MaxRefNameBytes: `<opening>… [<n> bytes, sha256 <32 hex>]`,
// an invalid byte of the opening shown as U+FFFD.
//
// EVERY COMMAND RIDES THE RECORD, a refused one too, so a push row is the
// whole answer its client was given — and git sends a command for any ref its
// pkt-line holds, up to about 64 KiB of any bytes but NUL, which the receiver
// refuses bad_ref. Carried whole, such a ref took up to six times its bytes in
// the record's JSON (`&` is spelled as a six-byte escape), so three of them
// beside a branch took the push's entry past its record's budget and the
// committer refused the receiver's own proposal — the branch refused with
// them, the bundle stored for nothing; and below that budget each was
// replicated into every data node's push history for 181 days, as a ref
// nothing can ever name. Invalid
// UTF-8 does not survive JSON at all: each such byte is read back as U+FFFD,
// so two refs differing only there became one, and a proposal holding both
// was refused as naming one ref twice.
//
// THE DIGEST IS OF THE WHOLE, so two refs sharing their opening still differ —
// textcut's rule for an identifier with a length limit — and sixteen bytes of
// SHA-256 wide, [NameToken]'s. The form is itself at most MaxRefNameBytes of
// valid UTF-8, so RecordedRef of it is itself. Nothing reads a ref back out of
// the form: the receiver answers git with the ref it holds whole
// (internal/code/host), and the form is what a person reading the push's
// history sees of a ref the engine refused.
func RecordedRef(ref string) string {
	if len(ref) <= MaxRefNameBytes && utf8.ValidString(ref) {
		return ref
	}
	sum := sha256.Sum256([]byte(ref))
	tail := " [" + strconv.Itoa(len(ref)) + " bytes, sha256 " + hex.EncodeToString(sum[:16]) + "]"
	opening := strings.ToValidUTF8(ref, string(utf8.RuneError))
	return textcut.Within(opening, MaxRefNameBytes-len(tail)) + tail
}
