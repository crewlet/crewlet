package caveat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// # A refusal, worded for a person (ADR-0060)
//
// A call that is REFUSED answers a model with a sentence written where the
// refusal is decided, naming the argument to change and the tool to call next:
// that sentence is the tool's `Output`, and it stays exactly as it is. A
// person reading the same refusal on a screen is helped by none of it. So a
// refusal a person may be shown carries a second phrasing, a [Refusal],
// written in this package beside every other sentence a person is shown, and
// the act route answers that phrasing and never the model's.
//
// # The floor: no phrasing is not a model's phrasing
//
// A refusal is answered whether or not a person phrasing was written for it,
// because its CODE already says what happened: `stale_version`, `forbidden`,
// `invalid`. A refusal with no [Refusal] is answered with its code's own
// sentence (httpjson's per-code `message`, written for people) and no
// `detail`. A site without a phrasing tells a person less, and never tells
// them a model's words.
//
// # Constructed here only
//
// A Refusal's field is unexported, so the walk over this package's catalog is
// a walk over every refusal sentence any surface can show a person, the same
// guarantee [Caveat] makes.

// Refusal is a refused call's words for a person: what was refused and, where
// something is theirs to do, what, in the words of the controls they see. The
// zero value is no phrasing at all, which a person's surface answers with the
// refusal code's own sentence.
type Refusal struct {
	person string
}

// Person is the refusal as a person reads it.
func (r Refusal) Person() string { return r.person }

// IsZero reports whether r is no phrasing at all.
func (r Refusal) IsZero() bool { return r.person == "" }

// MarshalJSON writes the refusal as it crosses between nodes: its sentence.
func (r Refusal) MarshalJSON() ([]byte, error) { return json.Marshal(r.person) }

// UnmarshalJSON reads a refusal a peer wrote. Its words are kept as written: a
// newer peer's phrasing is still a person's phrasing, held by that peer's own
// walk.
func (r *Refusal) UnmarshalJSON(raw []byte) error {
	var person string
	if err := json.Unmarshal(raw, &person); err != nil {
		return fmt.Errorf("caveat: a refusal's phrasing: %w", err)
	}
	*r = Refusal{person: strings.TrimSpace(person)}
	return nil
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
