// Package opkey is the HTTP half of an operation id: the header a caller
// sends one in, the rule a key it sends is held to, the digest that binds the
// id a write is published under to the request that asked for it, and what an
// unknown outcome tells the caller to retry with.
//
// # Why one package, and not a copy per surface
//
// Two surfaces take a key — `/iam` and the human write surface (`/work`,
// `/pages`) — and a script retrying a write on this engine is one
// script whichever of them it wrote to: an `unknown` answer carries the key,
// and the route accepts it back under the same header, held to the same
// grammar and refused with the same code. Each surface carried its own copy of
// all three, and of the request digest `/iam` binds a key to, and the copies
// said in their comments that they matched one another, which nothing
// checked. A rule a retry depends on is one rule.
//
// # What it is not
//
// It decides nothing about WHICH id a write is published under beyond the
// binding: that is each surface's — `/iam` steps the key by the verb and
// [Digest] ([statelog.StepOpID]); the human write surface and the
// operator surface's act route derive one id per write they make from the
// key, the verb, the object and the arguments — because what a write is about
// is the surface's own vocabulary.
//
// # A key is the caller's own, and nobody else's
//
// Every key [Key] answers is SCOPED BY THE PRINCIPAL the request resolved to
// ([iam.Principal.ID]) — see [scope]. Unscoped, a key named an operation for
// whoever sent it first: the ledger answers an operation it already holds
// before the write is decided, so a party who learned somebody else's key — a
// log line, a shared terminal, a client that printed it — could send it with
// their own request and have the victim's write answered as theirs, or send
// it FIRST and have the victim's real write answered from the ledger with
// nothing of it written. The scope is the principal's ID and never its login,
// a token's label or a session's lineage: a rename moves the first, a
// rotation the second and a step-up the third, and each would have turned one
// person's retry into a second write.
package opkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Header carries a caller's operation key: on a retry, the `op_id` an earlier
// answer handed back.
const Header = "Idempotency-Key"

// Key is the request's operation key: the caller's own where they sent one,
// held to [statelog.CheckCallerOpID], and one minted at now where they did
// not — either way SCOPED by the request's principal ([scope]) — and the
// answer then hands it back, so a retry can send it. It answers false once
// it has written the refusal ([Refuse]).
//
// # An operation id the engine's grammar minted, both ways
//
// The publisher vouches for a retry by the INSTANT its operation id carries
// ([statelog.OpMintedAt]), against the point the node's operation ledger may
// have lost rows from. A key outside the grammar carries none and reads as
// minted at the epoch, before every loss the ledger will ever have: once it
// had swept anything, every such write was answered `unknown` without being
// published, on the first attempt and on every retry. So a fresh key is minted
// through [statelog.NewOpID], and a caller's is refused naming the rule rather
// than ignored: ignoring it published under a fresh id, which is the double
// write the key was sent to prevent. The scoped key keeps the instant of the
// key it was scoped from, so the ledger vouches for it exactly as long.
//
// BARE OF ANY VERB: the key is what a caller holds, and every id a write is
// published under is derived from it and named by the surface. Its one name
// is the scope's tag, which is what lets a retry send it back unchanged.
func Key(w http.ResponseWriter, r *http.Request, now time.Time) (string, bool) {
	given := strings.TrimSpace(r.Header.Get(Header))
	if given == "" {
		return scope(r.Context(), statelog.NewOpID(now, "")), true
	}
	return scoped(w, r, given)
}

// Require is [Key] for a route on which the caller MUST name the operation:
// an absent header is refused `400 op_id_invalid` naming it, exactly as a
// malformed one is, rather than minted here.
//
// FOR A ROUTE WHOSE CALLER RETRIES BY ITSELF — the dashboard's act route,
// which mints its key once per gesture and sends it again unchanged when an
// answer never came. A key minted here would be handed back in an answer that
// is, in the one case a retry exists for, never read: the request that lost
// its answer would be retried under a fresh key and made twice.
func Require(w http.ResponseWriter, r *http.Request) (string, bool) {
	given := strings.TrimSpace(r.Header.Get(Header))
	if given == "" {
		Refuse(w, errNoKey)
		return "", false
	}
	return scoped(w, r, given)
}

// errNoKey is [Require]'s refusal of a request that named no operation.
var errNoKey = errors.New("this route takes the operation from the " + Header +
	" header, and the request carried none: mint one operation id when the " +
	"gesture begins and send it, unchanged, on every retry of it")

// scoped holds a caller's key to the grammar and scopes it.
func scoped(w http.ResponseWriter, r *http.Request, given string) (string, bool) {
	if err := statelog.CheckCallerOpID(given); err != nil {
		Refuse(w, err)
		return "", false
	}
	return scope(r.Context(), given), true
}

// scope is key as the operation of the principal on ctx: derived from the
// principal's ID and the key, at the key's own instant, and NAMED by a tag of
// that ID — unless key already carries the tag, which is this principal's
// retry of a key an earlier answer handed them.
//
// # Why the tag, and why it is safe to trust
//
// The answer hands the SCOPED key back, and a retry sends it as the header:
// scoped again, it would be another operation, and the retry a second write.
// So a key already bearing this principal's tag is taken as it is. Nothing is
// trusted that matters: the tag is compared with the REQUESTER's own, so a key
// bearing anybody else's — sent by somebody who copied it — is scoped again
// under the sender, and names an operation of theirs that no write of the
// key's owner ever had. Forging a key bearing your own tag reaches only your
// own operations.
//
// A REQUEST NOBODY RESOLVED is scoped by the nil ID, which no principal holds.
// It does not reach here behind the request guard, and a write with no
// principal is refused by every surface before it is made; the scope only has
// to keep such a key apart from every real caller's.
func scope(ctx context.Context, key string) string {
	id := uuid.Nil
	if p, how := iam.From(ctx); how == iam.Resolved {
		id = p.ID
	}
	tag := tagOf(id)
	if strings.HasSuffix(key, "."+tag) && strings.Count(key, ".") == 1 {
		return key
	}
	at, _ := statelog.OpMintedAt(key)
	return statelog.DeriveOpID(at, tag, scopeNamespace, id.String(), key)
}

// tagOf is the name a principal's scoped keys carry: `k` and sixteen hex
// digits of a digest of the ID — 64 bits, which tells two principals apart
// with certainty and keeps the key inside [statelog.MaxCallerOpIDBytes] with
// room for every step a surface appends. Not a credential: it is derived from
// an ID that is no secret, and [scope] compares it with the requester's own.
func tagOf(id uuid.UUID) string {
	sum := sha256.Sum256([]byte(scopeNamespace + "\x00" + id.String()))
	return "k" + hex.EncodeToString(sum[:])[:tagDigits]
}

// tagDigits is how many hex digits of the ID's digest a scoped key's tag
// carries — see [tagOf].
const tagDigits = 16

// scopeNamespace keeps a scoped key's derivation apart from every other one
// over the same key. FIXED for the life of the format: a new one would make a
// retry that straddles the change a second write.
const scopeNamespace = "crewlet.opkey.scope"

// Refuse answers a key the surface cannot publish under: `400
// op_id_invalid`, naming the header, with err as the rule it broke, and
// nothing read or written.
//
// [httpjson.CodeOpIDInvalid], THE ONE CODE EVERY SURFACE REFUSES A CALLER'S
// OPERATION ID WITH — the node gate's `?op_id=` included — so a client
// branches on one spelling for "the id you sent is not one this engine would
// have minted" whichever route it sent it to. The field names the header,
// because a key is sent there rather than in the body or the query.
func Refuse(w http.ResponseWriter, err error) {
	httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeOpIDInvalid,
		httpjson.Detail{"field": Header, "detail": err.Error()})
}

// UnknownDetail is what the 503 of an UNKNOWN outcome says to do: retry with
// the same operation id, sent back as [Header] — or, where this node's
// operation ledger cannot vouch for the operation (unvouched), send it with
// that id through another node, since asked here it answers the same way until
// the change arrives. Never under a fresh key, which is a second change if the
// first landed.
//
// THE RETRY IS PART OF THE KEY'S RULE, so it is said here rather than by each
// surface: /iam and the human write surface each wrote both sentences
// for themselves, and the copies had drifted — "the SAME operation id" beside
// "the SAME key", and one surface putting the unvouched sentence after the
// ordinary one as though both were true. The field that tells an unknown from
// a refusal is [httpjson.UnknownOutcome]'s; this is only the sentence beside
// it.
func UnknownDetail(unvouched bool) string {
	if unvouched {
		return "this node's operation ledger cannot vouch for this change, so " +
			"the same request asked here answers the same way until the change " +
			"reaches this node. Read whether it landed, or send it with the SAME " +
			Header + " to another node; never under a fresh one, which is a " +
			"second change if the first one landed."
	}
	return "this node cannot establish what happened to this change. Retry it " +
		"with the SAME operation id — send op_id back as the " + Header +
		" header — because a fresh one would defeat the ledger that makes the " +
		"retry safe."
}

// Digest is what binds a write's operation to its request: the path (the
// object), the query and the decoded body, in a form one request always
// produces — the body re-encoded from its decoded value, so the same fields
// in another order or spacing are the same request. asks is the decoded
// body, nil for a route that takes none.
//
// # Why a key alone is not the operation
//
// The ledger answers an operation it already holds BEFORE the write is
// decided ([statelog.Result.Collapsed]), so a write published under the key
// itself made the same key sent with ANOTHER request the first request's
// operation: answered `applied` from the ledger, with nothing of the second
// written — a change reported as made and silently dropped. Stepped by this
// digest, the same request is the same operation however often it is sent,
// and any other request under the key is an operation of its own.
//
// SIXTEEN HEX DIGITS, 64 bits of SHA-256: it tells an accidental change of
// request apart with certainty, which is all it is for — a caller can mint any
// key it likes, so this is not a credential — in an id that is published in
// every record and ledger row the write makes. An error is a body that decoded
// and will not encode: a type the surface declared wrongly, never the
// caller's to fix.
func Digest(r *http.Request, asks any) (string, error) {
	body, err := json.Marshal(asks)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(r.URL.Path),
		[]byte(r.URL.Query().Encode()), body} {
		_, _ = h.Write(part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:digestLen], nil
}

// digestLen is how many hex digits of a request's digest its operation
// carries — see [Digest].
const digestLen = 16
