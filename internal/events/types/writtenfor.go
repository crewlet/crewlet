package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// WrittenFor is whom a write was made for, as the actor that makes it states
// it (ADR-0109): whoever the turn that wrote it works for ([WorkingFor]), which
// the record the write leaves carries where that record wakes somebody, so the
// turn it wakes works for them too and never for the writing seat, which holds
// every app. A colleague's ask carries the same carrier ([A2ARequest]); this
// is the same hop through what a seat writes on the engine's own surfaces:
// Worklet, Doclet and Chatlet.
//
// # The builder states it, and the type refuses an agent's that did not
//
// THE ZERO VALUE STATES NOTHING. An agent writes only in a turn, and a turn
// works for somebody or for the seat itself, so an agent's actor that states
// neither came from a builder that forgot, and its write would wake whoever it
// reaches for the agent with every app: [WrittenFor.For] refuses it, every
// domain asks it of the actor before it builds a record, and every actor type
// that crosses the wire asks it before it is sent. A person, a
// credential acting as no seat and the engine write for nobody but themselves
// (a principal acting through their own credential is no deputy), so for them
// the zero value is the answer, and a carrier on their write is refused.
//
// # The engine's, never the model's
//
// [ForTurn]'s carrier is composed from the call's own bound ceiling
// (internal/agent/builtin), as a colleague's ask's is: never from a tool's
// arguments or anything a model wrote.
//
// # Across the wire
//
// An actor crosses to a data node inside the estate's signed request, and the
// node that built it refuses to SEND one [WrittenFor.For] refuses: every actor
// type that crosses checks it in its own MarshalJSON (Doclet's and Chatlet's
// Actor, and internal/estate's Actor, which carries Worklet's provenance
// beside its kind). The data node cannot make that check itself, because it
// cannot tell an absent carrier a current node stated from one an older node,
// knowing no carrier, never sent. So what arrived is what the node that built
// it stated ([WrittenFor.Received]): an absent carrier is nobody, the writing
// seat's own work or, until the roll completes, an older node's write
// (ADR-0109).
type WrittenFor struct {
	stated  bool
	carrier *WorkingFor
}

// ForTurn is a write a turn makes: whoever the turn works for, composed from
// the call's own bound ceiling, or nil for a turn that works for its own seat
// (one nobody woke, or one the outcome of its own act woke), whose write
// carries nobody.
func ForTurn(w *WorkingFor) WrittenFor { return WrittenFor{stated: true, carrier: w.Clone()} }

// ForNobody is an agent's write that no turn's work is in: the engine's own,
// made as the seat, which carries nobody, so the turn it wakes works for the
// seat as a turn nobody woke does. The site that states it says why.
func ForNobody() WrittenFor { return WrittenFor{stated: true} }

// Stated reports whether the actor's builder stated whom the write is for.
func (f WrittenFor) Stated() bool { return f.stated }

// Carrier is whom the write was made for, and nil for a write that carries
// nobody: one outside any turn's work, one a turn working for its own seat
// made, and one nobody stated.
func (f WrittenFor) Carrier() *WorkingFor { return f.carrier.Clone() }

// For is whom a write by an actor of kind was made for, or why such an actor
// cannot make it: an agent's actor that states nothing is refused
// ([ErrWrittenForUnstated]), and so is a carrier on the write of a person, a
// credential or the engine ([ErrWrittenForNotAnAgents]).
//
// ONE RULE FOR EVERY DOMAIN, whose author kinds are the identity directory's
// own words (iam.ActorFor), so Worklet, Doclet and Chatlet cannot each decide
// differently which actors must say.
func (f WrittenFor) For(kind iam.ActorKind) (*WorkingFor, error) {
	if kind == iam.ActorAgent {
		if !f.stated {
			return nil, ErrWrittenForUnstated
		}
		return f.Carrier(), nil
	}
	if f.carrier != nil {
		return nil, fmt.Errorf("%w: this is a write of kind %q", ErrWrittenForNotAnAgents, kind)
	}
	return nil, nil
}

var (
	// ErrWrittenForUnstated is an agent's write whose actor states nobody it
	// was made for: a builder that forgot, and a bug to report rather than a
	// refusal to show a caller.
	ErrWrittenForUnstated = errors.New("an agent's write states nobody it was made for: an actor " +
		"built for a turn carries whoever the turn works for (types.ForTurn), and the engine's own " +
		"write as the seat says it carries nobody (types.ForNobody)")

	// ErrWrittenForNotAnAgents is a carrier on a write that is no agent's: a
	// person, a credential and the engine write for nobody but themselves.
	ErrWrittenForNotAnAgents = errors.New("only an agent's write carries whom it was made for")
)

// MarshalJSON writes the carrier, or null. An actor's field is omitted where it
// carries nobody ([WrittenFor.IsZero], `omitzero`), so an actor carrying
// nobody goes out byte for byte as a build without the field sends it.
func (f WrittenFor) MarshalJSON() ([]byte, error) { return json.Marshal(f.carrier) }

// UnmarshalJSON reads the carrier an actor crossed the wire with, stated by the
// node that built the actor.
func (f *WrittenFor) UnmarshalJSON(b []byte) error {
	var w *WorkingFor
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("types: decode whom a write was made for: %w", err)
	}
	*f = WrittenFor{stated: true, carrier: w}
	return nil
}

// IsZero reports a WrittenFor that carries nobody, stated or not, which is
// what the encoder asks of a field tagged `omitzero`: on the wire nobody is an
// absent key, and a decoded actor reads one as stated ([WrittenFor.Received]).
// It is NOT the zero value's test, which [WrittenFor.Stated] is.
func (f WrittenFor) IsZero() bool { return f.carrier == nil }

// Received is f as an actor decoded off the wire carries it: stated, whatever
// arrived, since a node sends no actor that states nothing (each actor type's
// MarshalJSON refuses one), and an absent one carrying nobody, which is what
// an older node sends too: it knows no carrier. Every actor type that crosses
// the wire calls it from its own UnmarshalJSON, so a data node never refuses an
// agent's write for a key its sender left out because it carried nobody.
func (f WrittenFor) Received() WrittenFor {
	f.stated = true
	return f
}

// Clone is w with apps of its own, and nil for nil.
func (w *WorkingFor) Clone() *WorkingFor {
	if w == nil {
		return nil
	}
	c := *w
	c.Apps = slices.Clone(w.Apps)
	return &c
}
