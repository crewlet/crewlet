package executorkeep

import (
	"encoding/hex"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A KEPT SESSION'S LARGE MEDIA IS STORED APART (ADR-0073's amendment "a
// session's large media is kept apart"): each payload as a gzip member of its
// own, one object per distinct member, and the session's text object a
// container of frames naming them. The take writes the frames and the restore
// splices them, so both read the format here.
const (
	// FrameText is a text frame: FrameText, a uvarint n, then n bytes of the
	// archive as the take wrote them.
	FrameText byte = 'T'

	// FrameMedia is a media frame: FrameMedia, then the 32-byte SHA-256 of a
	// media member's bytes, which the restore splices in its place whole.
	FrameMedia byte = 'R'
)

// MediaDigestBytes is the length of a media member's digest in a frame, and
// twice it of its spelling on a row: a SHA-256.
const MediaDigestBytes = 32

// MediaObject is one media object a kept session names: the SHA-256 of the
// member it holds, lowercase hex ([ValidMediaDigest]), the sealed object, and
// the keyring key that sealed it.
type MediaObject struct {
	Digest     string
	Object     objstore.Object
	ContentKey string
}

// MediaSpool is a media member sealed for an object the executor log's
// session store minted, waiting for its upload, and the digest of the member.
type MediaSpool struct {
	SealedSpool
	Digest string
}

// ValidMediaDigest reports whether s spells a media member's digest: a
// SHA-256 in lowercase hex, the one spelling a row and a frame agree on.
func ValidMediaDigest(s string) bool {
	if len(s) != 2*MediaDigestBytes {
		return false
	}
	raw, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(raw) == s
}
