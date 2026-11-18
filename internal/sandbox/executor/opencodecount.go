package executor

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT OPENCODE COUNTS A SESSION AT BEFORE ITS NEXT REQUEST, read off the
// session's export ([openCodeCountReading]) in ONE WALK on the scanner, which
// holds no string: the last counted reply's input, cache, output and
// reasoning, and every message after it, the reply's tool results among them,
// at a quarter of its bytes rounded up, never under OpenCode's own estimate;
// restarted at the newest completed compaction, its restart the newest
// compaction of any status, the reply's provider beside it. The package doc
// ("What OpenCode counts a session at before its next request") says why the
// charge bounds OpenCode's, and where each rule was read.

// exportReading is what one walk of an OpenCode export read
// ([meterExport]).
type exportReading struct {
	// figure is the input the session's last request carried, by the one rule
	// for an OpenCode session's measure ([measuredPrompt]); zero where the
	// export did not read.
	figure int64
	// count is what OpenCode counts the session at before its next request:
	// a bound, or a mark saying why there is none.
	count *sandbox.ResumedCount
	// provider is the provider of the reply the count is anchored on, "" where
	// the count names none.
	provider string
	// next is the estimate of the session's next request: the count's bound,
	// at OpenCode's one rate, zero where the count is marked.
	next sandbox.NextRequest
}

// meterExport reads r, an OpenCode export gzipped, once, within ctx: the
// floor's figure and OpenCode's count of the session ([openCodeCountReading]),
// the count read by the rules of release, the box's word on the release that
// wrote it ([openCodeCountRead]). An export that does not read to its end has
// no figure and an unread count; err is what ended the read.
func meterExport(ctx context.Context, r io.Reader, release string) (exportReading, error) {
	var (
		count   openCodeCountReading
		callers countMarks
	)
	if !openCodeCountRead(release) {
		callers = callers.with(sandbox.CountRelease)
	}
	figure, err := walkExport(ctx, r, &count)
	if err != nil {
		return exportReading{count: count.count(callers.with(sandbox.CountUnread))}, err
	}
	out := exportReading{figure: figure, count: count.count(callers)}
	if bound, ok := out.count.Bound(); ok {
		out.next = sandbox.NextRequest{Tokens: bound}
	}
	if out.count.Mark == "" || out.count.Restart != nil {
		out.provider = count.provider
	}
	return out, nil
}

// openCodeRestart reports whether a message of kind is one where an OpenCode
// session restarts: a compaction, whatever its status. THE ONE PREDICATE every
// reader of an OpenCode session's restarts calls, so the count's identity
// ([openCodeCountReading]) and a reading of what a session holds never part on
// where it restarts.
func openCodeRestart(kind openCodeKind) bool { return kind == openCodeCompaction }

// openCodeCountReading is OpenCode's count of a session, read forward over its
// export's messages in order (the file comment). It keeps no string of the
// export but the newest compaction's id and the anchor's provider, each held
// to its bound.
type openCodeCountReading struct {
	// anchored says a counted reply was read since the session's start or its
	// newest completed compaction; figure is that reply's counts, tail the
	// charge of everything after it, and marks the files after it.
	anchored     bool
	figure, tail int64
	marks        countMarks
	// provider is the anchor's provider.
	provider string
	// restart is the newest compaction's id, "" where there is none.
	restart string
	// unread says the message list was given twice, so which one a reader
	// keeps is the reader's.
	unread bool
	// unsure says a value the count from its anchor on reads did not read: a
	// step's tokens, the anchor's provider, a compaction's status, or a tool's
	// answer of no shape OpenCode writes, at the anchor or after it. A later
	// anchor, or a completed compaction, starts the count again and clears it.
	unsure bool
	// unnamed says the newest compaction's id did not read.
	unnamed bool
}

// openCodeCountTokens is what one reply's request measured, as the count reads
// it: the prompt's three parts, and the output and reasoning OpenCode counts
// after them.
type openCodeCountTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// exportMessage is one message of an export as the walk reads it: its small
// values raw, its span, and what its content and files hold.
type exportMessage struct {
	// wire is what the floor's figure reads ([measuredPrompt]), its id left
	// out: the figure never reads one, so an id that does not read costs it
	// nothing.
	wire              openCodeMeasuredWire
	id, status, model json.RawMessage
	// span is the message's bytes as the export holds it, from its opening
	// brace to its closing one.
	span int64
	// results is the charge of its tool calls' answers and failures, a
	// quarter of each one's span rounded up.
	results int64
	// files are the marks of the files it names: a person's, or a tool's
	// answer's.
	files countMarks
	// odd says a value the count reads is of no shape OpenCode writes.
	odd bool
}

// message is one message read, in order.
func (r *openCodeCountReading) message(m exportMessage, kind openCodeKind) {
	switch {
	case openCodeRestart(kind):
		id, ok := exportString(m.id)
		r.restart, r.unnamed = id, !ok || id == "" || len(id) > sandbox.RestartIDMaxBytes
		status, ok := exportString(m.status)
		switch {
		case !ok:
			// WHETHER THE COUNT RESTARTED HERE is not known.
			r.unsure = true
		case status == openCodeCompleted:
			// THE COUNT RESTARTS: OpenCode counts from the compaction on, and a
			// reply after it anchors the count again.
			r.anchored, r.figure, r.tail, r.marks, r.provider, r.unsure = false, 0, 0, 0, "", false
			return
		}
		r.after(m)
	case kind == openCodeAssistant && absent(m.wire.Error) && !absent(m.wire.Tokens):
		var t openCodeCountTokens
		if err := wireDecode(m.wire.Tokens, &t); err != nil {
			// A STEP THAT MAY BE THE ANCHOR, unread.
			r.unsure = true
			return
		}
		prompt := max(t.Input, 0) + max(t.Cache.Read, 0) + max(t.Cache.Write, 0)
		if prompt <= 0 {
			r.after(m)
			return
		}
		var model struct {
			ProviderID json.RawMessage `json:"providerID"`
		}
		provider, ok := "", false
		if wireDecode(m.model, &model) == nil {
			provider, ok = exportString(model.ProviderID)
		}
		named := ok && provider != "" && sandbox.ValidCountProvider(provider)
		if !named {
			provider = ""
		}
		// THE ANCHOR: its counts, and its own tool results after it, which
		// OpenCode counts as the messages that follow its reply.
		r.anchored, r.figure, r.provider = true, prompt+max(t.Output, 0)+max(t.Reasoning, 0), provider
		r.tail, r.marks, r.unsure = m.results, m.files, !named || m.odd
	default:
		r.after(m)
	}
}

// after charges a message that is no anchor to the tail, where a reply
// anchors the count: its whole span, and its files' marks.
func (r *openCodeCountReading) after(m exportMessage) {
	if !r.anchored {
		return
	}
	r.tail += quarter(m.span)
	r.marks |= m.files
	r.unsure = r.unsure || m.odd
}

// count is the bound the reading holds, or the first mark of callers' and its
// own ([sandbox.CountMarks]' order); and, where the export was read whole by
// the rules of its release, the newest compaction's id.
func (r *openCodeCountReading) count(callers countMarks) *sandbox.ResumedCount {
	marks := callers
	if r.unread || r.unsure || r.unnamed {
		marks = marks.with(sandbox.CountUnread)
	}
	if r.anchored {
		marks |= r.marks
	} else {
		marks = marks.with(sandbox.CountUnanchored)
	}
	out := &sandbox.ResumedCount{Tokens: r.figure + r.tail}
	// THE IDENTITY IS BLIND ONLY WHERE THE EXPORT WAS NOT READ WHOLE OR NOT BY
	// THE RULES OF ITS RELEASE, as a Claude Code count's is: every other mark
	// says what the bytes cannot bound, never where the session restarts.
	if blind := countMarks(0).with(sandbox.CountUnread).with(sandbox.CountRelease); marks&blind == 0 {
		out.Restart = new(r.restart)
	}
	if mark := marks.first(); mark != "" {
		out.Tokens, out.Mark = 0, mark
	}
	return out
}

// quarter is n bytes at four a token, rounded up.
func quarter(n int64) int64 { return (max(n, 0) + 3) / 4 }

// exportString is raw read as a string by its exact keys, and whether it read
// as one: absent, null or another kind is none.
func exportString(raw json.RawMessage) (string, bool) {
	if absent(raw) {
		return "", false
	}
	var s string
	if wireDecode(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// exportNameBytes is the most of a name the walk reads: one byte past the
// longest it looks for (`messages`), so a longer name reads as none of them,
// and at least a rune's bytes, the least the scanner reads a text into.
const exportNameBytes = len("messages") + 1

// exportWordBytes is the most of a content item's kind or a file's media type
// the walk reads: one past the longest it compares (`application/pdf`), so a
// longer one is none of them.
const exportWordBytes = len("application/pdf") + 1

// exportValueMaxBytes is the most of one of a message's small values the walk
// holds: the scanner's own read buffer, 64 KiB, so a value within it is
// captured from the buffer it is already in. A message's kind, status and id
// are words, its time, tokens and model a few values each, under a few
// hundred bytes; and its error, which the measure reads only for whether it is
// there, is never read past it.
const exportValueMaxBytes = 64 << 10

// errorPastBound stands for a message's error past [exportValueMaxBytes]: no
// null, so the step ended on an error, which is all a reader reads of one.
var errorPastBound = json.RawMessage("true")

// walkExport reads r, an OpenCode export gzipped, once on the scanner, within
// ctx, asked before each message: each message to count in order, and the
// floor's figure, its last message that measured a prompt ([measuredPrompt]).
// An error where the export is no export, or does not read to its end.
func walkExport(ctx context.Context, r io.Reader, count *openCodeCountReading) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	sc := jsonscan.New(zr)
	if err = expectKind(sc, jsonscan.ObjectStart); err != nil {
		return 0, err
	}
	var (
		last  int64
		lists int
		name  [exportNameBytes]byte
	)
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return 0, err
		case k == jsonscan.ObjectEnd:
			return last, nil
		}
		key, err := shortName(sc, name[:])
		if err != nil {
			return 0, err
		}
		if key != "messages" {
			if err = skipNext(sc); err != nil {
				return 0, err
			}
			continue
		}
		// A LIST GIVEN TWICE: the figure reads both, as it always has, and the
		// count reads neither, since which a reader keeps is the reader's.
		if lists++; lists > 1 {
			count.unread = true
		}
		if err = expectKind(sc, jsonscan.ArrayStart); err != nil {
			return 0, err
		}
		for {
			k, err := sc.Next()
			if err != nil {
				return 0, err
			}
			if k == jsonscan.ArrayEnd {
				break
			}
			if err = ctx.Err(); err != nil {
				return 0, err
			}
			m, err := exportMessageAt(sc, k)
			if err != nil {
				return 0, err
			}
			msg, _, err := m.wire.message()
			if err != nil {
				return 0, err
			}
			if n, measured := measuredPrompt(msg); measured {
				last = n
			}
			count.message(m, msg.Type)
		}
	}
}

// exportMessageAt reads one message of an export, its first token of kind
// first: its small values by their exact names, captured at
// [exportValueMaxBytes] and decoded by [openCodeMeasuredWire]'s one rule; its
// content and its files walked; everything else passed over.
//
// HELD TO [exportValueMaxBytes]: an error past it is one (it is no null), an
// id, a status or a model past it does not read, and a kind, a time or tokens
// past it is no message OpenCode writes, which the walk reads as an export it
// cannot read, rather than hold whatever a box wrote there.
func exportMessageAt(sc *jsonscan.Scanner, first jsonscan.Kind) (exportMessage, error) {
	var m exportMessage
	if first != jsonscan.ObjectStart {
		return m, fmt.Errorf("%w: it holds %s where a message belongs", errNotExport, first)
	}
	start := sc.Offset()
	var name [exportNameBytes]byte
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return m, err
		case k == jsonscan.ObjectEnd:
			m.span = sc.Offset() + 1 - start
			return m, nil
		}
		key, err := shortName(sc, name[:])
		if err != nil {
			return m, err
		}
		var to *json.RawMessage
		switch key {
		case "id":
			to = &m.id
		case "type":
			to = &m.wire.Type
		case "time":
			to = &m.wire.Time
		case "tokens":
			to = &m.wire.Tokens
		case "error":
			to = &m.wire.Error
		case "status":
			to = &m.status
		case "model":
			to = &m.model
		case "content":
			if err = m.content(sc); err != nil {
				return m, err
			}
			continue
		case "files":
			if err = m.filesOf(sc); err != nil {
				return m, err
			}
			continue
		default:
			if err = skipNext(sc); err != nil {
				return m, err
			}
			continue
		}
		if _, err = sc.Next(); err != nil {
			return m, err
		}
		raw, whole, err := sc.Capture(exportValueMaxBytes)
		switch {
		case err != nil:
			return m, err
		case whole:
			*to = append(json.RawMessage(nil), raw...)
		case key == "error":
			*to = errorPastBound
		case key == "id" || key == "status" || key == "model":
			// No string, so it reads as none, and the count that needs it
			// is unread.
			*to = errorPastBound
		default:
			return m, fmt.Errorf("%w: a message's %s runs past %d bytes", errNotExport, key,
				exportValueMaxBytes)
		}
	}
}

// content walks a message's content, the list of an assistant step's items: of
// each tool call, the spans of its answer and its failure, and the files its
// answer holds. Content of another shape is odd (null is none).
func (m *exportMessage) content(sc *jsonscan.Scanner) error {
	k, err := sc.Next()
	if err != nil {
		return err
	}
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ArrayStart:
	default:
		m.odd = true
		return sc.Skip()
	}
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return err
		case k == jsonscan.ArrayEnd:
			return nil
		case k != jsonscan.ObjectStart:
			if err = sc.Skip(); err != nil {
				return err
			}
			continue
		}
		var (
			kind    string
			results int64
			files   countMarks
			odd     bool
			name    [exportNameBytes]byte
			word    [exportWordBytes]byte
		)
		for {
			k, err := sc.Next()
			if err != nil {
				return err
			}
			if k == jsonscan.ObjectEnd {
				break
			}
			key, err := shortName(sc, name[:])
			if err != nil {
				return err
			}
			switch key {
			case "type":
				if kind, err = shortString(sc, word[:]); err != nil {
					return err
				}
			case "state":
				if results, files, odd, err = toolState(sc); err != nil {
					return err
				}
			default:
				if err = skipNext(sc); err != nil {
					return err
				}
			}
		}
		if kind == "tool" {
			m.results += results
			m.files |= files
			m.odd = m.odd || odd
		}
	}
}

// toolState walks a tool call's state: the charge of its answer (`content`)
// and its failure (`error`), each a quarter of its span rounded up, and the
// files its answer holds. An answer or a failure that is neither a container
// nor null is odd: OpenCode writes a list and an object, and the walk reads
// no span of a scalar.
func toolState(sc *jsonscan.Scanner) (int64, countMarks, bool, error) {
	k, err := sc.Next()
	if err != nil {
		return 0, 0, false, err
	}
	if k != jsonscan.ObjectStart {
		return 0, 0, k != jsonscan.Null, sc.Skip()
	}
	var (
		charge int64
		files  countMarks
		odd    bool
		name   [exportNameBytes]byte
	)
	for {
		k, err := sc.Next()
		if err != nil {
			return 0, 0, false, err
		}
		if k == jsonscan.ObjectEnd {
			return charge, files, odd, nil
		}
		key, err := shortName(sc, name[:])
		if err != nil {
			return 0, 0, false, err
		}
		if key != "content" && key != "error" {
			if err = skipNext(sc); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		k, err = sc.Next()
		if err != nil {
			return 0, 0, false, err
		}
		start := sc.Offset()
		switch {
		case k == jsonscan.Null:
			continue
		case key == "content" && k == jsonscan.ArrayStart:
			marks, itemsErr := fileItems(sc, false)
			if itemsErr != nil {
				return 0, 0, false, itemsErr
			}
			files |= marks
		case k == jsonscan.ArrayStart || k == jsonscan.ObjectStart:
			if err = sc.Skip(); err != nil {
				return 0, 0, false, err
			}
		default:
			odd = true
			if err = sc.Skip(); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		// THE CONTAINER'S CLOSING TOKEN IS CURRENT: its span ends past it.
		charge += quarter(sc.Offset() + 1 - start)
	}
}

// filesOf walks a person's message's files: every file is a mark, a PDF's
// [sandbox.CountPDF] and any other's [sandbox.CountMedia]; files that are no
// list are read as an unknown file (null is none).
func (m *exportMessage) filesOf(sc *jsonscan.Scanner) error {
	k, err := sc.Next()
	if err != nil {
		return err
	}
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ArrayStart:
		marks, err := fileItems(sc, true)
		m.files |= marks
		return err
	}
	m.files = m.files.with(sandbox.CountMedia)
	return sc.Skip()
}

// fileItems walks a list whose ArrayStart is current, to its ArrayEnd: the
// marks of the files it holds. In a tool's answer (all false) an item whose
// `type` is `text` is text and no file; every other item, and in a person's
// files (all true) every item, is a file, a PDF where its `mime` says so.
func fileItems(sc *jsonscan.Scanner, all bool) (countMarks, error) {
	var marks countMarks
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return 0, err
		case k == jsonscan.ArrayEnd:
			return marks, nil
		case k != jsonscan.ObjectStart:
			marks = marks.with(sandbox.CountMedia)
			if err = sc.Skip(); err != nil {
				return 0, err
			}
			continue
		}
		var (
			kind, mime string
			name       [exportNameBytes]byte
			word       [exportWordBytes]byte
		)
		for {
			k, err := sc.Next()
			if err != nil {
				return 0, err
			}
			if k == jsonscan.ObjectEnd {
				break
			}
			key, err := shortName(sc, name[:])
			if err != nil {
				return 0, err
			}
			switch key {
			case "type":
				kind, err = shortString(sc, word[:])
			case "mime":
				mime, err = shortString(sc, word[:])
			default:
				err = skipNext(sc)
			}
			if err != nil {
				return 0, err
			}
		}
		switch {
		case !all && kind == "text":
		case mime == "application/pdf":
			marks = marks.with(sandbox.CountPDF)
		default:
			marks = marks.with(sandbox.CountMedia)
		}
	}
}

// shortName is the current name's or string's decoded text where it fits in
// buf less a byte, and "" where it is longer, its rest passed over by the next
// Next.
func shortName(sc *jsonscan.Scanner, buf []byte) (string, error) {
	read := 0
	for read < len(buf) {
		n, err := sc.Read(buf[read:])
		read += n
		if errors.Is(err, io.EOF) {
			return string(buf[:read]), nil
		}
		if errors.Is(err, io.ErrShortBuffer) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

// shortString reads the value after a name: its text where it is a string that
// fits in buf less a byte, and "" for a longer one or a value of another kind,
// passed over.
func shortString(sc *jsonscan.Scanner, buf []byte) (string, error) {
	k, err := sc.Next()
	if err != nil {
		return "", err
	}
	if k != jsonscan.String {
		return "", sc.Skip()
	}
	return shortName(sc, buf)
}

// expectKind reads the next token, an error where it is not of kind want.
func expectKind(sc *jsonscan.Scanner, want jsonscan.Kind) error {
	k, err := sc.Next()
	if err != nil {
		return err
	}
	if k != want {
		return fmt.Errorf("%w: it holds %s where %s belongs", errNotExport, k, want)
	}
	return nil
}

// skipNext passes over the value after a name.
func skipNext(sc *jsonscan.Scanner) error {
	if _, err := sc.Next(); err != nil {
		return err
	}
	return sc.Skip()
}
