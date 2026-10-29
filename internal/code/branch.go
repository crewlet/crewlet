package code

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
)

// THE BRANCH A SEAT'S CODING RUN WORKS ON when its run_sandbox call names
// none: seats/<handle>/<the work item's key, spelled for a ref>, inside the
// seat's own namespace ([SeatNamespace]) — so every run on one work item works
// on ONE branch, and a second run on the item continues the first's pushed
// work — or seats/<handle>/run-<12 hex> for a turn charged to no work item.
//
// The key is the work ITEM's (types.WorkItem.Key: "ENG-412", "acme/api#88" —
// the label its tracker shows), never turnctx.Turn.WorkKey, which is the
// dedupe key over a turn's trigger events: a digest no person reads, different
// for two turns on one item.

// seatBranchDigestHex is how many hex digits of a key's SHA-256 a respelled
// key carries: 48 bits, so two keys that spell alike are two branches unless
// their digests collide in that prefix, and the suffix costs 13 bytes of a
// ref (the digits and the '-' before them).
const seatBranchDigestHex = 12

// seatBranchRunHex is how many hex digits of the run id the run form carries:
// the LAST 12 of a UUIDv7, its random tail. The first digits are its
// millisecond clock, which two runs a minute apart share.
const seatBranchRunHex = 12

// SeatBranch is the branch a seat's coding run works on when its run_sandbox
// call names none: seats/<handle>/<workKey, spelled for a ref> where the turn
// is charged to a work item with a key, else seats/<handle>/run-<the last 12
// hex digits of runID>. Always a branch [gitref.ValidBranch] accepts, inside
// [SeatNamespace](handle).
//
// THE KEY IS SPELLED FOR A REF: every byte outside [A-Za-z0-9_-] becomes '-',
// runs of '-' collapse to one, and '-' is trimmed from both ends. Where that
// changed the key at all — or where the unchanged spelling already ends in '-'
// and 12 lowercase hex digits, the shape a digest takes, so no unchanged key
// reads as a respelled one — '-' and the first 12 hex digits of SHA-256(key)
// are appended: two keys that spell alike ("acme/api#88", "acme-api-88") are
// two branches, injective up to a 48-bit SHA-256 prefix collision. A spelling
// past what the 255-byte ref leaves ([gitref.MaxRefNameBytes], less
// refs/heads/seats/, the handle and its '/') is cut, before the digest, at the
// room the digest leaves, and takes the digest because the cut changed it. A
// key that spells to nothing ("###") takes the run form. CASE IS KEPT: a
// tracker's keys are unique as written, and folding them would make two items
// one branch on every host.
//
// ErrInvalid for a handle [org.ValidHandle] refuses, naming it, and for a run
// id that is no UUID where the run form is needed. A seat's handle is at most
// 64 bytes, so the run form always fits and a key keeps at least 173 bytes.
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

// spellForRef is key with every byte outside [A-Za-z0-9_-] written '-', runs
// of '-' collapsed to one, and '-' trimmed from both ends: a component every
// ref grammar takes, since it holds no '.', '/', control byte or space.
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
// [seatBranchDigestHex] lowercase hex digits.
func digestShaped(s string) bool {
	if len(s) < seatBranchDigestHex+1 || s[len(s)-seatBranchDigestHex-1] != '-' {
		return false
	}
	for i := len(s) - seatBranchDigestHex; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
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
