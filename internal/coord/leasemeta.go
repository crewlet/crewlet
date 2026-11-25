package coord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// MaxLeaseMetaBytes is the largest Meta a lease carries, as the store encodes
// it ([LeaseMetaBytes]): 8 KiB, refused before anything is sent ([HoldLeaseMeta]).
//
// A Meta is re-sent on EVERY renewal, so an unbounded one is an unbounded
// renewal. Two classes carry one: a node's presence, whose worst case (the label
// limits of internal/seat/placement, [MaxBuildBytes], one alarm mark per kind)
// this is, rounded up to the next KiB (internal/engine's
// TestThePresenceLeaseAtItsWorstFitsItsBound), and a browser control's record,
// whose worst case fits under it. A record crosses in a quarter second at
// internal/objstore's MiBPace. A NODE OVER IT LOSES ITS STATUS, NEVER ITS
// PRESENCE: the seat host publishes the placement half alone (internal/seat,
// presenceMeta), and this refusal is the backstop for every other caller.
const MaxLeaseMetaBytes = 8 << 10

// MaxBuildBytes is the most of a node's build name its status carries
// ([NodeStatus.Build]): 64 bytes, above every form internal/version writes
// (at most 42). The cut makes the presence's bound TOTAL: a build name is
// whatever the linker was handed, the one status field no table bounds.
const MaxBuildBytes = 64

// EncodeLeaseRecord is how a lease's record, and so its Meta, is written to
// the store: JSON without encoding/json's default HTML escapes.
//
// ONE ENCODING, which [LeaseMetaBytes] measures by and the KV backend writes
// by, so the bound is on the bytes that cross. `<`, `>` and `&` would cost six
// bytes each and triple the worst case the bound is derived from.
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
// lease's Meta by, before anything is sent: [ErrValueTooLarge] because it is a
// definite answer about the write's size, which no retry changes.
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

// MaxLeaseNameBytes is the most each name a lease's record holds may take as
// the store encodes it ([LeaseNameBytes]): its resource, its owner and its
// preferred node, each refused past it before anything is sent
// ([HoldLeaseName]). The widest a writer names is a browser control's owner,
// 118 bytes ([BrowserControlOwner.Key]), and an OAuth renewal's resource, a
// grant's name behind its class, 90 (internal/engine's
// TestEveryLeaseThisBuildTakesNamesItselfWithinItsRecord builds each, and
// internal/worklet's own test its walks' claims); a name
// a configuration writes at any length is a digest there, as a growth act's.
// 256 is twice the widest with room, and three names at it beside
// [MaxLeaseMetaBytes] keep the record a small value read at the term
// ([SmallValueBytes]; internal/coord/kv's widest-record test counts it).
const MaxLeaseNameBytes = 256

// LeaseNameBytes is how many bytes name takes in a lease's record, encoded as
// the store encodes it ([EncodeLeaseRecord]), its quotes left out: a control
// or invalid byte takes six.
func LeaseNameBytes(name string) int {
	// A string always encodes.
	raw, _ := EncodeLeaseRecord(name)
	return len(raw) - len(`""`)
}

// HoldLeaseName refuses a lease's name, field naming which (resource, owner,
// preferred), past [MaxLeaseNameBytes] as stored, with [ErrValueTooLarge]
// naming its size: every backend refuses it before anything is sent, since
// every renewal re-sends the record whole.
func HoldLeaseName(field, name string) error {
	if size := LeaseNameBytes(name); size > MaxLeaseNameBytes {
		return fmt.Errorf("%w: a lease's %s of %d bytes as stored, past the %d a lease's record "+
			"holds for it: name it shorter", ErrValueTooLarge, field, size, MaxLeaseNameBytes)
	}
	return nil
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
