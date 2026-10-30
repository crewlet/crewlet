package proposal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/words"
)

// DigestDomain opens every proposal digest's preimage, so a proposal digest
// never equals any other digest of the same bytes. FIXED for the life of the
// format: a proposal recorded under one domain is re-hashed under the same one
// at its approval.
const DigestDomain = "crewlet.assistant.proposal.v1"

// PageMethod is the method a page action's request carries. A page action is
// applied by the screen and never sent, so its "request" is the action's name
// and payload in the request's shape, and the digest binds it like any other.
const PageMethod = "PAGE"

// PageActionPath is the path prefix a page action's request names its action
// under: `/page-actions/<action id>`.
const PageActionPath = "/page-actions/"

// Fields are every value a proposal's digest binds: who it is for, where it was
// made, what it sends and against which revision, and until when.
//
// EVERYTHING AN APPROVAL ACTS ON IS HERE. The request digest covers the method,
// the path, the query and the body; Act is the verb the exec grant is derived
// for; StepUp is whether the approval asks a fresh proof. A field the approval
// read from the row and the digest did not bind would be one a tampered row
// could change unseen (decision D24).
type Fields struct {
	Owner        uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID
	Proposal     uuid.UUID

	Scope wire.ProposalScope
	Tool  string
	Act   string

	RequestDigest  string
	SourceRevision string
	StepUp         bool
	ExpiresAt      time.Time
}

// ErrInvalid reports fields or a preview this package refuses, naming what to
// change.
var ErrInvalid = errors.New("proposal: invalid")

// check refuses fields no proposal is recorded with.
func (f Fields) check() error {
	switch {
	case f.Owner == uuid.Nil || f.Conversation == uuid.Nil || f.Turn == uuid.Nil ||
		f.Proposal == uuid.Nil:
		return fmt.Errorf("%w: a proposal names its owner, conversation, turn and id", ErrInvalid)
	case !f.Scope.Valid():
		return fmt.Errorf("%w: scope %q is not one of %s", ErrInvalid, f.Scope,
			words.Or(wire.ProposalScopes))
	case f.Tool == "":
		return fmt.Errorf("%w: a proposal names the tool that made it", ErrInvalid)
	case f.Scope == wire.ScopeServer && f.Act == "":
		return fmt.Errorf("%w: a server proposal names the verb it executes as", ErrInvalid)
	case !hexDigest(f.RequestDigest):
		return fmt.Errorf("%w: the request digest is 64 lowercase hex digits", ErrInvalid)
	case f.ExpiresAt.IsZero():
		return fmt.Errorf("%w: a proposal expires", ErrInvalid)
	}
	return nil
}

// Digest is the proposal's digest, as the wire carries it: "sha256:" and 64
// lowercase hex digits over [DigestDomain] and every field, each followed by a
// NUL. The expiry is bound at the second, in UTC, which is how the row stores
// it.
func Digest(f Fields) (string, error) {
	if err := f.check(); err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range []string{
		DigestDomain, f.Owner.String(), f.Conversation.String(), f.Turn.String(),
		f.Proposal.String(), string(f.Scope), f.Tool, f.Act, f.RequestDigest,
		f.SourceRevision, strconv.FormatBool(f.StepUp),
		f.ExpiresAt.UTC().Truncate(time.Second).Format(time.RFC3339),
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return wire.DigestPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// RequestDigest is [opkey.RequestDigestOf] of the request a proposal would
// send: the one encoding the request guard checks an exec grant's request
// against, so what the person approved and what the guard admits are compared
// in the same bytes.
func RequestDigest(r exec.Request) (string, error) {
	u, err := r.URL()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return opkey.RequestDigestOf(r.Method, u, r.Body)
}

// FieldsOf are the fields a stored proposal's digest is re-hashed from.
func FieldsOf(p assistantdomain.Proposal) Fields {
	return Fields{
		Owner: p.Owner, Conversation: p.Conversation, Turn: p.Turn, Proposal: p.ID,
		Scope: p.Scope, Tool: p.Tool, Act: p.Act, RequestDigest: p.RequestDigest,
		SourceRevision: p.SourceRevision, StepUp: p.StepUp, ExpiresAt: p.ExpiresAt,
	}
}

// RequestOf is the request a stored proposal sends, under its own operation key
// and the precondition its source revision names ([Revision.Precondition]):
// an entity-tagged revision's tag as its If-Match, a create's
// `If-None-Match: *`.
//
// THE PRECONDITION IS DERIVED, NEVER STORED BESIDE IT: the source revision is
// a field the proposal's digest binds, so the precondition an approval sends
// is the one the person was shown (an edit of the version they saw, or a
// create of what they saw absent), and a row whose revision was changed fails
// [Verify] before anything is sent.
func RequestOf(p assistantdomain.Proposal) exec.Request {
	req := exec.Request{Method: p.Method, Path: p.Path, Query: p.Query, Body: p.Body,
		IdempotencyKey: p.OpKey}
	if r, err := ParseRevision(p.SourceRevision); err == nil {
		req.IfMatch, req.IfNoneMatch = r.Precondition()
	}
	return req
}

// ErrDigestMismatch reports an approval whose digest is not the proposal's, or
// a stored proposal whose fields no longer hash to the digest it was recorded
// with. Nothing is sent for either.
var ErrDigestMismatch = errors.New("proposal: the digest does not match the proposal")

// Verify reports whether echoed is the digest the person was shown AND the
// stored proposal still hashes to it: its request re-encoded to the request
// digest it was recorded with, and every field to its digest. Compared in
// constant time.
//
// BOTH HALVES, because each catches what the other cannot: an echo that is not
// the stored digest is an approval of something else (another card, a forged
// post), and a stored row that no longer re-hashes is a proposal whose request
// changed after the person was shown it.
func Verify(p assistantdomain.Proposal, echoed string) error {
	if !wire.ValidDigest(echoed) || !equal(echoed, p.Digest) {
		return fmt.Errorf("%w: the approval echoes a digest this proposal was not shown with",
			ErrDigestMismatch)
	}
	request, err := RequestDigest(RequestOf(p))
	if err != nil || !equal(request, p.RequestDigest) {
		return fmt.Errorf("%w: the stored request no longer encodes to the request "+
			"digest it was recorded with", ErrDigestMismatch)
	}
	digest, err := Digest(FieldsOf(p))
	if err != nil || !equal(digest, p.Digest) {
		return fmt.Errorf("%w: the stored proposal no longer hashes to its digest",
			ErrDigestMismatch)
	}
	return nil
}

// equal compares two digests in constant time.
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// hexDigest reports a SHA-256 in lowercase hex.
func hexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// PageAction is the `action` frame that hands a page action to the page: its
// id is the proposal's, so the page's claim and report name the proposal, and
// it carries the action's name, the revision it was prepared against and the
// payload the digest binds. confirm says whether the person approved it
// (approve) or the screen declared it applies at once with an Undo (none).
func PageAction(id uuid.UUID, path, revision string, payload []byte,
	confirm wire.ActionConfirm) wire.ActionEvent {

	return wire.ActionEvent{ActionID: id, Name: strings.TrimPrefix(path, PageActionPath),
		ProposalID: &id, SourceRevision: revision, Payload: payload, Confirm: confirm}
}
