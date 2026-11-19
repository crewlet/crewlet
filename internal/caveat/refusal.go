package caveat

import (
	"encoding/json"
	"errors"
)

// # A refusal, worded for a person (ADR-0060)
//
// A REFUSED call answers a model with a sentence naming the argument to change
// and the tool to call next: the tool's `Output`, kept exactly as is. It helps
// a person on a screen not at all, so a refusal a person may be shown carries
// a second phrasing, a [Refusal], written here; the act route answers that,
// never the model's.

// # The floor: no phrasing is not a model's phrasing
//
// A refusal with no [Refusal] is answered with its CODE's own sentence
// (`stale_version`, `forbidden`; httpjson's per-code `message`) and no
// `detail`: less for a person, never a model's words.

// # Constructed here, or rebuilt from a peer at one boundary
//
// A Refusal's field is unexported and does not decode from JSON, so only this
// package's catalog makes one, or the estate router rebuilds a peer's
// ([SaidFromPeer]): a walk over the catalog covers every refusal sentence this
// build can show a person, the guarantee [Caveat] makes.

// Refusal is a refused call's words for a person: what was refused and, where
// something is theirs to do, what, in the words of the controls they see; and,
// for a refusal a configurable limit caused, the [Remedy] naming that limit.
// The zero value is no phrasing at all, which a person's surface answers with
// the refusal code's own sentence.
type Refusal struct {
	person string
	remedy Remedy
}

// Person is the refusal as a person reads it.
func (r Refusal) Person() string { return r.person }

// Remedy is where a person changes the limit that caused the refusal, or the
// zero [Remedy] for a refusal no configurable limit caused.
func (r Refusal) Remedy() Remedy { return r.remedy }

// IsZero reports whether r is no phrasing at all.
func (r Refusal) IsZero() bool { return r.person == "" }

// peerRefusal is a [Refusal] as it crosses between nodes.
type peerRefusal struct {
	Person string  `json:"person"`
	Remedy *Remedy `json:"remedy,omitempty"`
}

// MarshalJSON writes the refusal as it crosses between nodes: its sentence and
// its remedy. There is no UnmarshalJSON, for [Caveat]'s reason: a peer's
// phrasing is rebuilt only by the estate router ([SaidFromPeer]).
func (r Refusal) MarshalJSON() ([]byte, error) {
	out := peerRefusal{Person: r.person}
	if !r.remedy.IsZero() {
		out.Remedy = &r.remedy
	}
	return json.Marshal(out)
}

// Said carries a [Refusal] in an error chain, so a domain writer's refusal
// reaches the tool that answers it with its person's words still on it, on
// this node and across the estate router (whose wire carries it as a typed
// value).
//
// THE WORDS RIDE THE ERROR, rather than being chosen again by each tool that
// answers it, because the writer is where the facts are: which question was
// asked of whom, which option was named. A tool turns the error into its
// result through [RefusalOf], and builtin's one refusal helper does so for
// every refusal that carries a cause.
type Said struct {
	Words Refusal
}

func (s *Said) Error() string { return "caveat: " + s.Words.person }

// Saying is err with a person's phrasing beside it: the same message, every
// identity err answers, and the [Said] that [RefusalOf] reads. A nil err stays
// nil, and a zero phrasing leaves err as it is.
func Saying(err error, words Refusal) error {
	if err == nil || words.IsZero() {
		return err
	}
	return &saying{err: err, said: &Said{Words: words}}
}

// saying is [Saying]'s error.
type saying struct {
	err  error
	said *Said
}

func (e *saying) Error() string   { return e.err.Error() }
func (e *saying) Unwrap() []error { return []error{e.err, e.said} }

// RefusalOf is the person's phrasing err carries, or the zero [Refusal] where
// it carries none.
func RefusalOf(err error) Refusal {
	var said *Said
	if errors.As(err, &said) && said != nil {
		return said.Words
	}
	return Refusal{}
}
