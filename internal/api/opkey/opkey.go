// Package opkey is the HTTP half of an operation id: the header a caller
// sends one in, the rule a key it sends is held to, and the digest that binds
// the id a write is published under to the request that asked for it.
//
// # Why one package, and not a copy per surface
//
// Three surfaces take a key — `/chart`, `/iam` and the human write surface
// (`/work`, `/pages`) — and a script retrying a write on this engine is one
// script whichever of them it wrote to: an `unknown` answer carries the key,
// and the route accepts it back under the same header, held to the same
// grammar and refused with the same code. Each surface carried its own copy of
// all three, and of the request digest `/chart` and `/iam` bind a key to, and
// the copies said in their comments that they matched one another, which
// nothing checked. A rule a retry depends on is one rule.
//
// # What it is not
//
// It decides nothing about WHICH id a write is published under beyond the
// binding: that is each surface's — `/chart` and `/iam` step the key by the
// verb and [Digest] ([statelog.StepOpID]); the human write surface derives
// one id per write it makes from the key, the verb, the object and the
// arguments — because what a write is about is the surface's own vocabulary.
package opkey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Header carries a caller's operation key: on a retry, the `op_id` an earlier
// answer handed back.
const Header = "Idempotency-Key"

// Key is the request's operation key: the caller's own where they sent one,
// held to [statelog.CheckCallerOpID], and one minted at now where they did
// not — which the answer then hands back, so a retry can send it. It answers
// false once it has written the refusal ([Refuse]).
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
// write the key was sent to prevent.
//
// BARE, with no name: the key is what a caller holds, and every id a write is
// published under is derived from it and named by the surface.
func Key(w http.ResponseWriter, r *http.Request, now time.Time) (string, bool) {
	given := strings.TrimSpace(r.Header.Get(Header))
	if given == "" {
		return statelog.NewOpID(now, ""), true
	}
	if err := statelog.CheckCallerOpID(given); err != nil {
		Refuse(w, err)
		return "", false
	}
	return given, true
}

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
