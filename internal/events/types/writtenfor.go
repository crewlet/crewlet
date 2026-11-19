package types

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// WrittenFor is whom a write was made for, as the actor that makes it states
// it (ADR-0109): whoever the turn that wrote it works for ([WorkingFor]),
// carried on the record the write leaves (an item's creation and every move of
// its assignee, and any other record only where its domain proves an agent may
// wake for it), so the turn it wakes or hands work to works for them too, never
// for the writing seat, which holds every app. A colleague's ask carries the
// same ([A2ARequest]); this is the hop through Worklet, Doclet, Chatlet and
// Gitlet writes. The builder states it (see [WrittenFor.For]); the carrier is
// the engine's, composed from the call's own bound ceiling
// (internal/agent/builtin), never from tool arguments or anything a model
// wrote. Its wire rule is on [WrittenFor.Received].
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

// For is whom a write by an actor of kind was made for, or why it cannot make
// it: an agent's actor stating nothing is refused ([ErrWrittenForUnstated]), as
// is a carrier on a person's, credential's or the engine's write
// ([ErrWrittenForNotAnAgents]).
//
// THE ZERO VALUE STATES NOTHING. An agent writes only in a turn, which works
// for somebody or its own seat, so an actor stating neither came from a builder
// that forgot, and its write would wake whoever it reaches for with every app.
// Every domain asks this before building a record, and internal/estate before
// the write is asked of any node. People, credentials and the engine write for
// nobody but themselves, so for them the zero value is the answer.
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

// MarshalJSONTo writes the carrier, or null, through the caller's own encoder
// and so under its options: the estate's rule for what an operation takes
// reaches inside the carrier (a member at its zero is left off the wire; one
// the serving build does not declare refuses the request, named at its path,
// internal/estate's wirerule.go). A MarshalJSON would re-encode bytes under
// encoding/json's defaults, which hold neither half. An actor's field is
// omitted where it carries nobody ([WrittenFor.IsZero], `omitzero`), byte for
// byte as a build without the field sends it; a tracker actor's provenance then
// crosses as `{}`, which that build declares and reads as empty.
func (f WrittenFor) MarshalJSONTo(enc *jsontext.Encoder) error {
	return jsonv2.MarshalEncode(enc, f.carrier)
}

// UnmarshalJSONFrom reads the carrier an actor crossed the wire with, stated by
// the node that built the actor, through the caller's own decoder and so under
// its options: a member the serving build does not declare refuses the request
// where it is, which an UnmarshalJSON would drop without a word.
func (f *WrittenFor) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var w *WorkingFor
	if err := jsonv2.UnmarshalDecode(dec, &w); err != nil {
		// UNWRAPPED, against the rule that an error wraps with %w: json/v2
		// hands its caller a *jsonv2.SemanticError a method returns as it
		// is, and wraps any other in a second one, typed as WrittenFor and
		// placed wherever the decoder stopped. The estate's wire names a
		// member it refuses from the first SemanticError it finds (its
		// decodeInput), so returned bare that is the decoder's own, at the
		// member's path as the decoder recorded it, never a position
		// json/v2 inferred, which is the member's only because that is
		// where the decoder stops.
		return err
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
// arrived, and an absent carrier is nobody, as an older node sends too (it
// knows no carrier). A data node cannot tell that absence from a current
// node's stated one, so it never refuses an agent's write for a key its sender
// left out; the node that builds the actor refuses an unstated write BEFORE IT
// IS ASKED ANYWHERE, its own process included (internal/estate's
// op.refusedBeforeAsking and the router's check), so every node answers alike.
// Every wire-crossing actor type calls it from its own UnmarshalJSONFrom,
// whatever members arrived (internal/estate's Actor, even with no provenance,
// and each Gitlet write request: internal/gitlet's writtenfor.go).
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
