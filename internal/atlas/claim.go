package atlas

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/redact"
)

// SourceKind is where a claim came from. A closed set, and the claim's
// confidence is a function of it: nobody passes a confidence in.
type SourceKind string

const (
	// SourceSettings is the company settings, read through the redacting copy.
	SourceSettings SourceKind = "settings"
	// SourceRecords is a record the engine already keeps (Gitlet's rows, an
	// integration's status, the access log), read when the atlas is read.
	SourceRecords SourceKind = "records"
	// SourceIntegration is an integration's read with the company's credential.
	SourceIntegration SourceKind = "integration"
	// SourceAgent is an agent in a named turn.
	SourceAgent SourceKind = "agent"
	// SourcePerson is a person.
	SourcePerson SourceKind = "person"
)

// SourceKinds is every source kind.
var SourceKinds = []SourceKind{SourceSettings, SourceRecords, SourceIntegration, SourceAgent, SourcePerson}

// Valid reports whether k is a member of the closed set.
func (k SourceKind) Valid() bool { return slices.Contains(SourceKinds, k) }

// Computed reports whether claims of this kind are computed when the atlas is
// read, from state that already lives elsewhere, and never stored.
func (k SourceKind) Computed() bool { return k == SourceSettings || k == SourceRecords }

// Evidence is what backs an agent's claim, as the engine checked it.
type Evidence string

const (
	// EvidenceCall is a server or vendor tool call of the same turn whose
	// answer showed the value.
	EvidenceCall Evidence = "call"
	// EvidenceFile is a repository file, a page or a work item the agent read.
	EvidenceFile Evidence = "file"
	// EvidenceNone is nothing the engine can check.
	EvidenceNone Evidence = "none"
)

// Evidences is every evidence class.
var Evidences = []Evidence{EvidenceCall, EvidenceFile, EvidenceNone}

// Valid reports whether e is a member of the closed set.
func (e Evidence) Valid() bool { return slices.Contains(Evidences, e) }

// Source is who or what made one claim. Name is what within its kind: a
// [Record] word for the engine's records, an integration's kind, an agent's
// seat handle, a person's handle; empty for the settings, named by Revision.
type Source struct {
	Kind     SourceKind `json:"kind"`
	Name     string     `json:"name,omitempty"`
	Revision string     `json:"revision,omitempty"`
	Turn     string     `json:"turn,omitempty"`
	Evidence Evidence   `json:"evidence,omitempty"`
}

// Record is which of the engine's own records a computed claim was read from.
type Record string

// The engine's records the atlas reads.
const (
	RecordGitlet       Record = "gitlet"
	RecordIntegrations Record = "integrations"
	RecordSeatAccess   Record = "seat_access"
	RecordToolServers  Record = "tool_servers"
	RecordSSHTests     Record = "ssh_tests"
	RecordAccessLog    Record = "access_log"
	RecordUsage        Record = "usage"
)

// Records is every record family.
var Records = []Record{
	RecordGitlet, RecordIntegrations, RecordSeatAccess, RecordToolServers,
	RecordSSHTests, RecordAccessLog, RecordUsage,
}

// Valid reports whether r is a member of the closed set.
func (r Record) Valid() bool { return slices.Contains(Records, r) }

// SettingsSource is the source of a claim the company settings make at
// revision.
func SettingsSource(revision string) Source { return Source{Kind: SourceSettings, Revision: revision} }

// RecordSource is the source of a claim the engine's own record r makes.
func RecordSource(r Record) Source { return Source{Kind: SourceRecords, Name: string(r)} }

// PersonSource is the source of a claim the person with this handle made.
func PersonSource(handle string) Source { return Source{Kind: SourcePerson, Name: handle} }

// AgentSource is the source of a claim the seat made in turn, backed by
// evidence.
func AgentSource(seat, turn string, evidence Evidence) Source {
	return Source{Kind: SourceAgent, Name: seat, Turn: turn, Evidence: evidence}
}

// IntegrationSource is the source of a claim an integration's read made.
func IntegrationSource(kind string) Source { return Source{Kind: SourceIntegration, Name: kind} }

// Confidence is how far a claim can be trusted, DERIVED from its source and
// the evidence the engine checked, never chosen by whoever made the claim: a
// value a writer chooses is a channel content can steer.
func (s Source) Confidence() Confidence {
	switch s.Kind {
	case SourceSettings, SourceRecords:
		return ConfidenceDeclared
	case SourcePerson:
		return ConfidenceAsserted
	case SourceIntegration:
		return ConfidenceVerified
	case SourceAgent:
		switch s.Evidence {
		case EvidenceCall:
			return ConfidenceObserved
		case EvidenceFile:
			return ConfidenceInferred
		}
	}
	return ConfidenceReported
}

// nameRE is a handle, a unit's key or an integration's kind, compiled at
// first use for the same reason as the value grammars.
var nameRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._:@+-]{0,127}$`) })

func (s Source) validate() error {
	switch {
	case !s.Kind.Valid():
		return errors.New("source: not a kind of source")
	case s.Kind == SourceSettings:
		if s.Name != "" || s.Turn != "" || s.Evidence != "" {
			return errors.New("source: the settings are named by their revision alone")
		}
	case s.Kind == SourceRecords:
		if !Record(s.Name).Valid() || s.Revision != "" || s.Turn != "" || s.Evidence != "" {
			return errors.New("source: not one of the engine's records")
		}
	case !nameRE().MatchString(s.Name):
		return fmt.Errorf("source: a %s is named by its handle or kind", s.Kind)
	case s.Kind == SourceAgent:
		if s.Turn == "" || !s.Evidence.Valid() {
			return errors.New("source: an agent's claim names its turn and its evidence")
		}
	case s.Turn != "" || s.Evidence != "":
		return fmt.Errorf("source: only an agent's claim names a turn and evidence, not a %s's", s.Kind)
	}
	return nil
}

// Confidence orders claims. Six levels for the fold, three words for a reader.
type Confidence string

// The confidence levels, the most trusted first.
const (
	ConfidenceDeclared Confidence = "declared"
	ConfidenceAsserted Confidence = "asserted"
	ConfidenceVerified Confidence = "verified"
	ConfidenceObserved Confidence = "observed"
	ConfidenceInferred Confidence = "inferred"
	ConfidenceReported Confidence = "reported"
)

// Confidences is every level, the most trusted first.
var Confidences = []Confidence{
	ConfidenceDeclared, ConfidenceAsserted, ConfidenceVerified,
	ConfidenceObserved, ConfidenceInferred, ConfidenceReported,
}

// Valid reports whether c is a member of the closed set.
func (c Confidence) Valid() bool { return slices.Contains(Confidences, c) }

// Rank orders confidences, 0 the most trusted. What the settings or the
// engine's records declare and what a person asserts share the top rank: a
// writer refuses a person's claim on a fact the settings declare, so the two
// never contend for one facet.
func (c Confidence) Rank() int {
	switch c {
	case ConfidenceDeclared, ConfidenceAsserted:
		return 0
	case ConfidenceVerified:
		return 1
	case ConfidenceObserved:
		return 2
	case ConfidenceInferred:
		return 3
	}
	return 4
}

// Word is how a confidence reads on screen.
type Word string

// The words a confidence reads as.
const (
	WordConfirmed   Word = "confirmed"
	WordLikely      Word = "likely"
	WordUnconfirmed Word = "unconfirmed"
)

// Words is every word a confidence reads as.
var Words = []Word{WordConfirmed, WordLikely, WordUnconfirmed}

// Display is the word a reader is shown: confirmed for the settings, the
// engine's records, a person and an integration; likely for an agent backed by
// a call or a file; unconfirmed for an agent with nothing to check.
func (c Confidence) Display() Word {
	switch c.Rank() {
	case 0, 1:
		return WordConfirmed
	case 2, 3:
		return WordLikely
	}
	return WordUnconfirmed
}

// FacetKind is what a claim is about.
type FacetKind string

const (
	// FacetExists is the entry itself.
	FacetExists FacetKind = "exists"
	// FacetFact is one fact, or one value of a fact that holds many.
	FacetFact FacetKind = "fact"
	// FacetRelation is one relation to one target.
	FacetRelation FacetKind = "relation"
	// FacetDistinct is a person's "not the same": two entries a shared key
	// would merge stay apart, whatever the key says.
	FacetDistinct FacetKind = "distinct"
	// FacetSame is a person's "the same": two entries of one kind no exact key
	// joins are one entry. A "not the same" between them always wins.
	FacetSame FacetKind = "same"
)

// FacetKinds is every facet kind.
var FacetKinds = []FacetKind{FacetExists, FacetFact, FacetRelation, FacetDistinct, FacetSame}

// Valid reports whether k is a member of the closed set.
func (k FacetKind) Valid() bool { return slices.Contains(FacetKinds, k) }

// Target is what a relation, or a person's "not the same", points at: an
// entry, or an identity outside the atlas (a seat, a unit, a page).
type Target struct {
	Entry    Ref      `json:"entry,omitzero"`
	External External `json:"external,omitempty"`
	Name     string   `json:"name,omitempty"`
}

// Facet is one thing a claim is about.
type Facet struct {
	Kind     FacetKind `json:"kind"`
	Fact     Fact      `json:"fact,omitempty"`
	Relation Relation  `json:"relation,omitempty"`
	Target   Target    `json:"target,omitzero"`
}

// Claim is one fact somebody or something holds about one entry: what, from
// where, as of when. A claim computed when the atlas is read has no Position;
// a recorded one carries its log position.
type Claim struct {
	Subject    Ref       `json:"subject"`
	Facet      Facet     `json:"facet"`
	Value      Value     `json:"value,omitzero"`
	Denied     bool      `json:"denied,omitempty"`
	Source     Source    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Position   uint64    `json:"position,omitempty"`
}

// Confidence is the claim's, derived from its source.
func (c Claim) Confidence() Confidence { return c.Source.Confidence() }

// Exists is the claim that subject is something the company runs on.
func Exists(subject Ref, src Source, at time.Time) (Claim, error) {
	return checked(Claim{Subject: subject, Facet: Facet{Kind: FacetExists}, Source: src, ObservedAt: at})
}

// Says is the claim that subject's fact f holds raw, raw read through f's
// grammar and refused as it would refuse it.
func Says(subject Ref, f Fact, raw string, src Source, at time.Time) (Claim, error) {
	v, err := f.Value(raw)
	if err != nil {
		return Claim{}, err
	}
	return checked(Claim{Subject: subject, Facet: Facet{Kind: FacetFact, Fact: f}, Value: v, Source: src, ObservedAt: at})
}

// Relates is the claim that from relies on the entry to through r.
func Relates(from Ref, r Relation, to Ref, src Source, at time.Time) (Claim, error) {
	return checked(Claim{Subject: from, Facet: Facet{Kind: FacetRelation, Relation: r, Target: Target{Entry: to}},
		Source: src, ObservedAt: at})
}

// Names is the claim that from relates through r to an identity outside the
// atlas: its owning seat or unit, the page that documents it.
func Names(from Ref, r Relation, ext External, name string, src Source, at time.Time) (Claim, error) {
	return checked(Claim{Subject: from, Facet: Facet{Kind: FacetRelation, Relation: r,
		Target: Target{External: ext, Name: name}}, Source: src, ObservedAt: at})
}

// Distinct is a person's claim that a and b are not the same thing, whatever
// key they share.
func Distinct(a, b Ref, src Source, at time.Time) (Claim, error) {
	return checked(Claim{Subject: a, Facet: Facet{Kind: FacetDistinct, Target: Target{Entry: b}}, Source: src, ObservedAt: at})
}

// Same is a person's claim that a and b are one thing, though no exact key
// joins them. A "not the same" between them always wins.
func Same(a, b Ref, src Source, at time.Time) (Claim, error) {
	return checked(Claim{Subject: a, Facet: Facet{Kind: FacetSame, Target: Target{Entry: b}}, Source: src, ObservedAt: at})
}

// Slot is the one thing a claim fills on its entry, spelled for a store that
// keeps claims: its fact, one value of a fact that holds many, one relation to
// one target, one other entry it is or is not the same as, or the entry itself.
// Two claims share a slot exactly when the fold reads them as one facet
// (facetOf), so a store replaces a voice's claims where the fold does.
func (c Claim) Slot() string {
	id := facetOf(&c)
	f := id.facet
	return strings.Join([]string{string(f.Kind), string(f.Fact), string(f.Relation), f.Target.Entry.String(),
		string(f.Target.External), f.Target.Name, id.value}, "\x00")
}

// Denial is a person's claim that c does not hold: the entry is retired, the
// value is wrong, the relation is not there.
func Denial(c Claim, person Source, at time.Time) (Claim, error) {
	c.Denied, c.Source, c.ObservedAt, c.Position = true, person, at, 0
	return checked(c)
}

func checked(c Claim) (Claim, error) {
	if err := c.Validate(); err != nil {
		return Claim{}, err
	}
	return c, nil
}

// Validate refuses a claim no writer may make: an entry or a target outside
// its kind's grammar, a fact or a relation its kind does not take, a value its
// fact's grammar does not read back to itself, a source that names itself
// wrongly, a denial from anybody but a person (an integration's read may deny
// the entry itself alone), a "not the same" from anybody but a person, and a
// fact of meaning or an owner from an agent.
func (c Claim) Validate() error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("atlas: claim on %s: %w", c.Subject, err)
	}
	return nil
}

// Recordable refuses a claim a writer may not record though [Claim.Validate]
// admits it: one naming an entry, or a seat or a unit, by a key shaped like a
// credential ([ErrCredential]). Every value its grammar already judges; this is
// the keys', asked by every writer of a recorded claim and never by the fold.
// The refusal names the entry's kind and never a key.
func (c Claim) Recordable() error {
	refs := []Ref{c.Subject}
	if t := c.Facet.Target.Entry; t != (Ref{}) {
		refs = append(refs, t)
	}
	for _, r := range refs {
		if err := r.Recordable(); err != nil {
			return fmt.Errorf("atlas: claim on a %s: %w", c.Subject.Kind, err)
		}
	}
	if t := c.Facet.Target; t.Name != "" && redact.Contains(t.Name) {
		return fmt.Errorf("atlas: claim on a %s: the %s it names: %w", c.Subject.Kind, t.External,
			ErrCredential)
	}
	return nil
}

func (c Claim) validate() error {
	r, err := NewRef(c.Subject.Kind, c.Subject.Key)
	if err != nil {
		return err
	}
	if r != c.Subject {
		return errors.New("the entry's key is not in its canonical form")
	}
	if err := c.Source.validate(); err != nil {
		return err
	}
	if c.ObservedAt.IsZero() {
		return errors.New("a claim says when it was seen")
	}
	if c.Denied && c.Source.Kind != SourcePerson &&
		(c.Source.Kind != SourceIntegration || c.Facet.Kind != FacetExists) {
		// A PERSON DENIES ANY CLAIM. An integration's read denies only the
		// entry itself: its retirement, once two complete reads missed it.
		// It never denies a fact or a relation, which it withdraws instead.
		return errors.New("only a person denies a claim, and an integration only the entry itself")
	}
	if c.Facet.Kind != FacetFact && !c.Value.Equal(Value{}) {
		return errors.New("only a fact carries a value")
	}
	switch c.Facet.Kind {
	case FacetExists:
		if c.Facet != (Facet{Kind: FacetExists}) {
			return errors.New("the entry's own claim names no fact and no relation")
		}
	case FacetFact:
		return c.validateFact()
	case FacetRelation:
		return c.validateRelation()
	case FacetDistinct, FacetSame:
		if c.Source.Kind != SourcePerson {
			return errors.New(`only a person says whether two entries are the same`)
		}
		if t := c.Facet.Target; t.External != "" || t.Name != "" || t.Entry.Kind != c.Subject.Kind || t.Entry == c.Subject {
			return errors.New(`"the same" and "not the same" name another entry of the same kind`)
		}
		if _, err := NewRef(c.Facet.Target.Entry.Kind, c.Facet.Target.Entry.Key); err != nil {
			return err
		}
	default:
		return errors.New("not a facet")
	}
	return nil
}

func (c Claim) validateFact() error {
	f := c.Facet.Fact
	if c.Facet.Relation != "" || c.Facet.Target != (Target{}) {
		return errors.New("a fact names no relation")
	}
	if !f.AppliesTo(c.Subject.Kind) {
		return fmt.Errorf("%s does not carry %q", c.Subject.Kind, f)
	}
	if c.Source.Kind == SourceAgent && f.Spec().Meaning {
		return fmt.Errorf("an agent never sets %s: a person decides it", f)
	}
	v, err := f.Value(c.Value.Text)
	if err != nil {
		return err
	}
	if v != c.Value {
		return fmt.Errorf("%s: the value is not in its canonical form", f)
	}
	return nil
}

func (c Claim) validateRelation() error {
	r, t := c.Facet.Relation, c.Facet.Target
	if c.Facet.Fact != "" {
		return errors.New("a relation names no fact")
	}
	if r == RelationOwnedBy && c.Source.Kind == SourceAgent {
		return errors.New("an agent never sets an owner: a person decides it")
	}
	if t.External != "" {
		if t.Entry != (Ref{}) || !r.Names(c.Subject.Kind, t.External) {
			return fmt.Errorf("%s does not name a %s through %s", c.Subject.Kind, t.External, r)
		}
		if t.External == ExternalPage {
			if _, err := uuid.Parse(t.Name); err != nil {
				return errors.New("a page is named by its id")
			}
			return nil
		}
		if !nameRE().MatchString(t.Name) {
			return fmt.Errorf("a %s is named by its handle or key", t.External)
		}
		return nil
	}
	if t.Name != "" || !r.Joins(c.Subject.Kind, t.Entry.Kind) {
		return fmt.Errorf("%s does not run from %s to %s", r, c.Subject.Kind, t.Entry.Kind)
	}
	canonical, err := NewRef(t.Entry.Kind, t.Entry.Key)
	if err != nil {
		return err
	}
	if canonical != t.Entry {
		return errors.New("the target's key is not in its canonical form")
	}
	if t.Entry == c.Subject {
		return errors.New("an entry does not relate to itself")
	}
	return nil
}
