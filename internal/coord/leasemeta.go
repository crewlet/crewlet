package coord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// MaxLeaseMetaBytes is the largest Meta a lease carries, as the store encodes
// it ([LeaseMetaBytes]): 8 KiB. Every backend refuses a claim whose Meta is
// larger before anything is sent ([HoldLeaseMeta]).
//
// # Why this figure
//
// The one lease that carries a Meta is a node's presence, and it is
// re-sent on EVERY heartbeat: a renewal waits on its whole record crossing the
// node's link, so a Meta nothing bounded is a renewal nothing bounds. Its
// worst case is computed from the engine's own limits — the roles, at most
// sixteen labels of a 63-byte key and a 64-byte value
// (internal/seat/placement's MaxLabels, MaxLabelKeyBytes and
// MaxLabelValueBytes, which Tier A refuses past), the held count, the
// withdrawal, a Gitlet node's budget, and the status core with the build cut
// to [MaxBuildBytes] and one alarm mark per kind of the alarm table — each at
// its most expensive encoding. That is under 8 KiB, and this is it rounded up
// to the next kibibyte: internal/engine's
// TestThePresenceLeaseAtItsWorstFitsItsBound builds that worst case through
// the seat host's own assembler and holds this constant equal to it rounded
// up, so a new alarm kind or a new core field that pushes it past moves this
// figure deliberately rather than failing a node's presence at run time.
//
// A record adds its envelope beside it — the resource, the owner, the epoch,
// the TTL, the protocol, the tenure's start and the hint — some four hundred
// bytes: a presence record is at most about 8.6 KB, a quarter second at the
// slowest pace the engine allows a link (internal/objstore's MiBPace).
//
// A NODE OVER IT LOSES ITS STATUS, NEVER ITS PRESENCE: the seat host measures
// its own Meta before it claims and, past this, publishes the placement half
// alone with an error naming the size (internal/seat, presenceMeta). The
// refusal here is the backstop that holds every other caller to the same
// bound.
const MaxLeaseMetaBytes = 8 << 10

// MaxBuildBytes is the most of a node's build name its status carries
// ([NodeStatus.Build]): 64 bytes. Every form internal/version writes is under
// fifty (a dirty build past a tag, a pseudo-version with "+dirty", is 42), and the cut is what makes the presence's bound TOTAL — a build name
// is whatever its linker was handed, and the one field of the status no
// table bounds.
const MaxBuildBytes = 64

// EncodeLeaseRecord is how a lease's record, and so its Meta, is written to
// the store: JSON, without the HTML escapes encoding/json applies by default.
//
// ONE ENCODING, which [LeaseMetaBytes] measures by and the KV backend writes
// by, so the bound is a bound on the bytes that cross. Without HTML escaping
// because a lease record is no HTML: `<`, `>` and `&` would each cost six
// bytes for nothing, and the worst case the bound is derived from would
// triple.
func EncodeLeaseRecord(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode ends every value with a newline, which no record carries.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// LeaseMetaBytes is how many bytes meta takes in a lease's record, encoded as
// the store encodes it ([EncodeLeaseRecord]); zero for an empty Meta, which a
// record leaves out. An error is a Meta JSON cannot express, which no store
// could write either.
func LeaseMetaBytes(meta map[string]any) (int, error) {
	if len(meta) == 0 {
		return 0, nil
	}
	raw, err := EncodeLeaseRecord(meta)
	if err != nil {
		return 0, fmt.Errorf("coord: a lease's meta is not encodable: %w", err)
	}
	return len(raw), nil
}

// HoldLeaseMeta refuses a claim of resource whose meta is past
// [MaxLeaseMetaBytes], with [ErrValueTooLarge] naming the resource, the size
// and the bound; nil for one that fits. The one rule every backend refuses a
// lease's Meta by, before anything is sent.
//
// ErrValueTooLarge rather than a sentinel of its own, for the reason that
// error states: it is a definite answer about the write's size, refused before
// anything reached the store, which no retry changes — the same fact a value
// past what a key holds is.
func HoldLeaseMeta(resource string, meta map[string]any) error {
	size, err := LeaseMetaBytes(meta)
	if err != nil {
		return err
	}
	if size <= MaxLeaseMetaBytes {
		return nil
	}
	return fmt.Errorf("%w: a lease meta of %d bytes for %s, past the %d a lease carries — a "+
		"holder re-sends it on every renewal, so its size is the renewal's", ErrValueTooLarge,
		size, resource, MaxLeaseMetaBytes)
}

// cutBuild is a build name held to [MaxBuildBytes], cut at the last whole
// character that fits so the cut never leaves half of one.
func cutBuild(build string) string {
	if len(build) <= MaxBuildBytes {
		return build
	}
	end := MaxBuildBytes
	for end > 0 && !utf8.RuneStart(build[end]) {
		end--
	}
	return build[:end]
}
