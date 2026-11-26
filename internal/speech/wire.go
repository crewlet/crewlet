package speech

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// The worker protocol: frames over the worker's stdin (from the engine) and
// stdout (to it). A frame is a kind byte, a big-endian stream id and payload
// length, and the payload. Both ends are this binary, so it carries no
// version.
const (
	// From the engine.
	frameOpen   byte = 1 // start stream id
	frameAudio  byte = 2 // PCM for stream id
	frameFinish byte = 3 // no more audio for stream id
	frameClose  byte = 4 // abandon stream id

	// From the worker.
	frameReady   byte = 10 // the model is loaded (id 0)
	framePartial byte = 11 // provisional text for stream id
	frameFinal   byte = 12 // a punctuated segment for stream id
	frameDone    byte = 13 // stream id delivered its last final
	frameFailed  byte = 14 // the worker could not load (id 0); payload is the cause
)

// frameHeader is a frame's fixed prefix: kind, id, length.
const frameHeader = 1 + 4 + 4

// maxFramePayload bounds what either end reads for one frame: well past a
// second of audio or a segment of text, and short of a hostile length.
const maxFramePayload = 1 << 20

type frame struct {
	kind    byte
	id      uint32
	payload []byte
}

func writeFrame(w io.Writer, f frame) error {
	buf := make([]byte, frameHeader+len(f.payload))
	buf[0] = f.kind
	binary.BigEndian.PutUint32(buf[1:], f.id)
	binary.BigEndian.PutUint32(buf[5:], uint32(len(f.payload))) //nolint:gosec // bounded by maxFramePayload at every writer
	copy(buf[frameHeader:], f.payload)
	_, err := w.Write(buf)
	return err
}

func readFrame(r *bufio.Reader) (frame, error) {
	var head [frameHeader]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(head[5:])
	if n > maxFramePayload {
		return frame{}, fmt.Errorf("speech: a worker frame of %d bytes is over the %d limit", n, maxFramePayload)
	}
	f := frame{kind: head[0], id: binary.BigEndian.Uint32(head[1:]), payload: make([]byte, n)}
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, err
	}
	return f, nil
}
