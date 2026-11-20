package wire

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// Ref is one record a proposal or its receipt names: its kind and id, the name
// the card shows it by, and its page where the link table holds one.
type Ref struct {
	Kind ResourceKind `json:"kind"`
	ID   string       `json:"id"`
	// Name is how the record is shown: a seat's or a person's name, a task's
	// key, an artifact's title. Never a handle where the record has a name.
	Name string `json:"name"`
	// ArtifactKind is an artifact's kind ("slides"), which the dashboard
	// draws as its glyph; absent for every other record.
	ArtifactKind string `json:"artifact_kind,omitempty"`
	// Open is the record's own page, built from the link table; absent for
	// a record no screen opens (a team).
	Open *Link `json:"open,omitempty"`
}

// Part is one run of a [Phrase]: words, or exactly one record they name.
type Part struct {
	Text string `json:"text,omitempty"`
	Ref  *Ref   `json:"ref,omitempty"`
}

// Phrase is words that name records, run by run ("QA can open it." is the seat,
// then words): a card draws each record as itself, a plain reader [Phrase.Text].
type Phrase []Part

// Plain is words that name nothing; empty words are the empty phrase.
func Plain(s string) Phrase { return Phrase(nil).Plain(s) }

// Naming is the phrase that is one record.
func Naming(r Ref) Phrase { return Phrase(nil).Name(r) }

// Plain appends words that name nothing.
func (p Phrase) Plain(s string) Phrase {
	if s == "" {
		return p
	}
	return append(p[:len(p):len(p)], Part{Text: s})
}

// Name appends a record. One with no id is its name alone, so a record the
// preview could not identify still says what it said.
func (p Phrase) Name(r Ref) Phrase {
	if r.ID == "" || !r.Kind.Valid() {
		return p.Plain(r.Name)
	}
	if r.Name == "" {
		r.Name = r.ID
	}
	return append(p[:len(p):len(p)], Part{Ref: &r})
}

// Then appends another phrase.
func (p Phrase) Then(o Phrase) Phrase { return append(p[:len(p):len(p)], o...) }

// Text is the phrase as words: each record by its name.
func (p Phrase) Text() string {
	var b strings.Builder
	for _, part := range p {
		if part.Ref != nil {
			b.WriteString(part.Ref.Name)
			continue
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

// Refs are the records the phrase names, in order.
func (p Phrase) Refs() []Ref {
	var out []Ref
	for _, part := range p {
		if part.Ref != nil {
			out = append(out, *part.Ref)
		}
	}
	return out
}

// Join is the phrases one after another, sep between each.
func Join(phrases []Phrase, sep string) Phrase {
	var out Phrase
	for i, p := range phrases {
		if i > 0 {
			out = out.Plain(sep)
		}
		out = out.Then(p)
	}
	return out
}

// validate refuses a run that is neither words nor one record, and a record
// with no kind this build knows, no id or no name.
func (p Phrase) validate(what string) error {
	for _, part := range p {
		switch {
		case (part.Text == "") == (part.Ref == nil):
			return fmt.Errorf("%w: each run of %s is words or one record", ErrInvalid, what)
		case part.Ref == nil:
		case !part.Ref.Kind.Valid():
			return fmt.Errorf("%w: %s names a record of kind %q, not one of %s", ErrInvalid,
				what, part.Ref.Kind, words.Or(ResourceKinds))
		case part.Ref.ID == "" || part.Ref.Name == "":
			return fmt.Errorf("%w: %s names a record with no id or no name", ErrInvalid, what)
		case part.Ref.Open != nil && part.Ref.Open.Href == "":
			return fmt.Errorf("%w: %s names a record whose page has no address", ErrInvalid, what)
		}
	}
	return nil
}
