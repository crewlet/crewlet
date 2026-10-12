package pages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// CallKey is the caller's own idempotency key for one write: the unit of work
// that made it — the turn, or the request a person's write carried — when that
// unit began, and which of its calls this is. Or the zero value, for a call
// nothing will repeat.
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
// # Why it carries an instant
//
// Every operation id carries the instant it was minted ([statelog.DeriveOpID]),
// and that instant is what the state log reads to decide whether this node's
// operation ledger can vouch for a retry: a ledger that lost the first
// attempt's row to its retention sweep answers an operation minted before the
// loss `unknown` rather than deciding it again on rows that already hold it. So
// [CallKey.Since] is the UNIT OF WORK's instant, reproduced by every
// repetition, and never the call's: a re-run's clock is after the loss by
// construction, and an id carrying it would be decided and published twice.
//
// # Why it counts repeats
//
// The key and the call's arguments say WHICH write a call is, not WHEN it
// happened. A run that renamed a page to "Runbook", then to "Old runbook",
// then back to "Runbook" would derive one operation for the first and the
// third, and the third would be answered as the first's retry — applied, at
// the first's position — while the page stayed "Old runbook". [CallKey.Repeat]
// is how many earlier calls to the same tool in the run asked for something
// else, and it is part of every derived id, so the third call is an operation
// of its own while a call repeated with nothing different in between — an
// executor asking twice, a retry after `unknown`, a re-run — stays one.
//
// # Why it carries the call's own arguments
//
// Because the count and the identity have to agree on what makes two calls
// different, and on their own they do not. [CallKey.Repeat] tells calls apart
// by their ARGUMENTS, as a model typed them, and skips every earlier call that
// was this same call; the parts a derived id is computed over are what those
// arguments RESOLVED to — the page's id, never the "ENG/Runbook" or the uuid
// it was named by. comment_on_page names its page either way, so two calls
// typed differently can resolve alike, and a count two of them happen to share
// hands the later one the earlier one's id. A seat that commented "ok" by the
// page's id, asked that again, commented "ok" by its title, "other" by its id
// and then "ok" by its id once more gave the last call a count of two — the
// title call and "other", the two by id being itself — which is exactly the
// title call's count. So the last call derived the title comment's id, was
// answered with that comment's success, and wrote nothing, though it was the
// same words said again after a different remark.
//
// With a digest of the arguments in the identity ([CallKey.Args]) two calls
// derive one id only where they share the arguments and the count, and the
// call log gives a repeated call its earlier count only when no different call
// to its tool came between: which is exactly a call made again, and nothing
// else. A tracker write's id carries the digest too (builtin's opIDFor). The
// parts still follow it, because a key that carries none — a request's, whose
// operation already names the call — is distinguished by them alone.
//
// A STRUCT RATHER THAN A STRING because two of the writes that take it already
// take three strings in a row, and a key passed where a body was meant is a
// write that compiles and derives nothing; and because its values are one
// identity, which a caller could otherwise hand over part of.
//
// THE ZERO VALUE IS A REAL ONE: a call that names no seed — an operator's
// assistant over MCP, an import — writes under a fresh operation every time,
// because nothing will repeat it and a stable id would collapse every later
// write into the first.
//
// NOT ATTRIBUTION, which is why it is not a field of [Actor]: nothing about it
// is written to a history row, and an actor is the record of who wrote.
type CallKey struct {
	// Seed names the unit of work: a turn's work key or run id, or a
	// person's request key. Empty is a call nothing will repeat.
	Seed string

	// Since is when the unit of work Seed names began — a turn's earliest
	// trigger, a request's own mint instant — and the instant every id
	// derived from this key carries. It must be the seed's own, reproduced
	// by every repetition. A zero Since is read as minted before every loss
	// the ledger has had, so where this node's ledger has lost rows a keyed
	// write answers `unknown` rather than publishing: the caller that could
	// not say when its work began pays for it, never with a duplicate.
	Since time.Time

	// Repeat is how many earlier calls to the same tool in the seed's run
	// asked for something else — see turnctx.CallLog. Zero adds nothing to
	// a derived id, so a run's first call of each kind derives the id it
	// always did.
	Repeat int

	// Args is a digest of the arguments the call was made with
	// (turnctx.ArgsDigest), for a key whose Repeat a call log counted — a
	// turn's. Empty for a key whose seed already names the one call it was
	// derived for, which is a request's operation. See the type's doc for
	// why the count needs it beside it.
	Args string
}

// String is the seed as text.
func (k CallKey) String() string { return strings.TrimSpace(k.Seed) }

func (k CallKey) empty() bool { return k.String() == "" }

// identity is what a derived id is computed over: a namespace, the seed, the
// parts that make this operation this operation, the call's own arguments
// where the key carries them, and the repeat count when there is one.
//
// ONE SPELLING for every derivation here — an operation's, a comment's, a new
// page's — so the three can never disagree about what makes two calls one.
func (k CallKey) identity(namespace string, parts ...string) []string {
	out := make([]string, 0, len(parts)+4)
	out = append(out, namespace, k.String())
	out = append(out, parts...)
	if k.Args != "" {
		out = append(out, "args:"+k.Args)
	}
	if k.Repeat > 0 {
		out = append(out, "repeat:"+strconv.Itoa(k.Repeat))
	}
	return out
}

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
func (s *Store) callOpID(key CallKey, verb string, parts ...string) string {
	if key.empty() {
		return s.newSeqID()
	}
	return statelog.DeriveOpID(key.Since, "",
		key.identity(operationNamespace, append([]string{verb}, parts...)...)...)
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
func (s *Store) pageIDFor(key CallKey, container, title string) string {
	if key.empty() {
		return s.newID()
	}
	name := strings.Join(key.identity(pageIDNamespace, container,
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
	save.CallKey = CallKey{}
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
