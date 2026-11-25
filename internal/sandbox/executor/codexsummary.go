package executor

import "github.com/crewlet/crewlet/internal/jsonscan"

// codexCompactionUnshaped says a compaction of no shape this read knows.
const codexCompactionUnshaped = "a Codex compaction of a shape this read does not know"

// codexCompaction is what a walk of a rollout that holds no line reads of
// each line for the newest compaction's summary, as the transcript reads a
// `compacted` line ([codexReading.commit]): whether the envelope's decode
// refuses it (a fragment, no record), whether its payload is of a
// compaction's shape (an object or null), and its `message` as written. A
// line's `type` may follow its payload, so the message of a line of no known
// type is read, and the line's end picks it ([codexCompaction.into]). Its
// methods are a no-op on nil, a walk asked no summary.
type codexCompaction struct {
	fragment, payload bool
	message           summaryText
}

// start starts a line, which holds no payload until one is read.
func (c *codexCompaction) start() {
	if c == nil {
		return
	}
	c.fragment, c.payload = false, false
	c.message.reset()
}

// payloadStart starts the line's payload: one written again replaces it.
func (c *codexCompaction) payloadStart() {
	if c != nil {
		c.payload = true
		c.message.reset()
	}
}

// payloadTook says whether the payload's decode takes it: an object or null.
func (c *codexCompaction) payloadTook(took bool) {
	if c != nil {
		c.payload = c.payload && took
	}
}

// messageField reads the payload's `message`, k its kind, where typ, the
// line's type as read so far, is a compaction's or none yet; any other
// line's is passed over.
func (c *codexCompaction) messageField(w *lineWalk, k jsonscan.Kind, typ string) error {
	if c == nil || typ != "" && typ != "compacted" {
		return w.skip(k)
	}
	_, err := c.message.read(w, k)
	return err
}

// envelopeTook says whether the envelope's decode takes a member the walk
// read for itself: its `type`, a string or null.
func (c *codexCompaction) envelopeTook(took bool) {
	if c != nil {
		c.fragment = c.fragment || !took
	}
}

// envelopeField reads name, a member of the line's envelope a walk reads for
// nothing else, as the envelope's decode reads it: `timestamp` a string,
// `metadata` an object whose `compaction_output` is a flag. A member of
// another kind makes the line a fragment.
func (c *codexCompaction) envelopeField(w *lineWalk, name string, k jsonscan.Kind) error {
	var ok bool
	var err error
	switch {
	case c == nil:
		return w.skip(k)
	case name == "timestamp":
		ok, err = w.typedField(k, fieldString)
	case name == "metadata":
		ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
			if name != "compaction_output" {
				return w.skip(k)
			}
			_, took, flagErr := w.flagField(k)
			c.fragment = c.fragment || !took
			return flagErr
		})
	default:
		return w.skip(k)
	}
	c.fragment = c.fragment || !ok
	return err
}

// into makes the line, read whole, of type typ, the newest compaction where
// it is one: a fragment (fragment, or c's own) is no record, and a
// compaction whose payload or message is of no shape this read knows is one
// that cannot be said.
func (c *codexCompaction) into(s *summaryRead, typ string, fragment bool) {
	switch {
	case fragment || c.fragment || typ != "compacted":
	case !c.payload || c.message.odd:
		s.unshaped(codexCompactionUnshaped)
	default:
		s.taken(&c.message)
	}
}
