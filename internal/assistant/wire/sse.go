package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// KeepaliveComment is the comment line a quiet stream writes every
// [KeepaliveSeconds]. A client ignores it; a proxy sees bytes.
const KeepaliveComment = ": keepalive\n\n"

// EventID is a frame's SSE id: `<turn_id>.<seq>`, which a client sends back as
// Last-Event-ID to be replayed everything after it.
func EventID(turn uuid.UUID, seq uint64) string {
	return turn.String() + "." + strconv.FormatUint(seq, 10)
}

// ParseEventID reads a Last-Event-ID: the turn and the last seq drawn. An id
// in any other shape is an error, never a turn of the zero uuid at seq zero,
// which would replay a whole stream to a client that asked for a point in it.
func ParseEventID(s string) (uuid.UUID, uint64, error) {
	turn, seq, ok := strings.Cut(s, ".")
	if !ok {
		return uuid.Nil, 0, fmt.Errorf("%w: event id %q is not <turn_id>.<seq>", ErrInvalid, s)
	}
	id, err := uuid.Parse(turn)
	if err != nil || id == uuid.Nil || id.String() != turn {
		return uuid.Nil, 0, fmt.Errorf("%w: event id %q names no turn in canonical form",
			ErrInvalid, s)
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != seq {
		return uuid.Nil, 0, fmt.Errorf("%w: event id %q names no seq from 1", ErrInvalid, s)
	}
	return id, n, nil
}

// Encode writes one frame as one SSE event: its id, its kind as the event
// name, and the frame as one line of JSON.
//
// THE FRAME IS VALIDATED FIRST, so a stream never carries a frame its client
// would refuse: a frame that fails here is a fault in the code that built it,
// and the stream ends rather than send it.
func Encode(w io.Writer, f Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("wire: encode a %s frame: %w", f.Kind, err)
	}
	if len(data) > FrameMaxBytes {
		return fmt.Errorf("%w: a %s frame encodes to %d bytes, over %d", ErrInvalid, f.Kind,
			len(data), FrameMaxBytes)
	}
	var b bytes.Buffer
	b.Grow(len(data) + 96)
	b.WriteString("id: ")
	b.WriteString(EventID(f.TurnID, f.Seq))
	b.WriteString("\nevent: ")
	b.WriteString(string(f.Kind))
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	_, err = w.Write(b.Bytes())
	return err
}

// Decoder reads frames from a stream [Encode] wrote, skipping comments.
type Decoder struct {
	lines *bufio.Scanner
}

// NewDecoder reads frames from r.
func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), FrameMaxBytes+64<<10)
	return &Decoder{lines: s}
}

// Next is the next frame, [io.EOF] at a clean end, and an error for an event
// whose id, name and frame disagree or whose frame does not validate.
func (d *Decoder) Next() (Frame, error) {
	var id, event, data string
	var fields int
	for d.lines.Scan() {
		line := d.lines.Text()
		switch {
		case line == "":
			if fields == 0 {
				continue
			}
			return decodeEvent(id, event, data)
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if data != "" {
				return Frame{}, fmt.Errorf("%w: event %s carries data on two lines; "+
					"a frame is one", ErrInvalid, id)
			}
			data = strings.TrimPrefix(line, "data: ")
		default:
			return Frame{}, fmt.Errorf("%w: stream line %q is no field this protocol writes",
				ErrInvalid, line)
		}
		fields++
	}
	if err := d.lines.Err(); err != nil {
		return Frame{}, err
	}
	if fields != 0 {
		return Frame{}, fmt.Errorf("%w: the stream ended inside event %s", ErrInvalid, id)
	}
	return Frame{}, io.EOF
}

func decodeEvent(id, event, data string) (Frame, error) {
	var f Frame
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Frame{}, fmt.Errorf("%w: event %s data is not one frame: %w", ErrInvalid, id, err)
	}
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	switch {
	case EventID(f.TurnID, f.Seq) != id:
		return Frame{}, fmt.Errorf("%w: event id %s and its frame's %s disagree", ErrInvalid,
			id, EventID(f.TurnID, f.Seq))
	case string(f.Kind) != event:
		return Frame{}, fmt.Errorf("%w: event %s is named %q and its frame is a %s", ErrInvalid,
			id, event, f.Kind)
	}
	return f, nil
}
