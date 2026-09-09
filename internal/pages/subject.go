package pages

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The knowledge base as a state-log DOMAIN: what a record is, and what the
// subject is for.
//
// Every change is one RECORD on one ordered stream, published to the subject
// of the object it changes and conditioned on that object's last sequence
// there. The subject is the ARBITRATION UNIT rather than a routing label: two
// writers saving one page contend at the broker and exactly one wins, and two
// writers on different pages never contend at all.
//
// # What the log removes that the bucket could not
//
// The package doc lists three two-key sequences — create, save and rename —
// each with its own crash state and its own grace rule for stepping over the
// debris. Every one of them existed because coordination has no multi-key
// transaction. Here they do not exist: a create is ONE record whose apply
// writes the title row, the page row, the first revision and the history entry
// in one transaction, so there is no orphan claim, no orphan revision and no
// window in which a title is held by a page that was never written.
//
// What the record must still state is the SCOPE — every object its apply
// touches — because that is what a node which cannot decode it files the
// deferral under.

// ObjectKind is what an object on the pages log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of — and the literal is RETAINED either way, for the reason
// [tracker.ObjectKind] gives: a newer peer publishes a kind this build does
// not know, and the deferral this build files it under forms a scope term out
// of that literal.
type ObjectKind string

// The six kinds.
//
// EXPORTED AND ENUMERATED because four readers that cannot see each other all
// compare against them: the publisher builds the subject, the wake feed's
// filter includes or excludes the kind, the applier's dispatch switches on it,
// and the domain's own Tables declaration must classify every one.
const (
	// KindContainer is a space's own settings: its name and its purpose.
	//
	// ITS ID IS THE CONTAINER KEY, which is what makes a deferred settings
	// edit block every write inside that container — the containment the
	// scope alphabet is for.
	KindContainer ObjectKind = "container"

	// KindPage is one page's head, and the overwhelming majority of
	// records: a body save, a label, a watcher, a comment, a trash and a
	// restore are all writes to the page.
	//
	// A COMMENT RIDES THE PAGE rather than holding a subject of its own,
	// which is the tracker's rule for the same object and for the same
	// reason: a comment changes what the page's card shows and what its
	// history says, so the two are one object's state and not two.
	KindPage ObjectKind = "page"

	// KindTitle is a container's hold on one normalised title, and the
	// subject a CREATE or a RENAME arbitrates on.
	//
	// THE TITLE IS THE ADDRESS, so making a page is a create-only append
	// at an expectation of zero on the title itself: two nodes creating
	// "Deploy Runbook" contend at the broker and exactly one wins, where
	// two nodes publishing on their own new pages' uuids would not contend
	// at all and would both succeed.
	KindTitle ObjectKind = "title"

	// KindEviction is a node's eviction from THIS log, or its readmission.
	//
	// Its own record here rather than the tracker's, because an eviction
	// fences records above a POSITION and positions on different streams
	// name different number spaces. One of the two kinds that install a
	// gate, which is why its version is pinned for ever.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero: two operators deriving the same number race there and
	// exactly one wins.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain.
	//
	// The only kind that writes no row on any node, which is why its table
	// declaration is the EMPTY set stated explicitly rather than left out.
	KindBarrier ObjectKind = "barrier"
)

// ObjectKinds are the six, and THE ORDER IS LOAD-BEARING.
//
// [statelogtest] publishes the FIRST THREE a domain declares, twice each, in
// order — so the declaration decides what the framework's own suite certifies.
// These three are a real sequence rather than three unrelated records: a
// container, a create on a title whose payload names the page, and then a
// patch on that page. A page patch on a page no create wrote is a malformed
// record under a strict replay, so putting the page before the title would
// certify a failure.
var ObjectKinds = []ObjectKind{
	KindContainer, KindTitle, KindPage, KindEviction, KindGeneration,
	KindBarrier,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// FIVE OF SIX DO. The barrier shares one subject across the whole domain, so
// an expectation there would serialise every linearizable read behind every
// other one and write an anchor row per read into the transaction holding this
// store's only writer.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier }

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later.
//
// A deferred gate does not postpone one record's effect on one node: it
// silently licenses every record above it, with no inverse that repairs it.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// ContainerSubject and the rest are the constructors.
//
// ONE PER KIND rather than a Subject{Kind, ID} literal at every call site,
// because the title's id is COMPOSED — "<CONTAINER>.<normalised title>" — and
// a composition written twice is a subject two writers disagree about.
func ContainerSubject(key string) Subject {
	return Subject{Kind: KindContainer, ID: key}
}
func PageSubject(id string) Subject { return Subject{Kind: KindPage, ID: id} }
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: fmt.Sprintf("%d", gen)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// TitleSubject names one container's claim on one title.
//
// # Why the title is a TOKEN rather than the title
//
// A subject is a broker path: it may not contain a space, a dot, a `*` or a
// `>`, and a page title is prose that routinely contains all four. The bucket
// this domain replaces solved that by escaping every segment of its key
// grammar; doing the same here would mean a SECOND escaping alphabet — the
// broker's, which is not coordination's — and two alphabets that must agree
// about one address for ever.
//
// So the address is arbitrated on a fixed-width digest of the normalised
// title, and three things make that the better trade:
//
//   - ARBITRATION NEEDS ONLY EQUALITY. Two writers claiming one address must
//     land on one subject, and nothing here ever needs to read the title back
//     out of the subject.
//   - THE SUBJECT IS A COST. It is a key in the broker's per-subject index on
//     every member for the life of the deployment; a 32-byte token is bounded
//     where an escaped title is up to 3x [MaxTitle].
//   - NOTHING IS LOST. The record's own payload carries the container and the
//     displayed title, the applier writes both, and [TitleToken] recomputes
//     the token from them — so a writer that arbitrated on one address while
//     claiming another is REFUSED rather than applied.
//
// THE TITLE IS NORMALISED HERE, once, so a caller cannot arbitrate on the
// author's own capitalisation: "Deploy Runbook" and "deploy runbook" are one
// address, and two subjects would make them two.
func TitleSubject(container, title string) Subject {
	return Subject{
		Kind: KindTitle,
		ID:   strings.ToUpper(container) + "." + TitleToken(title),
	}
}

// TitleToken is the address a title is arbitrated on.
//
// The first sixteen bytes of SHA-256 over the NORMALISED title, in lower-case
// hex. Truncated because the subject is a durable per-member index key and 128
// bits is already far past what the collision matters: a company with a
// million pages sits at about 1.5e-27, and a collision's whole consequence is
// that two titles contend at the broker and one create retries — not a wrong
// row and not a lost write.
func TitleToken(title string) string {
	sum := sha256.Sum256([]byte(NormalizeTitle(title)))
	return hex.EncodeToString(sum[:16])
}

// SplitTitleID takes a title subject's id apart, the inverse of
// [TitleSubject].
//
// It answers the container and the TOKEN — never a title, because the token is
// a digest. A caller that needs the title reads it from the record's payload,
// which is where the writer stated it.
func SplitTitleID(id string) (container, token string, err error) {
	container, token, ok := strings.Cut(id, ".")
	if !ok || container == "" || token == "" {
		return "", "", fmt.Errorf("pages: %q is not a title id — a title's id "+
			"is its container key, a dot and the title's own token", id)
	}
	return container, token, nil
}

// String renders the subject's own path — what the framework appends to the
// domain's subject prefix, and what a scope term names.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string {
	return topics.PagesLogSubject(string(s.Kind), s.ID)
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN and accepted where one is READ, which is the asymmetry the
// whole two-pass decode exists for: this build must be able to hold a newer
// peer's record under its own subject without being able to act on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("pages: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("pages: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again",
			s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("pages: a %s subject needs an id — only the barrier "+
			"is a kind with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("pages: subject id %q carries whitespace or a "+
			"wildcard, which the broker would read as a subject pattern", s.ID)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the pages log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.PagesLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
