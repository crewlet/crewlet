package sandbox

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A TAKE THAT KEEPS ITS MEDIA APART FRAMES ITS BODY ([SessionTake.MediaApart]),
// so the spool stores each large payload once, apart from the session's text
// object (ADR-0073's "a session's large media is kept apart"): the archive is
// gzip members, each payload one of its own, and the body says which is which.
// The archive whole, every member in order, is what the take digests and its
// box restores.
const (
	// BodyWhole leads a body whose payloads all stay inline: the archive
	// follows as it is kept, the session's object byte for byte.
	BodyWhole byte = 'W'

	// BodySpliced leads a body of frames: text, as executorkeep.FrameText
	// frames; a payload's first member, as [FrameMember] frames ended by
	// [FrameMemberEnd] and the member's SHA-256; and a repeat, as
	// executorkeep.FrameMedia and its first member's SHA-256.
	BodySpliced byte = 'S'

	// FrameMember is a piece of a payload's member: FrameMember, a uvarint
	// n, then n bytes. FrameMemberEnd ends the member: FrameMemberEnd, then
	// the 32-byte SHA-256 of all its pieces.
	FrameMember    byte = 'M'
	FrameMemberEnd byte = 'E'
)

// ErrCarryGone is the executor log's refusal of a capture carrying a media
// object its turn's row, or the base that row holds, no longer names: a record
// refused whole, which its keep makes again uploading that media
// ([Coordinator.keepCapture]). The engine wraps the log's own refusal in it,
// since this package links no state log.
var ErrCarryGone = errors.New("sandbox: a media object the capture carries is no longer named by its turn")

// errBodyFrames is a body framed otherwise than a take frames one.
var errBodyFrames = errors.New("sandbox: the take's body is framed otherwise than a take frames it")

// TakenText reads a framed body ([BodySpliced], [BodyWhole]) as the session's
// text object: the archive as it is for a whole body, and for a spliced one
// its text and an executorkeep.FrameMedia frame where each payload stands, the
// frames executordomain's SessionStore.Open splices from. Each first member
// is handed to media as it comes, read to its end before its frame stands,
// and must answer the SHA-256 it read, in lowercase hex. Made by
// [SplitTakenBody].
type TakenText struct {
	r     *bufio.Reader
	media func(member io.Reader) (digest string, err error)

	began, spliced bool
	// textLeft is what the body's text frame being read still holds; out is
	// what is owed the reader before the next frame: a media frame, or a
	// text frame staged for a read too short for its head.
	textLeft uint64
	out      []byte
	owed     [1 + executorkeep.MediaDigestBytes]byte
	stage    [64]byte
	firsts   map[[executorkeep.MediaDigestBytes]byte]bool
	err      error
}

// SplitTakenBody reads body, a take's framed body, as the session's text
// object ([TakenText]), handing each payload's first member to media.
func SplitTakenBody(body io.Reader, media func(member io.Reader) (digest string, err error)) *TakenText {
	return &TakenText{r: bufio.NewReaderSize(body, 64<<10), media: media,
		firsts: map[[executorkeep.MediaDigestBytes]byte]bool{}}
}

func (t *TakenText) Read(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	n, err := t.read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

func (t *TakenText) read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !t.began {
		tag, err := t.r.ReadByte()
		switch {
		case errors.Is(err, io.EOF):
			return 0, fmt.Errorf("%w: it ended before its lead", errBodyFrames)
		case err != nil:
			return 0, err
		case tag == BodySpliced:
			t.spliced = true
		case tag != BodyWhole:
			return 0, fmt.Errorf("%w: it is led by %q", errBodyFrames, tag)
		}
		t.began = true
	}
	if !t.spliced {
		return t.r.Read(p)
	}
	for {
		if len(t.out) > 0 {
			n := copy(p, t.out)
			t.out = t.out[n:]
			return n, nil
		}
		if t.textLeft > 0 {
			return t.text(p)
		}
		kind, err := t.r.ReadByte()
		if err != nil {
			return 0, err
		}
		switch kind {
		case executorkeep.FrameText:
			if t.textLeft, err = binary.ReadUvarint(t.r); err != nil {
				return 0, cutFrames(err)
			}
		case FrameMember:
			if err = t.member(); err != nil {
				return 0, err
			}
		case executorkeep.FrameMedia:
			var digest [executorkeep.MediaDigestBytes]byte
			if _, err = io.ReadFull(t.r, digest[:]); err != nil {
				return 0, cutFrames(err)
			}
			if !t.firsts[digest] {
				return 0, fmt.Errorf("%w: a repeat names media %x before its first", errBodyFrames, digest)
			}
			t.owe(digest)
		default:
			return 0, fmt.Errorf("%w: a frame of kind %q", errBodyFrames, kind)
		}
	}
}

// text answers the next of the body's text as one text frame, taking on every
// text frame after it that is already buffered, so the object holds a frame a
// read rather than a frame each write of the take's gzip.
func (t *TakenText) text(p []byte) (int, error) {
	const head = 1 + binary.MaxVarintLen64
	if len(p) <= head {
		staged, err := t.text(t.stage[:])
		n := copy(p, t.stage[:staged])
		t.out = t.stage[n:staged]
		return n, err
	}
	room, n := p[head:], 0
	for n < len(room) {
		if t.textLeft == 0 {
			next, err := t.r.Peek(1)
			if err != nil || next[0] != executorkeep.FrameText {
				break
			}
			_, _ = t.r.ReadByte()
			if t.textLeft, err = binary.ReadUvarint(t.r); err != nil {
				t.err = cutFrames(err)
				break
			}
			continue
		}
		k, err := t.r.Read(room[n : n+int(min(uint64(len(room)-n), t.textLeft))])
		n, t.textLeft = n+k, t.textLeft-uint64(k)
		if err != nil {
			t.err = cutFrames(err)
			break
		}
		if t.r.Buffered() == 0 {
			break
		}
	}
	if n == 0 && t.err != nil {
		return 0, t.err
	}
	var size [binary.MaxVarintLen64]byte
	w := binary.PutUvarint(size[:], uint64(n))
	at := head - 1 - w
	p[at] = executorkeep.FrameText
	copy(p[at+1:head], size[:w])
	return copy(p, p[at:head+n]), nil
}

// member hands the first member of a payload to media, the frame that began
// it read, and owes the reader the media frame standing in its place.
func (t *TakenText) member() error {
	m := &memberFrames{r: t.r}
	digest, err := t.media(m)
	switch {
	case err != nil:
		return err
	case !m.ended:
		return fmt.Errorf("%w: a media member was not read to its end", errBodyFrames)
	case digest != hex.EncodeToString(m.sum[:]):
		return fmt.Errorf("%w: media %x was read as %s", errBodyFrames, m.sum, digest)
	case t.firsts[m.sum]:
		return fmt.Errorf("%w: media %x is framed as a first twice, and a member is stored once", errBodyFrames,
			m.sum)
	}
	t.firsts[m.sum] = true
	t.owe(m.sum)
	return nil
}

// owe has the next read answer the media frame naming digest.
func (t *TakenText) owe(digest [executorkeep.MediaDigestBytes]byte) {
	t.owed[0] = executorkeep.FrameMedia
	copy(t.owed[1:], digest[:])
	t.out = t.owed[:]
}

// memberFrames reads one payload's first member out of a spliced body, from
// the length of the frame that began it to its end frame, whose digest it
// keeps.
type memberFrames struct {
	r       *bufio.Reader
	begun   bool
	left    uint64
	ended   bool
	sum     [executorkeep.MediaDigestBytes]byte
	failure error
}

func (m *memberFrames) Read(p []byte) (int, error) {
	for {
		switch {
		case m.failure != nil:
			return 0, m.failure
		case m.ended:
			return 0, io.EOF
		case m.left > 0:
			k, err := m.r.Read(p[:min(uint64(len(p)), m.left)])
			m.left -= uint64(k)
			if err != nil {
				m.failure = cutFrames(err)
			}
			if k == 0 {
				continue
			}
			return k, nil
		case m.begun:
			kind, err := m.r.ReadByte()
			if err != nil {
				m.failure = cutFrames(err)
				continue
			}
			switch kind {
			case FrameMemberEnd:
				if _, err = io.ReadFull(m.r, m.sum[:]); err != nil {
					m.failure = cutFrames(err)
					continue
				}
				m.ended = true
				continue
			case FrameMember:
			default:
				m.failure = fmt.Errorf("%w: a frame of kind %q inside a media member", errBodyFrames, kind)
				continue
			}
		}
		m.begun = true
		var err error
		if m.left, err = binary.ReadUvarint(m.r); err != nil {
			m.failure = cutFrames(err)
		}
	}
}

// cutFrames is err, a read inside a frame, as the body's fault where the body
// ended there.
func cutFrames(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: it ends inside a frame", errBodyFrames)
	}
	return err
}
