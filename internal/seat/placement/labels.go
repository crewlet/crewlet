package placement

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLabelKeyBytes bounds a node label key.
//
// SIXTY-THREE: Kubernetes' limit on a label name — the length of a DNS label —
// which is the grammar an operator writing node.labels most likely already
// types, and a bound on what every presence lease repeats once per key.
const MaxLabelKeyBytes = 63

// ErrLabelKey is a string that is not a node label key.
var ErrLabelKey = errors.New("not a node label key")

// CheckLabelKey refuses a string that cannot be a node label key: empty, longer
// than [MaxLabelKeyBytes], not UTF-8, or holding whitespace or any character
// that does not print.
//
// ONE GRAMMAR, here, because a key is written in more than one place and
// matched EXACTLY between them: node.labels on every node, and the selector a
// role's placement names. Two validators that disagree never raise — a key one
// accepts and the other refuses is a label nothing can match.
// Whitespace is refused rather than trimmed because it is the one difference a
// reader cannot see: "zone" and "zone " look identical in both files and
// match nothing.
func CheckLabelKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: it is empty", ErrLabelKey)
	case len(key) > MaxLabelKeyBytes:
		return fmt.Errorf("%w: %q is %d bytes, at most %d", ErrLabelKey, key, len(key),
			MaxLabelKeyBytes)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: %q is not UTF-8", ErrLabelKey, key)
	}
	for _, r := range key {
		if unicode.IsSpace(r) || !unicode.IsGraphic(r) {
			return fmt.Errorf("%w: %q holds %U, and a key is matched exactly, so "+
				"it may hold no whitespace or unprintable character", ErrLabelKey, key, r)
		}
	}
	return nil
}

// MaxLabels bounds how many labels one node carries, and so how many one
// selector can name.
//
// SIXTEEN. The facts a role's placement selects on are a deployment's
// topology — Kubernetes' well-known labels name six (region, zone, hostname,
// instance type, architecture, operating system) — and a company's own (a
// pool, a seat, a GPU, a tier): ten in all, and sixteen leaves room. It is a
// bound because every label rides the node's presence lease, which is
// rewritten on every heartbeat and held to coord.MaxLeaseMetaBytes: sixteen
// labels at their widest are about half of that bound (internal/engine,
// TestThePresenceLeaseAtItsWorstFitsItsBound).
const MaxLabels = 16

// MaxLabelValueBytes bounds a node label value, in bytes.
//
// SIXTY-FOUR, a seat handle's own width (iam.MaxLogin): the longest value the
// engine itself has a reason to carry is a seat handle, which
// crewlet-operator writes as a per-seat pool's `crewlet.ai/seat` label, and
// Kubernetes' own 63-byte label values fit under it. TestALabelValueHoldsASeatHandle
// holds it to at least iam.MaxLogin.
const MaxLabelValueBytes = 64

// ErrLabelValue is a string that is not a node label value.
var ErrLabelValue = errors.New("not a node label value")

// CheckLabelValue refuses a string that cannot be a node label value: longer
// than [MaxLabelValueBytes], not UTF-8, holding a character that does not
// print (the ASCII space prints; a tab, a control character or a line
// separator does not), or with whitespace around it. The empty string is a
// value.
//
// ONE GRAMMAR, for [CheckLabelKey]'s reason: a value is matched EXACTLY
// between node.labels and a role's placement selector, so a value one side
// accepts and the other refuses is a label nothing can match. Whitespace
// AROUND a value is refused rather than trimmed for the same reason a key's
// is: " eu" and "eu" look identical in a file. Tier A trims every string as it
// loads, so a node's own values never carry any; a selector, written in Tier
// B, keeps what its author wrote.
func CheckLabelValue(value string) error {
	switch {
	case len(value) > MaxLabelValueBytes:
		return fmt.Errorf("%w: %q is %d bytes, at most %d", ErrLabelValue, value, len(value),
			MaxLabelValueBytes)
	case !utf8.ValidString(value):
		return fmt.Errorf("%w: %q is not UTF-8", ErrLabelValue, value)
	case strings.TrimSpace(value) != value:
		return fmt.Errorf("%w: %q has whitespace around it, and a value is matched exactly: "+
			"write %q", ErrLabelValue, value, strings.TrimSpace(value))
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: %q holds %U, and a value is matched exactly, so it may "+
				"hold no character that does not print", ErrLabelValue, value, r)
		}
	}
	return nil
}
