package collect

import (
	"errors"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact"
)

// Builder gathers one source's claims through the vocabulary's grammars, so a
// vendor's text reaches the atlas only as a typed value: a key or a value the
// grammar refuses, a credential-shaped one above all, is withheld and counted
// by where it sat, never by what it was. One source speaks in one voice, and
// every claim it makes is seen at the read's one instant.
type Builder struct {
	src    atlas.Source
	at     time.Time
	calls  int
	claims []atlas.Claim
	seen   map[slotKey]bool
	held   []derive.Withheld
	unread []Unread
}

// slotKey is one entry's one slot: a read holds each at most once.
type slotKey struct {
	entry atlas.Ref
	slot  string
}

// NewBuilder is a builder of the claims a read of kind makes at `at`.
func NewBuilder(kind integration.Kind, at time.Time) *Builder {
	return &Builder{src: atlas.IntegrationSource(string(kind)), at: at.UTC(), seen: map[slotKey]bool{}}
}

// errReference is a value that is a ${VAR} reference, which a vendor never
// returns and a read never resolves.
var errReference = errors.New("a ${VAR} reference, which a read does not resolve")

// Withhold records one value the read did not take: where it sat and why.
// THE LOCATOR IS REDACTED TOO: a source names a value's place by the vendor's
// own identifiers, and a vendor lets anybody write one, so a record id shaped
// like a credential would otherwise reach the standing through the one field
// meant to say where a refused value sat without saying what it was.
func (b *Builder) Withhold(where string, err error) {
	b.held = append(b.held, derive.Withheld{Where: redact.Secrets(where), Reason: err.Error()})
}

// Call counts one request made at the vendor.
func (b *Builder) Call() { b.calls++ }

// Calls counts n requests made at the vendor.
func (b *Builder) Calls(n int) { b.calls += n }

// add keeps one claim, or withholds it where the grammar refused it. A claim
// on a slot the read already holds is kept once: the first wins, so a vendor
// listing one thing twice reads as one thing.
func (b *Builder) add(where string, c atlas.Claim, err error) bool {
	if err == nil {
		err = c.Recordable()
	}
	if err != nil {
		b.Withhold(where, err)
		return false
	}
	k := slotKey{c.Subject, c.Slot()}
	if b.seen[k] {
		return true
	}
	b.seen[k] = true
	b.claims = append(b.claims, c)
	return true
}

// reference reports a ${VAR} reference, withheld as one: a vendor's answer
// never carries one, and a read resolves nothing.
func (b *Builder) reference(where, raw string) bool {
	if strings.Contains(raw, "${") {
		b.Withhold(where, errReference)
		return true
	}
	return false
}

// Names is the entry key names, with no claim that it exists: what a source
// says something about without holding it, as a monitor names the service it
// watches. False where the key is withheld. The key is held to the grammar as
// a recorded one ([atlas.RecordedRef]), so a key shaped like a credential is
// refused as it was written.
func (b *Builder) Names(where string, k atlas.Kind, key string) (atlas.Ref, bool) {
	if b.reference(where, key) {
		return atlas.Ref{}, false
	}
	r, err := atlas.RecordedRef(k, key)
	if err != nil {
		b.Withhold(where, err)
		return atlas.Ref{}, false
	}
	return r, true
}

// Ref is the entry key names and its exists claim, for an entry the vendor
// itself holds; false where the key is withheld.
func (b *Builder) Ref(where string, k atlas.Kind, key string) (atlas.Ref, bool) {
	r, ok := b.Names(where, k, key)
	if !ok {
		return atlas.Ref{}, false
	}
	c, err := atlas.Exists(r, b.src, b.at)
	return r, b.add(where, c, err)
}

// Says claims r's fact f holds raw, read through f's grammar. An empty raw
// says nothing.
func (b *Builder) Says(where string, r atlas.Ref, f atlas.Fact, raw string) {
	if raw = strings.TrimSpace(raw); raw == "" || b.reference(where, raw) {
		return
	}
	c, err := atlas.Says(r, f, raw, b.src, b.at)
	b.add(where, c, err)
}

// Relates claims from relies on to through rel.
func (b *Builder) Relates(where string, from atlas.Ref, rel atlas.Relation, to atlas.Ref) {
	c, err := atlas.Relates(from, rel, to, b.src, b.at)
	b.add(where, c, err)
}

// Unread records one part of the vendor the credential may not read.
func (b *Builder) Unread(u Unread) { b.unread = append(b.unread, u) }

// Read is what the builder gathered.
func (b *Builder) Read() Read {
	return Read{Claims: b.claims, Withheld: b.held, Calls: b.calls, Unread: b.unread}
}
