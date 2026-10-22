package signedask

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// # One way: a frame nobody answers
//
// A request and its reply are a pair: the request carries an id the reply is
// bound to, and a serving node remembers the ids it took. A BROADCAST has no
// reply and no single receiver (the queue's Broadcast hands it to every node
// listening), so neither binding applies. What still applies is the reason
// for signing at all: the broker authenticates nothing, and a frame that names
// a person causes a display of that person. A forged presence frame would put
// a colleague in a room they are not in.
//
// So a one-way frame is sealed with the same frame the state log signs with,
// in a key domain of its own ([OneWay.Domain], the name it was built with),
// and its body carries the instant it EXPIRES, inside the MAC: a captured
// frame replayed later is refused once that instant has passed, and a sender
// sets it to the short validity its channel is worth. No id is remembered,
// because a broadcast is meant to be heard by every node and a repeat of one
// says only what the original said, for no longer than the original could.

// Message is a one-way frame's body as the seal binds it.
type Message interface {
	// Expires is the instant after which no node may believe the frame.
	// A FIELD OF THE BODY, so it is under the signature: there is no part
	// of a frame a sender can set that the MAC does not cover.
	Expires() time.Time
}

// OneWay seals and opens one channel's one-way frames for one node.
//
// ONE VALUE FOR BOTH HALVES, built once from the node's keyring, because every
// node that sends a channel's frames also hears its peers'. Immutable and safe
// for concurrent use.
type OneWay[M Message] struct {
	domain string
	seal   *statelog.Signer
	open   *statelog.Verifier
}

// NewOneWay builds a node's half of the one-way channel name from the fleet
// keyring, refusing a keyring that names no active key or holds none, for
// [New]'s reason.
func NewOneWay[M Message](name string, ring statelog.Keyring) (*OneWay[M], error) {
	if name == "" {
		return nil, errors.New("signedask: a one-way channel needs a name: it is the key " +
			"domain its frames are sealed under")
	}
	w := &OneWay[M]{domain: name}
	var err error
	if w.seal, err = statelog.NewSigner(name, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: seal the frames this node sends: %w", name, err)
	}
	if w.open, err = statelog.NewVerifier(name, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: open the frames this node hears: %w", name, err)
	}
	return w, nil
}

// Domain is the key domain this channel seals under, for the gate that holds
// every channel's domains apart from every other's and from every state log's.
func (w *OneWay[M]) Domain() string { return w.domain }

// HeldKeys is every key id this node opens frames under, sorted.
func (w *OneWay[M]) HeldKeys() []string { return w.open.KeyIDs() }

// Seal encodes and signs one frame.
//
// IT REFUSES A FRAME THAT NEVER EXPIRES, because every node would refuse it as
// stale, and a sender that built one is a bug that should fail here rather
// than as silence on every peer.
func (w *OneWay[M]) Seal(m M) ([]byte, error) {
	if m.Expires().IsZero() {
		return nil, errors.New("signedask: a one-way frame is sealed with the instant it " +
			"expires, or nothing bounds how long a captured copy could be replayed")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("signedask: encode the frame: %w", err)
	}
	return w.seal.Seal(raw), nil
}

// Open verifies one frame and decodes it, judging its expiry on now.
//
// THE DISPOSITION IS ONE OF FIVE, the request's own set ([Disposition]), and
// err is nil exactly when it is [Admitted]; otherwise err says why the frame
// must not be believed. The message comes back only where the fleet sealed it:
// for [Admitted], and for [Stale] (a frame past its expiry), where it is
// VERIFIED and is handed back only so a log line can say what was refused.
// [Tampered], [KeyUnknown] and [Undecodable] answer the zero message: a frame
// nothing vouches for is believed in nothing, not even who it names.
func (w *OneWay[M]) Open(sealed []byte, now time.Time) (M, Disposition, error) {
	var zero M
	body, verdict := w.open.Open(sealed)
	switch verdict {
	case statelog.Tampered:
		return zero, Tampered, errors.New("signedask: the frame is not sealed by this fleet")
	case statelog.KeyUnknown:
		return zero, KeyUnknown, fmt.Errorf("signedask: the frame is sealed under a key "+
			"this node does not hold (it holds %s); add the key to secrets.keys", words.And(w.HeldKeys()))
	}
	var m M
	if err := json.Unmarshal(body, &m); err != nil {
		return zero, Undecodable, fmt.Errorf("signedask: the fleet sealed a frame this "+
			"build cannot decode: %w", err)
	}
	switch expires := m.Expires(); {
	case expires.IsZero():
		return m, Stale, errors.New("signedask: the frame names no expiry, so nothing " +
			"bounds how long a copy of it could be replayed")
	case !now.Before(expires):
		return m, Stale, fmt.Errorf("signedask: the frame expired at %s",
			expires.UTC().Format(time.RFC3339Nano))
	}
	return m, Admitted, nil
}
