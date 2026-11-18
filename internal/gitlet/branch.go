package gitlet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
)

// THE BRANCH A SEAT'S EXECUTOR WORKS ON ([SeatBranch]) is one per work item,
// so a second run on the item continues the first's pushed work. The key is
// the work ITEM's (types.WorkItem.Key, the label its tracker shows), never
// turnctx.Turn.WorkKey: that dedupes trigger events and differs for two turns
// on one item.

// seatBranchDigestHex is the hex digits of SHA-256 a respelled key carries:
// 48 bits, at 13 bytes of the ref with its '-'.
const seatBranchDigestHex = 12

// seatBranchRunHex is the run form's digits: the LAST 12 of a UUIDv7, its
// random tail, since the leading millisecond clock is shared by nearby runs.
const seatBranchRunHex = 12

// SeatBranch is the branch a seat's executor works on when code_checkout names
// none: seats/<handle>/<workKey spelled for a ref>, else seats/<handle>/run-<the
// last 12 hex digits of runID>; always [gitref.ValidBranch] in [SeatNamespace].
// A spelling that changed the key, is cut (before the digest, to the room it
// leaves in [gitref.MaxRefNameBytes]) or is [digestShaped] takes '-' and
// [keyDigest]: "acme/api#88" and "acme-api-88" stay two branches. A key that
// spells to nothing takes the run form. Case is kept: tracker keys are unique
// as written, and folding would merge two items on every host.
//
// ErrInvalid for a handle [org.ValidHandle] refuses, and for a non-UUID run id
// where the run form is needed. A handle is at most 64 bytes, so the run form
// always fits and a key keeps at least 173 bytes.
func SeatBranch(handle, workKey, runID string) (string, error) {
	if !org.ValidHandle(handle) {
		return "", fmt.Errorf("%w: seat handle %q is not a seat's handle", ErrInvalid, handle)
	}
	namespace := strings.TrimPrefix(SeatNamespace(handle), branchPrefix)
	room := gitref.MaxRefNameBytes - len(SeatNamespace(handle))
	if spelled := spellForRef(workKey); spelled != "" {
		if spelled == workKey && !digestShaped(spelled) && len(spelled) <= room {
			return namespace + spelled, nil
		}
		cut := strings.TrimRight(spelled[:min(len(spelled), room-1-seatBranchDigestHex)], "-")
		return namespace + cut + "-" + keyDigest(workKey), nil
	}
	id, err := uuid.Parse(runID)
	if err != nil {
		return "", fmt.Errorf("%w: run id %q is not a UUID, and the turn names no work item to name its "+
			"branch by", ErrInvalid, runID)
	}
	canonical := id.String()
	return namespace + "run-" + canonical[len(canonical)-seatBranchRunHex:], nil
}

// spellForRef writes every byte outside [A-Za-z0-9_-] as '-', collapses runs
// of '-' and trims them from both ends: a component every ref grammar takes.
func spellForRef(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	dash := false
	for i := 0; i < len(key); i++ {
		c := key[i]
		if keyByte(c) && c != '-' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteByte(c)
			continue
		}
		dash = true
	}
	return b.String()
}

// keyByte reports a byte a spelled key keeps as it is.
func keyByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-'
}

// digestShaped reports a spelling that ends as a respelled key's does: '-' and
// [seatBranchDigestHex] lowercase hex digits. Such a key takes the digest even
// unchanged, so no kept key reads as a respelled one.
func digestShaped(s string) bool {
	if len(s) < seatBranchDigestHex+1 || s[len(s)-seatBranchDigestHex-1] != '-' {
		return false
	}
	for i := len(s) - seatBranchDigestHex; i < len(s); i++ {
		switch c := s[i]; {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// keyDigest is the first [seatBranchDigestHex] hex digits of key's SHA-256.
func keyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:seatBranchDigestHex]
}
