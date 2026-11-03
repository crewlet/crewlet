package artifactsdomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the WRITER and readable at every version.
//
// It is what a node that cannot decode a record files the deferral under, and
// what a later read or write probes to discover it is behind. The framework
// knows one thing about a path, that it nests left to right, so the alphabet
// below is chosen so the paths NEST where the rows do: an asset sits under its
// artifact, because an asset record moves its artifact's held bytes, so a
// deferred asset record blocks a write to the artifact and a deferred purge of
// the artifact blocks a write to any of its assets.
//
//	a                         the whole domain: a gate, or an unreadable scope
//	a/x/<artifact>            one artifact, its versions, objects, shares and history
//	a/x/<artifact>/s/<asset>  one asset of it
//	a/s/<owner>.<token>       one owner's hold on one republish-in-place address
//	a/r/<record>              one person's pins and opened marks
//	a/x                       every artifact: a folder's removal re-files them all
//	a/f                       the folder tree: every folder, its shares and history
//	a/f/<folder>              one folder, as a placement reads it
//	a/b                       the company's adopted brand and the files it serves
//
// THE TREE NESTS EVERY FOLDER, so a record on the tree subject (a create, a
// rename, a move, a share, a removal) orders against every artifact record
// that read a folder (a create into it, a move into or out of it), and a
// deferred tree record blocks them all. A folder's removal also names `a/x`,
// because it re-files every artifact in the folder, which nests every
// artifact.
//
// The domain letter is `a`, distinct from every other domain's (`t`, `p`,
// `i`): a path is what an operator reads out of a deferral row when a fleet is
// stuck, and two domains whose paths looked alike would make that row
// ambiguous exactly when it matters.

const (
	pathDomain   = "a"
	pathArtifact = "x"
	pathAsset    = "s"
	pathSource   = "s"
	pathReader   = "r"
	pathFolder   = "f"
	pathBrand    = "b"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermArtifact is one artifact by id.
	TermArtifact TermKind = "artifact"

	// TermAsset is one asset, within its artifact.
	TermAsset TermKind = "asset"

	// TermSource is one republish-in-place address, by the source subject's
	// own id.
	TermSource TermKind = "source"

	// TermReader is one person's own state, by the name it is kept under.
	TermReader TermKind = "reader"

	// TermDomain is everything on this log: what a gate names and what an
	// unreadable scope reads as.
	TermDomain TermKind = "domain"

	// TermArtifacts is every artifact (`a/x`): what a folder's removal names,
	// since it re-files every artifact the folder held.
	TermArtifacts TermKind = "artifacts"

	// TermTree is the whole folder tree (`a/f`): the folder subject's own
	// path, which nests every folder.
	TermTree TermKind = "tree"

	// TermFolder is one folder by id (`a/f/<folder>`): what an artifact
	// record that reads a folder (a create into it, a move into or out of
	// it) names.
	TermFolder TermKind = "folder"

	// TermBrand is the company's adopted brand (`a/b`): the brand subject's
	// own path.
	TermBrand TermKind = "brand"
)

// TermKinds are the nine.
var TermKinds = []TermKind{TermArtifact, TermAsset, TermSource, TermReader, TermDomain,
	TermArtifacts, TermTree, TermFolder, TermBrand}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope.
//
// FOUR: the widest record here names an artifact and its source address (a
// source create, a transfer or a purge of a sourced artifact) or a reader and
// the artifact it pins, beside one folder (a sourced create into a folder) or
// two (a move from one folder into another). Nothing names more, so the cap is
// a writer's mistake caught rather than a bound a writer has to handle.
const MaxScopeTerms = 4

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names an object and nothing about the operation.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Artifact is the artifact an asset lives in. Required for TermAsset
	// and empty for every other kind.
	Artifact string `json:"a,omitempty"`

	// ID is the artifact's id, the asset's, the source subject's id or the
	// reader's record. Empty for TermDomain.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermArtifact:
		return join(pathDomain, pathArtifact, t.ID)
	case TermAsset:
		return join(pathDomain, pathArtifact, t.Artifact, pathAsset, t.ID)
	case TermSource:
		return join(pathDomain, pathSource, t.ID)
	case TermReader:
		return join(pathDomain, pathReader, t.ID)
	case TermDomain:
		return pathDomain
	case TermArtifacts:
		return join(pathDomain, pathArtifact)
	case TermTree:
		return join(pathDomain, pathFolder)
	case TermFolder:
		return join(pathDomain, pathFolder, t.ID)
	case TermBrand:
		return join(pathDomain, pathBrand)
	}
	// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM: a term this
	// build cannot interpret is a claim about a blast radius it cannot
	// bound, and the only safe reading of an unbounded claim is the widest.
	return pathDomain
}

// join builds a scope path from its segments through the framework's one
// spelling of a path ([statelog.ScopePath]). A MISSING SEGMENT IS THE DOMAIN
// rather than a path with a hole in it: a term whose id is empty cannot be
// placed, and the widest reading is the only one that is not a guess.
func join(segments ...string) string {
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			return pathDomain
		}
	}
	return statelog.ScopePath(segments...)
}

// ScopeSet is the COMPLETE set of objects a record's apply may write.
//
// NEVER EMPTY. The exactly-the-subject case encodes as the one-byte sentinel,
// so the field can never be absent by accident. An absent or unreadable scope
// on a record this build cannot decode is the DOMAIN term, never "just the
// subject".
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	Subject bool

	// Terms is the enumeration, when the record touches more than its own
	// subject.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case is one byte on the wire.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the domain.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set: this runs on a node reading a record a newer build wrote, and an
// error here would take the whole envelope pass down with it.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		if sentinel == ScopeSentinel {
			*s = ScopeSet{Subject: true}
			return nil
		}
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		return nil
	}
	var terms []ScopeTerm
	if err := json.Unmarshal(b, &terms); err != nil || len(terms) == 0 {
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT, per the
		// paragraph above: returning the decode error would fail the
		// envelope pass that exists so such a record can be filed, gated
		// and reprocessed rather than dropped.
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Resolve renders the scope as the framework's own paths.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return statelog.ScopeSet{Paths: []string{subjectPath(subject)}}
	}
	paths := make([]string, 0, len(s.Terms))
	for _, t := range s.Terms {
		paths = append(paths, t.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// Validate refuses a scope a writer cannot mean.
func (s ScopeSet) Validate() error {
	if s.Subject {
		if len(s.Terms) > 0 {
			return errors.New("artifactsdomain: a scope is either the subject " +
				"sentinel or an enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("artifactsdomain: a record's scope names nothing, which " +
			"would say it makes no read stale: the one claim a record this build " +
			"may not be able to decode must never make")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("artifactsdomain: a record's scope names %d objects and "+
			"the cap is %d", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermArtifact, TermSource, TermReader, TermFolder:
			if t.ID == "" {
				return fmt.Errorf("artifactsdomain: a %s term names nothing", t.Kind)
			}
		case TermArtifacts, TermTree, TermBrand:
			if t.ID != "" || t.Artifact != "" {
				return fmt.Errorf("artifactsdomain: a %s term names a whole set and "+
					"carries no id", t.Kind)
			}
		case TermAsset:
			if t.ID == "" || t.Artifact == "" {
				return errors.New("artifactsdomain: an asset term names its " +
					"artifact and itself")
			}
		case TermDomain:
		default:
			return fmt.Errorf("artifactsdomain: scope term kind %q is not one this "+
				"build writes", t.Kind)
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindArtifact:
		return ScopeTerm{Kind: TermArtifact, ID: s.ID}.Path()
	case KindAsset:
		artifact, asset, err := SplitAssetID(s.ID)
		if err != nil {
			// AN UNPARSEABLE ASSET SUBJECT IS THE DOMAIN: a narrower
			// guess would be a claim the record never made.
			return pathDomain
		}
		return ScopeTerm{Kind: TermAsset, Artifact: artifact, ID: asset}.Path()
	case KindSource:
		return ScopeTerm{Kind: TermSource, ID: s.ID}.Path()
	case KindReader:
		return ScopeTerm{Kind: TermReader, ID: s.ID}.Path()
	case KindBarrier, KindTrimmed:
		// THE FRAMEWORK'S OWN SCOPE, so a framework record — the read
		// index's barrier, or the trim's trimmed record, which writes no
		// row of this domain's either — never intersects any
		// read's closure.
		return statelog.FrameworkScope
	case KindFolder:
		return ScopeTerm{Kind: TermTree}.Path()
	case KindBrand:
		return ScopeTerm{Kind: TermBrand}.Path()
	}
	// A GATE IS ABOUT THE WHOLE DOMAIN, and so is a kind this build does
	// not know: anything narrower would be a claim the record never made.
	return pathDomain
}

// artifactTerm is one artifact's term.
func artifactTerm(id string) ScopeTerm { return ScopeTerm{Kind: TermArtifact, ID: id} }

// FolderTerm is one folder's term, what an artifact record that reads the
// folder names. The top level (”) is no folder and has no term: a record
// filing an artifact at the top level reads no folder row.
func FolderTerm(id string) ScopeTerm { return ScopeTerm{Kind: TermFolder, ID: id} }

// TreeTerm is the whole folder tree's term.
func TreeTerm() ScopeTerm { return ScopeTerm{Kind: TermTree} }

// ArtifactsTerm is every artifact's term.
func ArtifactsTerm() ScopeTerm { return ScopeTerm{Kind: TermArtifacts} }

// BrandTerm is the adopted brand's term.
func BrandTerm() ScopeTerm { return ScopeTerm{Kind: TermBrand} }

// DomainScope is the closure of a read about every artifact: a listing, the
// usage counters, the trash reaper's question and the collector's pass.
func DomainScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}
}

// ArtifactScope is the closure of a read about one artifact and everything it
// holds: its head, versions, objects, assets, shares, reads and history.
func ArtifactScope(id string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{artifactTerm(id).Path()}}
}

// TreeScope is the closure of a read about the folder tree alone: its folders,
// their shares and history. A read that also counts the artifacts in each
// folder reads artifact rows too, and is closed over [DomainScope].
func TreeScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{TreeTerm().Path()}}
}

// BrandScope is the closure of a read about the adopted brand: its snapshot
// and the files it serves.
func BrandScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{BrandTerm().Path()}}
}
