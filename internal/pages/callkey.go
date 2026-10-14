package pages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE KEY A KEYED WRITE IS DERIVED FROM is [statelog.CallKey], shared with
// chat, and its doc carries the reasons for each field. What is the wiki's
// own is below: the namespaces its ids live under, and the parts each write
// derives over — the WIKI LEAVES THE KEY'S TOOL EMPTY, because no two of its
// tools make one gesture, so its ids are derived over the seed, the parts, the
// arguments and the count alone.
//
// # Why its count needs the arguments, in the wiki's own terms
//
// comment_on_page names its page either way — by id or by
// "CONTAINER/Title" — so two calls typed differently can resolve to the same
// page. A seat that commented "ok" by the page's id, asked that again,
// commented "ok" by its title, "other" by its id and then "ok" by its id once
// more gave the last call a count of two — the title call and "other", the
// two by id being itself — which is exactly the title call's count. Without
// the digest of the arguments in the identity the last call derived the title
// comment's id, was answered with that comment's success, and wrote nothing,
// though it was the same words said again after a different remark.

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
//
// UNNAMED, like a fresh one ([Store.newSeqID]): the id is also the change id a
// write answers with, and the page and the write it names are on the row.
func (s *Store) callOpID(key statelog.CallKey, verb string, parts ...string) string {
	if key.Empty() {
		return s.newSeqID()
	}
	return key.DeriveOpID(operationNamespace, append([]string{verb}, parts...)...)
}

// pageIDFor is a new page's id: derived from the key where there is one, so a
// create the broker collapsed into its first attempt answers with the page
// that attempt made rather than an id nothing holds.
//
// OVER THE CONTAINER AND THE NORMALISED TITLE, which is the address the create
// arbitrates on: two creates of one address under one key are a repetition,
// and two different addresses are two pages. AND THE REPEAT, because a create
// made again after a different one is a second operation, and a second
// operation re-using the first's page id would address a page a purge may
// already have gated.
func (s *Store) pageIDFor(key statelog.CallKey, container, title string) string {
	if key.Empty() {
		return s.newID()
	}
	name := strings.Join(key.Identity(pageIDNamespace, container,
		NormalizeTitle(title)), "\x00")
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
	save.CallKey = statelog.CallKey{}
	raw, err := json.Marshal(save)
	if err != nil {
		// Every field of a Save encodes; formatting is the deterministic
		// fallback rather than a reason to refuse a write.
		raw = []byte(fmt.Sprintf("%#v", save))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// operationNamespace and pageIDNamespace keep a derived id from colliding with
// one another package derives from the same seed — a turn's tracker writes are
// derived from the same work key — and pageNamespace is the uuid namespace a
// derived page id lives under. FIXED for the life of the format: a page id is
// durable on every node, and a new namespace would make every repeated write
// straddling the change a second record.
const (
	operationNamespace = "crewlet.pages.operation"
	pageIDNamespace    = "crewlet.pages.page"
)

var pageNamespace = uuid.MustParse("7e2a4c6b-1d3f-5b8a-9c0e-3a5b7d9f1c2e")
