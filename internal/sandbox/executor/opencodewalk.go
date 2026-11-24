package executor

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// THE ONE OPENCODE EXPORT WALK ([scanExport]) reads an export on the scanner
// (internal/jsonscan), which holds no string. It owns the export's grammar,
// its names and its frames, and hands each member to its visitor
// ([exportVisitor]) by the means the visitor asks for it ([exportMeans]),
// fixed per visitor. A value past its word or its capture is there, of its
// kind, and past. The package doc's "What OpenCode counts a session at before
// its next request" has the walk and the meter, and "An OpenCode session's
// calls are read by what they were" the holdings reading.

// exportFrame is a container of an export the walk reads: the document, a
// list, or an object whose members a visitor asks for by name.
type exportFrame uint8

// The frames of an export as OpenCode 2 writes it.
const (
	frameDocument   exportFrame = iota // the export
	frameInfo                          // the document's `info`
	frameMessages                      // a `messages` list
	frameMessage                       // a message
	frameStepTime                      // a message's `time`
	frameContent                       // a message's `content` list
	frameItem                          // an item of a step's content
	framePartTime                      // an item's `time`
	frameState                         // a tool item's `state`
	frameAnswer                        // a state's `content` list
	frameAnswerPart                    // a part of an answer
	frameMetadata                      // a state's `metadata`
	frameFailure                       // a state's `error`
	frameFiles                         // a message's `files` list
	frameFile                          // a file of `files`
	exportFrames
)

// The names of the document, of `info`, of an answer's part, of a
// metadata, of a failure and of a file, each its index in its frame's set
// ([exportFrameNames]). A message's, an item's, a state's and their times'
// are the decode walk's ([msgID], [itemType], [stateStatus],
// [openCodeStepNames]).
const (
	docMessages = 0
	docInfo     = 1

	infoID = 0

	stepCompleted = 2

	partType = 0
	partMime = 1
	partText = 2

	metaTruncated = 0

	failureType    = 0
	failureMessage = 1

	fileType = 0
	fileMime = 1
)

// msgFiles is a message's `files`, which the meter reads: past the decode
// walk's set, so a message's key facts read it as a name outside that set, as
// the decode walk does.
const msgFiles = msgMetadata + 1

// exportMembersMax is the most names a frame's set holds: a message's.
const exportMembersMax = msgFiles + 1

// exportFrameNames is each object frame's set of names, by index; a list
// frame has none.
var exportFrameNames = [exportFrames]map[string]int{
	frameDocument: openCodeNames("messages", "info"),
	frameInfo:     openCodeNames("id"),
	frameMessage: func() map[string]int {
		names := maps.Clone(openCodeMessageNames)
		names["files"] = msgFiles
		return names
	}(),
	frameStepTime:   openCodeStepNames,
	frameItem:       openCodeItemNames,
	framePartTime:   openCodePartTimes,
	frameState:      openCodeStateNames,
	frameAnswerPart: openCodeNames("type", "mime", "text"),
	frameMetadata:   openCodeNames("truncated"),
	frameFailure:    openCodeNames("type", "message"),
	frameFile:       openCodeNames("type", "mime"),
}

// exportChild is the frame the value of f's member i is read as, where a
// visitor asks to walk it ([meansFrame]); [exportFrames] where it is none.
func exportChild(f exportFrame, i int) exportFrame {
	switch {
	case f == frameDocument && i == docInfo:
		return frameInfo
	case f == frameMessage && i == msgTime:
		return frameStepTime
	case f == frameMessage && i == msgContent:
		return frameContent
	case f == frameMessage && i == msgFiles:
		return frameFiles
	case f == frameItem && i == itemTime:
		return framePartTime
	case f == frameItem && i == itemState:
		return frameState
	case f == frameState && i == stateContent:
		return frameAnswer
	case f == frameState && i == stateMetadata:
		return frameMetadata
	case f == frameState && i == stateError:
		return frameFailure
	}
	return exportFrames
}

// exportElement is the frame each element of list frame f is read as, where
// it is an object.
func exportElement(f exportFrame) exportFrame {
	switch f {
	case frameMessages:
		return frameMessage
	case frameContent:
		return frameItem
	case frameAnswer:
		return frameAnswerPart
	case frameFiles:
		return frameFile
	}
	return exportFrames
}

// exportLists says, by frame, whether it is a list frame, read from an
// ArrayStart; an object frame is read from an ObjectStart.
var exportLists = func() (lists [exportFrames]bool) {
	for f := range exportFrames {
		lists[f] = exportElement(f) != exportFrames
	}
	return lists
}()

// exportMeans is how the walk reads one member of a frame for a visitor.
type exportMeans uint8

const (
	// meansPass passes the value over unheld and tells the visitor nothing:
	// that it is there is in its frame's key facts.
	meansPass exportMeans = iota
	// meansWord reads a string's decoded text into the walk's word buffer
	// ([walkWordBytes]), or says it ran past it; a value of another kind is
	// passed, its kind told.
	meansWord
	// meansCapture holds the value as written within [exportValueMaxBytes], or
	// says it ran past it, holding none of it.
	meansCapture
	// meansFrame walks the value as its frame ([exportChild]) where it is that
	// frame's kind, its members each by these means; else passes it, its kind
	// and a container's span told.
	meansFrame
	// meansDigest reads a string's decoded text into the visitor's digest
	// ([textDigest]), a piece at a time, holding none of it; a value of
	// another kind is passed, its kind told.
	meansDigest
	// meansStream writes a string's decoded text, a piece at a time, to the
	// writer the visitor names for it ([exportVisitor.sink]), and any other
	// value's bytes as written ([jsonscan.Scanner.Tee]), holding none of it;
	// where the visitor names none, the value is passed.
	meansStream
)

// exportAsks is a visitor's means for each member of each frame, by its index
// (a name no frame holds is passed), and whether it reads the document to its
// end, white space alone after it, or stops at its closing brace.
type exportAsks struct {
	means [exportFrames][exportMembersMax]exportMeans
	toEnd bool
}

// exportAsksOf is the asks of means, pass where it names none.
func exportAsksOf(means map[exportFrame]map[int]exportMeans, toEnd bool) *exportAsks {
	asks := exportAsks{toEnd: toEnd}
	for f, members := range means {
		for i, m := range members {
			asks.means[f][i] = m
		}
	}
	return &asks
}

// exportValue is what the walk read of one value, as the visitor asked.
type exportValue struct {
	// kind is the kind of the value's first token.
	kind jsonscan.Kind
	// span is a container's bytes as written, its opening and closing tokens
	// included; zero for any other value.
	span int64
	// text is a word's decoded text or a capture's bytes as written, valid
	// only until the visitor returns.
	text []byte
	// past says a word or a capture ran past its buffer or its bound: the
	// value is there, of its kind, and none of it is in text.
	past bool
	// keys are a frame's key facts over its set ([openCodeKeys]).
	keys openCodeKeys
}

// is reports whether v is a string word whose text is word.
func (v exportValue) is(word string) bool {
	return v.kind == jsonscan.String && !v.past && string(v.text) == word
}

// exportVisitor is a reader of an export on the walk ([scanExport]). An error
// it answers ends the walk, which answers it as it came.
type exportVisitor interface {
	// asks is the visitor's means for every member, asked once.
	asks() *exportAsks
	// open is a frame begun: a `messages` list at its name, before its value
	// is read; any other frame at its opening token.
	open(f exportFrame) error
	// member is member i of frame f read by its means, a frame's after the
	// frame closed.
	member(f exportFrame, i int, v exportValue) error
	// element is an element of list frame f that is no object, of kind k,
	// passed once it returns.
	element(f exportFrame, k jsonscan.Kind) error
	// close is a frame but a `messages` list ended: its span and key facts.
	close(f exportFrame, v exportValue) error
	// digest is where a string the visitor reads by its digest is read into,
	// nil for a visitor that reads none.
	digest() *textDigest
	// sink is where member i of frame f, a value of kind k the visitor reads
	// by its stream, is written, nil to pass it; a write's error is the
	// visitor's to keep, and never ends the walk.
	sink(f exportFrame, i int, k jsonscan.Kind) io.Writer
}

// walkNameBytes is the most of a name the walk reads: one past the longest
// name of a frame's set (an item's `providerResultState`), so a longer name is
// none of them, as the decode walk's sets read one.
const walkNameBytes = len("providerResultState") + 1

// walkWordBytes is the most of a word the walk reads: one past the longest a
// visitor compares (a message's kind `location-switched`), so a longer one is
// none of them.
const walkWordBytes = len(openCodeLocationSwitched) + 1

// exportWalk is one walk of an export: its scanner, its visitor and the
// buffers a name and a word are read into.
type exportWalk struct {
	ctx  context.Context
	sc   *jsonscan.Scanner
	v    exportVisitor
	asks *exportAsks
	name [walkNameBytes]byte
	word [walkWordBytes]byte
	// piece is what a streamed string is read in, made for the first.
	piece []byte
}

// scanExport walks plain, an OpenCode export document, once, handing v what
// it asks of each member ([exportVisitor]), and returns at the document's
// closing brace, or where v asks for it, once white space alone has followed
// it to plain's end. ctx is asked before each message and each item of a
// step's content. An error where plain is no export, does not read to its
// end, or v ended the walk.
func scanExport(ctx context.Context, plain io.Reader, v exportVisitor) error {
	w := &exportWalk{ctx: ctx, sc: jsonscan.New(plain), v: v, asks: v.asks()}
	if err := expectKind(w.sc, jsonscan.ObjectStart); err != nil {
		return err
	}
	for {
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return err
		case k == jsonscan.ObjectEnd && !w.asks.toEnd:
			return nil
		case k == jsonscan.ObjectEnd:
			// THE SCANNER READS ONE VALUE: anything but white space after it is
			// refused, and its end answers io.EOF.
			if _, err = w.sc.Next(); !errors.Is(err, io.EOF) {
				return cmp.Or(err, errors.New("another value follows the document"))
			}
			return nil
		}
		i, err := w.nameOf(frameDocument)
		if err != nil {
			return err
		}
		if i != docMessages {
			err = w.member(frameDocument, i)
		} else if err = v.open(frameMessages); err == nil {
			// EVERY LIST IS WALKED, a second one too: which a reader keeps is
			// the reader's, told at its name.
			if err = expectKind(w.sc, jsonscan.ArrayStart); err == nil {
				err = w.elements(frameMessages)
			}
		}
		if err != nil {
			return err
		}
	}
}

// member reads the value of frame f's member i, its name current, by the
// visitor's means, and hands it over; a name of no set (i below zero) is
// passed.
func (w *exportWalk) member(f exportFrame, i int) error {
	k, err := w.sc.Next()
	if err != nil {
		return err
	}
	means := meansPass
	if i >= 0 {
		means = w.asks.means[f][i]
	}
	v := exportValue{kind: k}
	switch means {
	case meansPass:
		return w.sc.Skip()
	case meansWord:
		if k != jsonscan.String {
			err = w.sc.Skip()
			break
		}
		v.text, v.past, err = w.read(w.word[:])
	case meansCapture:
		start := w.sc.Offset()
		var whole bool
		v.text, whole, err = w.sc.Capture(exportValueMaxBytes)
		v.past, v.span = !whole, w.spanFrom(start, k)
	case meansFrame:
		v, err = w.frame(exportChild(f, i), k)
	case meansDigest:
		if k != jsonscan.String {
			err = w.sc.Skip()
			break
		}
		err = w.v.digest().scan(w.sc)
	case meansStream:
		err = w.stream(w.v.sink(f, i, k), k)
	}
	if err != nil {
		return err
	}
	return w.v.member(f, i, v)
}

// frame reads a value whose first token, of kind k, is current, as frame f:
// walked where it is f's kind, passed otherwise.
func (w *exportWalk) frame(f exportFrame, k jsonscan.Kind) (exportValue, error) {
	start := w.sc.Offset()
	list := f < exportFrames && exportLists[f]
	if f == exportFrames || k != jsonscan.ArrayStart && list || k != jsonscan.ObjectStart && !list {
		err := w.sc.Skip()
		return exportValue{kind: k, span: w.spanFrom(start, k)}, err
	}
	if err := w.v.open(f); err != nil {
		return exportValue{}, err
	}
	var (
		keys openCodeKeys
		err  error
	)
	if list {
		err = w.elements(f)
	} else {
		keys, err = w.members(f)
	}
	if err != nil {
		return exportValue{}, err
	}
	// THE CONTAINER'S CLOSING TOKEN IS CURRENT: its span ends past it.
	v := exportValue{kind: k, span: w.sc.Offset() + 1 - start, keys: keys}
	return v, w.v.close(f, v)
}

// members reads object frame f's members, its opening brace current, to its
// closing one: each name, its key fact, and its value by its means.
func (w *exportWalk) members(f exportFrame) (openCodeKeys, error) {
	var keys openCodeKeys
	for {
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return keys, err
		case k == jsonscan.ObjectEnd:
			return keys, nil
		}
		i, err := w.nameOf(f)
		if err != nil {
			return keys, err
		}
		fact := i
		if f == frameMessage && i >= msgFiles {
			fact = -1
		}
		keys.note(fact)
		if err = w.member(f, i); err != nil {
			return keys, err
		}
	}
}

// elements reads list frame f's elements, its opening bracket current, to its
// closing one: an object as f's element frame, anything else told and passed.
// ctx is asked before each message and each item.
func (w *exportWalk) elements(f exportFrame) error {
	element := exportElement(f)
	for {
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return err
		case k == jsonscan.ArrayEnd:
			return nil
		}
		if element == frameMessage || element == frameItem {
			if err = w.ctx.Err(); err != nil {
				return err
			}
		}
		if k == jsonscan.ObjectStart {
			_, err = w.frame(element, k)
		} else if err = w.v.element(f, k); err == nil {
			err = w.sc.Skip()
		}
		if err != nil {
			return err
		}
	}
}

// nameOf is the current name's index in frame f's set, -1 where it is none of
// them or runs past [walkNameBytes].
func (w *exportWalk) nameOf(f exportFrame) (int, error) {
	text, past, err := w.read(w.name[:])
	if err != nil || past {
		return -1, err
	}
	if i, ok := exportFrameNames[f][string(text)]; ok {
		return i, nil
	}
	return -1, nil
}

// read is the current name's or string's decoded text where it fits in buf
// less a byte; else past, its rest passed over by the next Next.
func (w *exportWalk) read(buf []byte) ([]byte, bool, error) {
	n := 0
	for n < len(buf) {
		m, err := w.sc.Read(buf[n:])
		n += m
		switch {
		case errors.Is(err, io.EOF):
			return buf[:n], false, nil
		case errors.Is(err, io.ErrShortBuffer):
			return nil, true, nil
		case err != nil:
			return nil, false, err
		}
	}
	return nil, true, nil
}

// spanFrom is the span of a value that began at start with a token of kind k,
// now read: a container's to its current closing token, zero for any other.
func (w *exportWalk) spanFrom(start int64, k jsonscan.Kind) int64 {
	if k != jsonscan.ObjectStart && k != jsonscan.ArrayStart {
		return 0
	}
	return w.sc.Offset() + 1 - start
}

// stream writes the current value, whose first token is of kind k, to sink:
// a string's decoded text a piece at a time, and any other value's bytes as
// written; nil passes it. A writer's error ends no read: a tee's is answered
// past the value ([jsonscan.TeeError]), and a piece's is the sink's own.
func (w *exportWalk) stream(sink io.Writer, k jsonscan.Kind) error {
	switch {
	case sink == nil:
		return w.sc.Skip()
	case k != jsonscan.String:
		err := w.sc.Tee(sink)
		if err == nil {
			err = w.sc.Skip()
		}
		if _, tee := errors.AsType[*jsonscan.TeeError](err); tee {
			return nil
		}
		return err
	}
	if w.piece == nil {
		w.piece = make([]byte, textDigestBytes)
	}
	for {
		n, err := w.sc.Read(w.piece)
		_, _ = sink.Write(w.piece[:n])
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}
