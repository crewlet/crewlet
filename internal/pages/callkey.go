package pages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CallKey is the caller's own idempotency key for one write: the key of the
// turn that made it, or the request id a person's write carried, or empty.
//
// # Why a write takes one
//
// A write whose answer never arrived is sent again — a redelivered turn runs
// again, a person whose save answered `unknown` presses retry — and an
// operation id minted fresh on every call makes that second send a second
// record under new identities. Derived from this key instead
// ([Store.callOpID]), the repetition is the SAME operation carrying the same
// ids: whatever keys on an operation — the broker's duplicate window, the
// operation ledger — sees one operation rather than two, and a create's second
// copy lands on the address and the page id its first attempt took.
//
// A TYPE RATHER THAN A STRING because two of the writes that take it already
// take three strings in a row, and a key passed where a body was meant is a
// write that compiles and derives nothing.
//
// EMPTY IS A REAL VALUE: a call that names no key — an operator's assistant
// over MCP, an import — writes under a fresh operation every time, because
// nothing will repeat it and a stable id would collapse every later write into
// the first.
//
// NOT ATTRIBUTION, which is why it is not a field of [Actor]: nothing about it
// is written to a history row, and an actor is the record of who wrote.
type CallKey string

// String is the key as text.
func (k CallKey) String() string { return strings.TrimSpace(string(k)) }

func (k CallKey) empty() bool { return k.String() == "" }

// callOpID is the operation one keyed write appends under.
//
// OVER THE KEY, THE VERB AND WHAT THE CALLER ASKED FOR — never over anything
// read from the rows. A retry must derive the same id, and a retry arrives
// AFTER its first attempt may have moved the page: a revision read at call
// time would differ between a request and its own retry and collapse nothing.
// One key legitimately writes one page more than once (a turn saves, then
// saves again from the version it got back), so the parts name what makes each
// of those a different operation: a save's base version and content, a
// rename's target title, an edit's text.
func (s *Store) callOpID(key CallKey, verb string, parts ...string) string {
	if key.empty() {
		return s.newSeqID()
	}
	name := verb + "\x00" + key.String() + "\x00" + strings.Join(parts, "\x00")
	return uuid.NewSHA1(operationNamespace, []byte(name)).String()
}

// pageIDFor is a new page's id: derived from the key where there is one, so a
// create the broker collapsed into its first attempt answers with the page
// that attempt made rather than an id nothing holds.
//
// OVER THE CONTAINER AND THE NORMALISED TITLE, which is the address the create
// arbitrates on: two creates of one address under one key are a repetition,
// and two different addresses are two pages.
func (s *Store) pageIDFor(key CallKey, container, title string) string {
	if key.empty() {
		return s.newID()
	}
	name := key.String() + "\x00" + container + "\x00" + NormalizeTitle(title)
	return uuid.NewSHA1(pageNamespace, []byte(name)).String()
}

// textKey is a digest of a text a caller sent, for an operation whose object
// is that text.
func textKey(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}

// saveKey is a digest of everything a save asked for, so two different saves
// from one base version under one key are two operations.
func saveKey(save Save) string {
	save.CallKey = ""
	raw, err := json.Marshal(save)
	if err != nil {
		// Every field of a Save encodes; formatting is the deterministic
		// fallback rather than a reason to refuse a write.
		raw = []byte(fmt.Sprintf("%#v", save))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// operationNamespace and pageNamespace are the uuid namespaces derived
// operation and page ids live under. FIXED for the life of the format: a page
// id is durable on every node, and a new namespace would make every repeated
// write after the change a second record.
var (
	operationNamespace = uuid.MustParse("5b8e1d3a-2c4f-5a6b-8d9e-1f2a3b4c5d6e")
	pageNamespace      = uuid.MustParse("7e2a4c6b-1d3f-5b8a-9c0e-3a5b7d9f1c2e")
)
