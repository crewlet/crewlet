package advisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

// AN OBSERVATION is one finding a run of its class raised, with what the store
// keeps beside it: the window its evidence covers, the revisions its fix was
// built against, and the two digests a later run and an Apply compare against
// (ADR-0164). A verdict is what the finding decides, never the numbers that
// drift under it, so an hour's new counts refresh a finding and do not revise it.

// Span is the span a finding's evidence was read over: a week for a pattern
// in what the company did, the instant of the read for a setting.
type Span struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// BasisKind is what a fix was built against.
type BasisKind string

// BasisConfig is the one basis: the company document at a revision.
const BasisConfig BasisKind = "config"

// Basis is one revision a fix was built against.
type Basis struct {
	Kind     BasisKind `json:"kind"`
	ID       string    `json:"id,omitempty"`
	Revision string    `json:"revision"`
}

// Observation is one finding as a run observed it.
type Observation struct {
	Finding Finding `json:"finding"`
	Window  Span    `json:"window"`
	Basis   []Basis `json:"basis"`
}

// Observe is f as observed over window, its fix built against the document at
// revision. AN OBSERVATION NAMES NO ATLAS ENTRY: entries are names a read
// fills for an asker who may read them (ADR-0141), so the store never keeps
// one, and the finding counts them in its facts.
func Observe(f Finding, window Span, revision string) Observation {
	f.Entries = nil
	basis := []Basis{}
	if f.Fix.Kind == FixConfigChange && revision != "" {
		basis = append(basis, Basis{Kind: BasisConfig, Revision: revision})
	}
	return Observation{Finding: f, Window: window, Basis: basis}
}

// Key is the observation's identity ([Key]).
func (o Observation) Key() string { return Key(o.Finding) }

// verdict is what a finding decides: its class and severity, who acts, what it
// is about, and the seats and atlas entries it names. Its facts, item and
// records are evidence and stay out.
type verdict struct {
	Class    Class    `json:"class"`
	Severity Severity `json:"severity"`
	Actor    Actor    `json:"actor"`
	Area     Area     `json:"area"`
	Subject  string   `json:"subject"`
	Focus    string   `json:"focus,omitempty"`
	Names    []string `json:"names,omitempty"`
	Entries  []string `json:"entries,omitempty"`
}

// VerdictDigest is the SHA-256 of f's verdict, in lowercase hex.
func VerdictDigest(f Finding) string {
	v := verdict{Class: f.Class, Severity: f.Severity, Actor: f.Actor, Area: f.Area,
		Subject: SubjectKey(f.Subject), Focus: f.Focus, Names: f.Names}
	for _, e := range f.Entries {
		v.Entries = append(v.Entries, e.ID.String())
	}
	slices.Sort(v.Entries)
	return digestOf(v)
}

// action is what a fix does, without the sentences that say it: a sentence or
// a prompt may carry a count, which is evidence.
type action struct {
	Kind    FixKind   `json:"kind"`
	Options []Option  `json:"options"`
	Setting string    `json:"setting,omitempty"`
	URL     string    `json:"url,omitempty"`
	Page    Page      `json:"page,omitempty"`
	Target  string    `json:"target,omitempty"`
	Act     *AtlasAct `json:"act,omitempty"`
}

// FixDigest is the SHA-256 of what fix does, in lowercase hex.
func FixDigest(fix Fix) string {
	options := make([]Option, len(fix.Options))
	for i, o := range fix.Options {
		options[i] = Option{Label: o.Label, Change: asStored(o.Change), Also: storedAll(o.Also)}
	}
	if fix.Options == nil {
		options = nil
	}
	return digestOf(action{Kind: fix.Kind, Options: options, Setting: fix.Setting, URL: fix.URL,
		Page: fix.Page, Target: fix.Target, Act: fix.Act})
}

// OptionDigest is the SHA-256 of one option's change, in lowercase hex: what an
// Apply sends, and what an ineffective fix is never offered again by.
func OptionDigest(o Option) string {
	// AN OPTION OF ONE CHANGE digests as it always has, so a finding kept
	// before options made several changes reads as unmoved.
	if len(o.Also) == 0 {
		return digestOf(asStored(o.Change))
	}
	return digestOf(storedAll(o.Changes()))
}

// storedAll is each change as a stored finding reads it back, nil for none.
func storedAll(chs []Change) []Change {
	if len(chs) == 0 {
		return nil
	}
	out := make([]Change, len(chs))
	for i, ch := range chs {
		out[i] = asStored(ch)
	}
	return out
}

// asStored is ch as a stored finding reads it back: its Set and Merge decoded
// from their own JSON. A DIGEST HASHES WHAT THE LOG KEEPS, not the values as a
// builder typed them: a typed value there (a schedule, which a struct encodes
// in its own field order) comes back from the log as a map, which encodes its
// keys sorted, so hashed as built the same change read back is another digest,
// and a finding observed again would read as moved every pass.
func asStored(ch Change) Change {
	ch.Set, ch.Merge = decodedFields(ch.Set), decodedFields(ch.Merge)
	return ch
}

// decodedFields is m as its JSON decodes into a map; m itself where it does
// not encode, which no change a builder makes does.
func decodedFields(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return m
	}
	return out
}

// digestOf is the SHA-256 of v's JSON. encoding/json writes a struct's fields
// in order and a map's keys sorted, so equal values give equal digests.
func digestOf(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Every value digested here is a struct of strings, numbers and maps
		// of them, which always encodes.
		panic("advisor: a digest's value does not encode: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
